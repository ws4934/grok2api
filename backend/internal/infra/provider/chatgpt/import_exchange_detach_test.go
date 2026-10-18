package chatgpt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A refresh-token import keeps the rotated pair when the request leaves
// mid-exchange: ChatGPT spends the submitted refresh token as it answers.
func TestImportExchangeOutlivesTheRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write([]byte(`{"access_token":"fresh-AT","refresh_token":"fresh-RT","expires_in":3600}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	a, _ := newTestAdapter(t, server)
	seeds, err := a.ParseImportedCredentials([]byte(`{"refresh_token":"RT","client_profile":"platform_pkce_v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	seed, err := a.PrepareImportedCredential(ctx, seeds[0])
	if err != nil || seed.AccessToken != "fresh-AT" || seed.RefreshToken != "fresh-RT" {
		t.Fatalf("seed = %+v, err = %v", seed, err)
	}
}
