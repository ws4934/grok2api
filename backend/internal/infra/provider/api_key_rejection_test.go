package provider

import (
	"net/http"
	"testing"
)

func TestBadRequestAPIKeyRejectionIsNotUserInputOrSSOFailure(t *testing.T) {
	for _, test := range []struct {
		code, message string
		want          bool
	}{
		{"invalid-argument", "Incorrect API key provided. You can obtain an API key from https://console.x.ai.", true},
		{"invalid_api_key", "authentication failed", true},
		{"incorrect_api_key", "", true},
		{"invalid_argument", "duration must be between 1 and 15", false},
		{"invalid_argument", `The prompt contains the text "Incorrect API key provided"`, false},
		{"invalid_api_key", "Generated video rejected by content moderation.", false},
	} {
		if got := IsBadRequestAPIKeyRejection(400, test.code, test.message); got != test.want {
			t.Fatalf("%q %q: got=%v", test.code, test.message, got)
		}
		message := LocalizePublicErrorMessage(400, test.code, test.message)
		if test.want && message != APIKeyRejectedMessage {
			t.Fatalf("public message=%q", message)
		}
	}
	if IsBadRequestAPIKeyRejection(http.StatusBadGateway, "invalid_api_key", "Incorrect API key provided") {
		t.Fatal("uncertain server failure was marked as an explicit key refusal")
	}
	result := ClassifyCredentialRejection(400, []byte(`{"code":"invalid-argument","error":"Incorrect API key provided."}`), nil)
	if result.Rejected || result.QuotaExhausted {
		t.Fatalf("API key failure invalidated SSO or quota: %+v", result)
	}
}
