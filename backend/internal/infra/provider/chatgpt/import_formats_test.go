package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// A sub2api export lists every platform; only its OpenAI OAuth accounts are
// imported, with their refresh token bound to the client that issued it.
func TestImportSub2APIExportKeepsOpenAIOAuthAccounts(t *testing.T) {
	a := &Adapter{}
	seeds, err := a.ParseImportedCredentials([]byte(`{"type":"sub2api-data","version":1,"proxies":[],"accounts":[
		{"name":"claude","platform":"anthropic","type":"oauth","credentials":{"access_token":"sk-ant-oat","refresh_token":"sk-ant-ort"}},
		{"name":"key","platform":"openai","type":"apikey","credentials":{"api_key":"sk-key"}},
		{"name":"empty","platform":"openai","type":"oauth","credentials":null},
		{"name":"gpt","platform":"openai","type":"oauth","credentials":{"access_token":"AT","refresh_token":"RT","client_id":"app_EMoamEEZ73f0CkXaXp7hrann"}}]}`))
	if err != nil || len(seeds) != 1 {
		t.Fatalf("seeds = %+v, err = %v", seeds, err)
	}
	if seeds[0].Name != "gpt" || seeds[0].AccessToken != "AT" || seeds[0].RefreshToken != "RT" || seeds[0].State["client_profile"] != codexProfile {
		t.Fatalf("seed = %+v", seeds[0])
	}
	// A file without ChatGPT logins is skipped, not an error that stops the other files.
	if seeds, err := a.ParseImportedCredentials([]byte(`{"type":"sub2api-data","accounts":[{"platform":"gemini","type":"oauth","credentials":{"access_token":"AT"}}]}`)); err != nil || len(seeds) != 0 {
		t.Fatalf("export without OpenAI OAuth accounts = %+v, %v", seeds, err)
	}
}

// A CPA auth folder holds every provider's files; only Codex files and
// records of other tools that use "type" for the plan are imported.
func TestImportSkipsOtherProvidersCPAFiles(t *testing.T) {
	a := &Adapter{}
	for name, test := range map[string]struct {
		record string
		seeds  int
	}{
		"claude":               {`{"type":"claude","access_token":"sk-ant-oat","refresh_token":"sk-ant-ort"}`, 0},
		"gemini object":        {`{"type":"gemini","token":{"access_token":"ya29"}}`, 0},
		"codex":                {`{"type":"codex","access_token":"AT","refresh_token":"RT"}`, 1},
		"chatgpt2api plan":     {`{"type":"Plus","access_token":"AT"}`, 1},
		"codex without tokens": {`{"type":"codex","email":"a@b.c"}`, 0},
	} {
		seeds, err := a.ParseImportedCredentials([]byte(test.record))
		if err != nil || len(seeds) != test.seeds {
			t.Fatalf("%s: seeds = %d, err = %v", name, len(seeds), err)
		}
	}
}

func TestImportDetectsIssuingClient(t *testing.T) {
	codexAT := testJWT(map[string]any{"client_id": codexClientID})
	for name, test := range map[string]struct{ record, want string }{
		"CPA file":                    {`{"type":"codex","access_token":"AT","refresh_token":"RT"}`, codexProfile},
		"CPA SIWC file":               {`{"type":"codex","use_siwc":true,"access_token":"AT","refresh_token":"RT"}`, ""},
		"access token claim":          {`{"access_token":"` + codexAT + `","refresh_token":"RT"}`, codexProfile},
		"platform access token claim": {`{"access_token":"` + testJWT(map[string]any{"client_id": oauthClientID}) + `","refresh_token":"RT"}`, oauthProfile},
		"ID token audience":           {`{"access_token":"AT","id_token":"` + testJWT(map[string]any{"aud": []any{codexClientID}}) + `","refresh_token":"RT"}`, codexProfile},
		"claim beats file default":    {`{"type":"codex","access_token":"` + testJWT(map[string]any{"client_id": "app_other"}) + `","refresh_token":"RT"}`, ""},
		// A wrong label must not send the refresh token to another client.
		"claim beats a given profile": {`{"access_token":"` + codexAT + `","refresh_token":"RT","client_profile":"` + oauthProfile + `"}`, codexProfile},
		"profile without claims":      {`{"access_token":"AT","refresh_token":"RT","client_profile":"` + oauthProfile + `"}`, oauthProfile},
		"plain access token":          {`{"access_token":"AT","refresh_token":"RT"}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			seeds, err := (&Adapter{}).ParseImportedCredentials([]byte(test.record))
			if err != nil {
				t.Fatal(err)
			}
			if got := seeds[0].State["client_profile"]; got != test.want {
				t.Fatalf("client_profile = %q, want %q", got, test.want)
			}
		})
	}
}

// Each refresh token is renewed by the client that issued it, in the form
// that client uses.
func TestRefreshUsesIssuingClient(t *testing.T) {
	for name, test := range map[string]struct {
		accessToken, profile, clientID, scope, origin string
	}{
		"codex":    {"AT", codexProfile, codexClientID, "openid profile email", ""},
		"platform": {"AT", oauthProfile, oauthClientID, "", "https://platform.openai.com"},
		// Accounts imported before profiles were derived have none stored.
		"stored without profile":      {testJWT(map[string]any{"client_id": oauthClientID}), "", oauthClientID, "", "https://platform.openai.com"},
		"stored with a wrong profile": {testJWT(map[string]any{"client_id": codexClientID}), oauthProfile, codexClientID, "openid profile email", ""},
	} {
		want := test
		t.Run(name, func(t *testing.T) {
			var body map[string]string
			var origin string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				origin = r.Header.Get("Origin")
				_ = json.NewDecoder(r.Body).Decode(&body)
				_, _ = w.Write([]byte(`{"access_token":"AT2","refresh_token":"RT2","expires_in":3600}`))
			}))
			t.Cleanup(server.Close)
			a, _ := newTestAdapter(t, server)
			credential := testCredential(t, a, credentials{AccessToken: want.accessToken, RefreshToken: "RT", ClientProfile: want.profile})
			if _, err := a.RefreshCredential(context.Background(), credential); err != nil {
				t.Fatal(err)
			}
			if body["client_id"] != want.clientID || body["scope"] != want.scope || body["refresh_token"] != "RT" || origin != want.origin {
				t.Fatalf("body = %v, origin = %q", body, origin)
			}
		})
	}
}

// The account list shows which client issued each credential and whether it renews.
func TestCredentialMetadataNamesIssuingClient(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	a, _ := newTestAdapter(t, server)
	for name, test := range map[string]struct {
		c           credentials
		kind        string
		unsupported bool
	}{
		"codex":                   {credentials{AccessToken: "AT", RefreshToken: "RT", ClientProfile: codexProfile}, "codex", false},
		"platform":                {credentials{AccessToken: "AT", RefreshToken: "RT", ClientProfile: oauthProfile}, "platform", false},
		"stored without profile":  {credentials{AccessToken: testJWT(map[string]any{"client_id": oauthClientID}), RefreshToken: "RT"}, "platform", false},
		"access token only":       {credentials{AccessToken: "AT"}, "accessToken", false},
		"codex access token only": {credentials{AccessToken: testJWT(map[string]any{"client_id": codexClientID})}, "accessToken", false},
		"other client":            {credentials{AccessToken: testJWT(map[string]any{"client_id": "app_other"}), RefreshToken: "RT"}, "otherClient", true},
	} {
		t.Run(name, func(t *testing.T) {
			got := a.CredentialMetadata(testCredential(t, a, test.c))
			if got.CredentialKind != test.kind || got.RefreshUnsupported != test.unsupported {
				t.Fatalf("metadata = %+v", got)
			}
		})
	}
}

// OpenAI answers token errors as {"error":{"code":…}}; a spent refresh token
// must read as permanent, or it is retried every 15 minutes forever.
func TestOAuthErrorReadsOpenAIShape(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"message":"Your refresh token has already been used.","type":"invalid_request_error","code":"refresh_token_reused"}}`: "refresh_token_reused",
		`{"error":"invalid_grant","error_description":"expired"}`:                                                                        "invalid_grant",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(body))
		}))
		a, _ := newTestAdapter(t, server)
		credential := testCredential(t, a, credentials{AccessToken: "AT", RefreshToken: "RT", ClientProfile: oauthProfile})
		_, err := a.RefreshCredential(context.Background(), credential)
		server.Close()
		var refreshErr *provider.CredentialRefreshError
		if !errors.As(err, &refreshErr) || refreshErr.Code != want || !refreshErr.Permanent || refreshErr.Message == "" {
			t.Fatalf("%s: err = %+v", body, err)
		}
	}
}
