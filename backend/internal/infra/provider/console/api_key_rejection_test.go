package console

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const invalidAPIKeyTestBody = `{"code":"invalid-argument","error":"Incorrect API key provided. You can obtain an API key from https://console.x.ai."}`

func TestConsoleVideoRepairsBadRequestAPIKeyOnce(t *testing.T) {
	for _, tc := range []struct {
		name                string
		freshKey            string
		homeStatus          int
		body                string
		wantCalls, wantHome int32
		wantCreate          int32
		wantSuccess         bool
	}{
		{"another key succeeds", testNewKeyID, 200, invalidAPIKeyTestBody, 2, 1, 0, true},
		{"rejected key cannot be replaced", testConsoleAPIKeyID, 200, invalidAPIKeyTestBody, 1, 1, 1, false},
		{"key lookup fails", "", 503, invalidAPIKeyTestBody, 1, 1, 0, false},
		{"ordinary parameter error", "", 200, `{"code":"invalid-argument","error":"duration is invalid"}`, 1, 0, 0, false},
		{"moderation error", "", 200, `{"error":"Generated video rejected by content moderation."}`, 1, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates, homes, keysCreated atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/dpop/token":
					serveTestDPoPToken(t, w, r)
				case "/":
					homes.Add(1)
					w.WriteHeader(tc.homeStatus)
					_, _ = io.WriteString(w, consoleHomeHTML(testTeamID, consoleHomeKey(testTeamID, tc.freshKey, false)))
				case "/v1/videos/generations":
					creates.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if tc.wantSuccess && r.Header.Get(consoleAPIKeyHeader) == testNewKeyID {
						_, _ = io.WriteString(w, `{"request_id":"video_key_repaired"}`)
					} else {
						w.WriteHeader(400)
						_, _ = io.WriteString(w, tc.body)
					}
				case "/v1/videos/video_key_repaired":
					_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://vidgen.x.ai/output.mp4","duration":6}}`)
				case consoleCreateAPIKeyPath:
					keysCreated.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			adapter, credential := newConsoleTestAdapter(t, server.URL)
			store := newRecordingCacheStore()
			adapter.SetCredentialCacheStore(store)
			checkpoint := ""
			result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
				Credential: credential, Model: "grok-imagine-video-1.5", Prompt: "a landscape", Duration: 6,
				SaveCheckpoint: func(value string) error { checkpoint = value; return nil },
			})
			if creates.Load() != tc.wantCalls || homes.Load() != tc.wantHome || keysCreated.Load() != tc.wantCreate {
				t.Fatalf("creates=%d home=%d keysCreated=%d err=%v", creates.Load(), homes.Load(), keysCreated.Load(), err)
			}
			if tc.wantSuccess {
				if err != nil || result.URL == "" || store.apiKey(credential.ID) != testNewKeyID {
					t.Fatalf("result=%+v stored=%s err=%v", result, store.apiKey(credential.ID), err)
				}
			} else {
				stage, ok := provider.VideoErrorStage(err)
				if err == nil || !ok || stage != provider.VideoStageCreate || checkpoint != "" || errors.Is(err, provider.ErrUnauthorized) {
					t.Fatalf("stage=%s checkpoint=%q err=%v", stage, checkpoint, err)
				}
				var diagnostic provider.UpstreamDiagnosticError
				if !errors.As(err, &diagnostic) {
					t.Fatal("missing raw diagnostic")
				}
				_, response := diagnostic.UpstreamDiagnostic()
				if response.StatusCode != 400 || !strings.Contains(string(response.Body), tc.body) {
					t.Fatalf("diagnostic=%+v", response)
				}
			}
		})
	}
}

func TestAPIKeyInspectionPreservesLargeParameterError(t *testing.T) {
	body := `{"code":"invalid-argument","error":"` + strings.Repeat("parameter detail ", provider.MaxDiagnosticBodyBytes/8) + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dpop/token":
			serveTestDPoPToken(t, w, r)
		case "/v1/images/generations":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, body)
		default:
			t.Errorf("ordinary parameter error attempted key repair: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	lease, err := adapter.egress.AcquireCredential(t.Context(), egressdomain.ScopeConsole, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	response, err := adapter.doDPoPRequest(t.Context(), credential, "test-sso", lease, http.MethodPost, server.URL+"/v1/images/generations", []byte(`{}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusBadRequest || string(got) != body || response.Header.Get("X-Grok2API-Body-Truncated") != "" {
		t.Fatalf("parameter response changed: status=%d bytes=%d err=%v", response.StatusCode, len(got), err)
	}
}
