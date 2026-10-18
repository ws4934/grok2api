package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestBuildDeviceLoginImplementsInteractiveAuth(t *testing.T) {
	idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-1","email":"user@example.com","team_id":"team-1"}`)) + ".signature"
	tokenPolls := 0
	adapter := NewAdapter(Config{}, nil)
	adapter.oauth.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch request.URL.Path {
		case "/oauth2/device/code":
			return oauthResponse(http.StatusOK, `{"device_code":"device-123","user_code":"ABCD-EFGH","verification_uri":"https://auth.x.ai/activate","interval":5,"expires_in":900}`), nil
		case "/oauth2/token":
			if request.Form.Get("device_code") != "device-123" {
				t.Fatalf("device code = %q", request.Form.Get("device_code"))
			}
			tokenPolls++
			if tokenPolls == 1 {
				return oauthResponse(http.StatusBadRequest, `{"error":"authorization_pending"}`), nil
			}
			return oauthResponse(http.StatusOK, `{"access_token":"access","refresh_token":"refresh","id_token":"`+idToken+`","expires_in":3600}`), nil
		default:
			t.Fatalf("unexpected OAuth path %q", request.URL.Path)
			return nil, nil
		}
	})}

	var auth provider.InteractiveAuthAdapter = adapter
	challenge, err := auth.StartAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if challenge.Kind != provider.AuthorizationDevice || challenge.State != "device-123" || challenge.UserCode != "ABCD-EFGH" || challenge.ExpiresIn.Minutes() != 15 {
		t.Fatalf("challenge = %#v", challenge)
	}
	if _, err := auth.CompleteAuthorization(context.Background(), challenge, ""); !errors.Is(err, provider.ErrAuthorizationPending) {
		t.Fatalf("first poll err = %v", err)
	}
	seed, err := auth.CompleteAuthorization(context.Background(), challenge, "")
	if err != nil {
		t.Fatal(err)
	}
	if seed.AccessToken != "access" || seed.RefreshToken != "refresh" || seed.UserID != "user-1" || seed.Email != "user@example.com" || seed.OIDCClientID != defaultOAuthClientID {
		t.Fatalf("seed = %#v", seed)
	}
}

func TestBuildImportPreparerKeepsSeedsWithAccessToken(t *testing.T) {
	adapter := NewAdapter(Config{}, nil)
	adapter.oauth.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("seed with an access token must not reach xAI")
		return nil, nil
	})}
	seed := provider.CredentialSeed{Name: "imported", SourceKey: "import:x", AccessToken: "access", RefreshToken: "refresh"}
	prepared, err := adapter.PrepareImportedCredential(context.Background(), seed)
	if err != nil || prepared.Name != seed.Name || prepared.SourceKey != seed.SourceKey || prepared.AccessToken != seed.AccessToken || prepared.RefreshToken != seed.RefreshToken {
		t.Fatalf("prepared = %#v, err = %v", prepared, err)
	}
}
