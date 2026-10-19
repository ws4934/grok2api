package chatgpt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A sub2api export lists every platform; only its OpenAI OAuth accounts are
// imported, with their refresh token bound to the client that issued it.
func TestImportSub2APIExportKeepsOpenAIOAuthAccounts(t *testing.T) {
	a := &Adapter{}
	seeds, err := a.ParseImportedCredentials([]byte(`{"type":"sub2api-data","version":1,"proxies":[],"accounts":[
		{"name":"claude","platform":"anthropic","type":"oauth","credentials":{"access_token":"sk-ant-oat","refresh_token":"sk-ant-ort"}},
		{"name":"key","platform":"openai","type":"apikey","credentials":{"api_key":"sk-key"}},
		{"name":"gpt","platform":"openai","type":"oauth","credentials":{"access_token":"AT","refresh_token":"RT","client_id":"app_EMoamEEZ73f0CkXaXp7hrann"}}]}`))
	if err != nil || len(seeds) != 1 {
		t.Fatalf("seeds = %+v, err = %v", seeds, err)
	}
	if seeds[0].Name != "gpt" || seeds[0].AccessToken != "AT" || seeds[0].RefreshToken != "RT" || seeds[0].State["client_profile"] != codexProfile {
		t.Fatalf("seed = %+v", seeds[0])
	}
	if _, err := a.ParseImportedCredentials([]byte(`{"type":"sub2api-data","accounts":[{"platform":"gemini","type":"oauth","credentials":{"access_token":"AT"}}]}`)); err == nil {
		t.Fatal("an export without OpenAI OAuth accounts was accepted")
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
		"explicit profile":            {`{"access_token":"` + codexAT + `","refresh_token":"RT","client_profile":"` + oauthProfile + `"}`, oauthProfile},
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
	for profile, want := range map[string]struct{ clientID, scope, origin string }{
		codexProfile: {codexClientID, "openid profile email", ""},
		oauthProfile: {oauthClientID, "", "https://platform.openai.com"},
	} {
		t.Run(profile, func(t *testing.T) {
			var body map[string]string
			var origin string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				origin = r.Header.Get("Origin")
				_ = json.NewDecoder(r.Body).Decode(&body)
				_, _ = w.Write([]byte(`{"access_token":"AT2","refresh_token":"RT2","expires_in":3600}`))
			}))
			t.Cleanup(server.Close)
			a, _ := newTestAdapter(t, server)
			credential := testCredential(t, a, credentials{AccessToken: "AT", RefreshToken: "RT", ClientProfile: profile})
			if _, err := a.RefreshCredential(context.Background(), credential); err != nil {
				t.Fatal(err)
			}
			if body["client_id"] != want.clientID || body["scope"] != want.scope || body["refresh_token"] != "RT" || origin != want.origin {
				t.Fatalf("body = %v, origin = %q", body, origin)
			}
		})
	}
}
