package chatgpt

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// identityUpstream accepts only the access token issued by its token endpoint.
func identityUpstream(t *testing.T, tokenCalls *atomic.Int32) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"fresh-AT","refresh_token":"fresh-RT","expires_in":3600}`))
		case "/backend-api/me", "/backend-api/accounts/check/v4-2023-04-27":
			if r.Header.Get("Authorization") != "Bearer fresh-AT" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"detail":"token expired"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"user-1","email":"a@example.com","accounts":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// An access token that expired or is rejected must not fail an import whose
// refresh token belongs to the supported OAuth client.
func TestImportExchangesRefreshTokenForUnusableAccessToken(t *testing.T) {
	expired := testJWT(map[string]any{"exp": float64(time.Now().Add(-time.Hour).Unix()), "https://api.openai.com/auth": map[string]any{"chatgpt_user_id": "user-1"}})
	revoked := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_user_id": "user-1"}})
	for name, test := range map[string]struct {
		record     string
		wantCalls  int32
		wantAccess string
	}{
		"expired":                {`{"access_token":"` + expired + `","refresh_token":"RT","client_profile":"platform_pkce_v1"}`, 1, "fresh-AT"},
		"rejected":               {`{"access_token":"` + revoked + `","refresh_token":"RT","client_profile":"platform_pkce_v1"}`, 1, "fresh-AT"},
		"expired foreign client": {`{"access_token":"` + expired + `","refresh_token":"RT","client_profile":"other_client"}`, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			var tokenCalls atomic.Int32
			a, _ := newTestAdapter(t, identityUpstream(t, &tokenCalls))
			seeds, err := a.ParseImportedCredentials([]byte(test.record))
			if err != nil {
				t.Fatal(err)
			}
			seed, err := a.PrepareImportedCredential(context.Background(), seeds[0])
			if tokenCalls.Load() != test.wantCalls {
				t.Fatalf("token exchanges = %d, want %d", tokenCalls.Load(), test.wantCalls)
			}
			if test.wantAccess == "" {
				if err == nil {
					t.Fatal("an unusable access token without a supported refresh token was accepted")
				}
				return
			}
			if err != nil || seed.AccessToken != test.wantAccess || seed.RefreshToken != "fresh-RT" || seed.UserID != "user-1" {
				t.Fatalf("seed = %+v, err = %v", seed, err)
			}
		})
	}
}

// A long OAuth error description is cut on a rune boundary.
func TestOAuthErrorMessageStaysValidUTF8(t *testing.T) {
	description := strings.Repeat("a", 511) + strings.Repeat("é", 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"` + description + `"}`))
	}))
	t.Cleanup(server.Close)
	a, _ := newTestAdapter(t, server)
	credential := testCredential(t, a, credentials{AccessToken: "AT", RefreshToken: "RT", ClientProfile: oauthProfile})
	_, err := a.RefreshCredential(context.Background(), credential)
	var refreshErr *provider.CredentialRefreshError
	if !errors.As(err, &refreshErr) {
		t.Fatalf("err = %v", err)
	}
	if !utf8.ValidString(refreshErr.Message) || len(refreshErr.Message) > 512 || !strings.HasPrefix(refreshErr.Message, strings.Repeat("a", 511)) {
		t.Fatalf("message = %q (%d bytes)", refreshErr.Message, len(refreshErr.Message))
	}
}
