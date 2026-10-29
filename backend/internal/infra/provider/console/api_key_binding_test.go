package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func TestConsoleAPIKeyAndDPoPStayBoundToAccountDuringRotation(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		name := "derived cache"
		if persisted {
			name = "persisted cache"
		}
		t.Run(name, func(t *testing.T) {
			keys := map[string]string{"sso-a": testConsoleAPIKeyID, "sso-b": testNewKeyID}
			var mu sync.Mutex
			tokenCalls, homeCalls := map[string]int{}, map[string]int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cookie, err := r.Cookie("sso")
				if err != nil || keys[cookie.Value] == "" {
					t.Errorf("missing test account cookie")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				sso := cookie.Value
				switch r.URL.Path {
				case "/":
					mu.Lock()
					homeCalls[sso]++
					mu.Unlock()
					_, _ = io.WriteString(w, consoleHomeHTML(testTeamID, consoleHomeKey(testTeamID, keys[sso], false)))
				case "/v1/dpop/token":
					mu.Lock()
					tokenCalls[sso]++
					mu.Unlock()
					var payload struct{ JWK dpopJWK }
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					thumbprint, err := dpopJWKThumbprint(payload.JWK)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					claims, _ := json.Marshal(map[string]any{"sub": sso, "exp": time.Now().Add(5 * time.Minute).Unix(), "cnf": map[string]string{"jkt": thumbprint}})
					token := "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".dGVzdA"
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "DPoP", "expires_in": 300})
				case "/v1/responses":
					if r.Header.Get(consoleAPIKeyHeader) != keys[sso] {
						t.Errorf("account %s received another account's x-apikey", sso)
					}
					parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "DPoP "), ".")
					if len(parts) != 3 {
						t.Errorf("missing DPoP token")
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
					var payload struct{ Sub string }
					if json.Unmarshal(claims, &payload) != nil || payload.Sub != sso {
						t.Errorf("account %s received another account's DPoP token", sso)
					}
					verifyTestDPoPProof(t, r)
					_, _ = io.WriteString(w, `{"id":"bound","object":"response","status":"completed","output":[]}`)
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			adapter, first := newConsoleTestAdapter(t, server.URL)
			store := newRecordingCacheStore()
			adapter.SetCredentialCacheStore(store)
			credentials := make([]account.Credential, 0, 2)
			for i, sso := range []string{"sso-a", "sso-b"} {
				credential := first
				credential.ID = uint64(i + 1)
				encrypted, err := adapter.cipher.Encrypt(sso)
				if err != nil {
					t.Fatal(err)
				}
				credential.EncryptedAccessToken = encrypted
				credential.ConsoleAPIKeyID = ""
				if persisted {
					credential.ConsoleAPIKeyID = keys[sso]
				}
				credentials = append(credentials, credential)
			}
			request := func(credential account.Credential) {
				response, err := adapter.ForwardResponse(t.Context(), provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3", Operation: conversation.OperationResponses, Body: []byte(`{"model":"grok-4.3","input":"test"}`), NormalizeBody: true})
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Errorf("status=%d", response.StatusCode)
				}
			}
			for _, index := range []int{0, 1, 0} {
				request(credentials[index])
			}
			var workers sync.WaitGroup
			for i := range 20 {
				workers.Go(func() { request(credentials[i%2]) })
			}
			workers.Wait()
			for i, sso := range []string{"sso-a", "sso-b"} {
				wantHomes := 1
				if persisted {
					wantHomes = 0
				}
				if tokenCalls[sso] != 1 || homeCalls[sso] != wantHomes {
					t.Fatalf("%s: token calls=%d home calls=%d", sso, tokenCalls[sso], homeCalls[sso])
				}
				if !persisted && store.apiKey(credentials[i].ID) != keys[sso] {
					t.Fatal("derived key persisted against another account")
				}
			}
		})
	}
}

func TestParseConsoleHomeKeyObjectKeepsTeamAndDisabledFieldsTogether(t *testing.T) {
	// disabled appears before the next key id; it must not disable the current key.
	raw := `{"name":"quoted \"key\" {label}","teamId":"` + testTeamID + `","apiKeyId":"` + testNewKeyID + `","disabled":false},` +
		`{"disabled":true,"teamId":"` + testTeamID + `","apiKeyId":"` + testConsoleAPIKeyID + `"}`
	for depth := range 3 {
		text := raw
		for range depth {
			text = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(text)
		}
		home := parseConsoleHome([]byte(text), testTeamID)
		if len(home.apiKeyIDs) != 1 || home.availableAPIKey("") != testNewKeyID {
			t.Fatalf("escape depth=%d keys=%v", depth, home.apiKeyIDs)
		}
	}
	mixed := consoleHomeKey("bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee", testConsoleAPIKeyID, false) + "," + raw
	if home := parseConsoleHome([]byte(mixed), testTeamID); len(home.apiKeyIDs) != 1 || home.availableAPIKey("") != testNewKeyID {
		t.Fatalf("mixed SSR fragments selected the wrong team: %v", home.apiKeyIDs)
	}
}

func TestConsoleRejectedAPIKeyCanBeRecreatedOnce(t *testing.T) {
	for _, tc := range []struct {
		name           string
		newKeyRejected bool
		workers        int
	}{{"new key succeeds", false, 1}, {"new key also rejected", true, 1}, {"parallel repair creates one key", false, 12}} {
		t.Run(tc.name, func(t *testing.T) {
			var provisioned atomic.Bool
			var creates, homes, oldRequests, newRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/dpop/token":
					serveTestDPoPToken(t, w, r)
				case "/":
					homes.Add(1)
					keys := []string{consoleHomeKey(testTeamID, testConsoleAPIKeyID, false)}
					if provisioned.Load() {
						keys = append(keys, ",", consoleHomeKey(testTeamID, testNewKeyID, false))
					}
					_, _ = io.WriteString(w, consoleHomeHTML(testTeamID, keys...))
				case consoleCreateAPIKeyPath:
					creates.Add(1)
					verifyTestDPoPProof(t, r)
					body, _ := io.ReadAll(r.Body)
					if r.Header.Get(consoleAPIKeyHeader) != "" || string(body) != string(consoleCreateAPIKeyBody(testTeamID)) {
						t.Error("key provisioning was not bound to the current team")
					}
					provisioned.Store(true)
					_, _ = w.Write([]byte{0, 0, 0, 0, 0})
				case "/v1/videos/generations":
					key := r.Header.Get(consoleAPIKeyHeader)
					if key == testConsoleAPIKeyID {
						oldRequests.Add(1)
					} else if key == testNewKeyID && provisioned.Load() {
						newRequests.Add(1)
					} else {
						t.Error("inference used an unknown key")
					}
					if key != testNewKeyID || tc.newKeyRejected {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, invalidAPIKeyTestBody)
						return
					}
					_, _ = io.WriteString(w, `{"request_id":"recreated"}`)
				case "/v1/videos/recreated":
					_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://vidgen.x.ai/created.mp4","duration":6}}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			adapter, credential := newConsoleTestAdapter(t, server.URL)
			store := newRecordingCacheStore()
			adapter.SetCredentialCacheStore(store)
			run := func() {
				checkpoint := ""
				_, err := adapter.GenerateVideo(t.Context(), provider.VideoRequest{Credential: credential, Model: "grok-imagine-video-1.5", Prompt: "test landscape", Duration: 6, SaveCheckpoint: func(value string) error { checkpoint = value; return nil }})
				if tc.newKeyRejected {
					stage, _ := provider.VideoErrorStage(err)
					if err == nil || stage != provider.VideoStageCreate || checkpoint != "" {
						t.Errorf("stage=%s checkpoint=%s err=%v", stage, checkpoint, err)
					}
				} else if err != nil {
					t.Error(err)
				}
			}
			var workers sync.WaitGroup
			for range tc.workers {
				workers.Go(run)
			}
			workers.Wait()
			if creates.Load() != 1 || homes.Load() < 2 || oldRequests.Load() < 1 || oldRequests.Load() > int32(tc.workers) || newRequests.Load() != int32(tc.workers) || store.apiKey(credential.ID) != testNewKeyID {
				t.Fatalf("key creates=%d home=%d rejected requests=%d repaired requests=%d", creates.Load(), homes.Load(), oldRequests.Load(), newRequests.Load())
			}
		})
	}
}

func TestConsoleAPIKeyMustMatchRedirectedTeam(t *testing.T) {
	const otherTeam = "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/team/"+testTeamID+"/chat", http.StatusTemporaryRedirect)
			return
		}
		// A different team's key is serialized before the selected team's key.
		_, _ = io.WriteString(w, consoleHomeHTML(testTeamID, consoleHomeKey(otherTeam, testConsoleAPIKeyID, false), ",", consoleHomeKey(testTeamID, testNewKeyID, false)))
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	lease, err := adapter.egress.AcquireCredential(context.Background(), egressdomain.ScopeConsole, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	home, err := adapter.readConsoleHome(t.Context(), "test-sso", lease)
	if err != nil || home.teamID != testTeamID || home.availableAPIKey("") != testNewKeyID {
		t.Fatalf("selected team's key was not used: team=%q key=%q err=%v", home.teamID, home.availableAPIKey(""), err)
	}
}
