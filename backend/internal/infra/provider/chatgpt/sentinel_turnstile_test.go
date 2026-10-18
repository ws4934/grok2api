// Tests adapted from SkyKoo/gptGrok2api 1577309; see LICENSE.reference.
package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestSolveSentinelTurnstileToken(t *testing.T) {
	key := "requirements-key"
	operations := [][]any{
		{float64(2), "token", "token-value"},
		{float64(3), "token-value"},
	}
	raw, err := json.Marshal(operations)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(xorSentinelString(string(raw), key)))
	token, err := solveSentinelTurnstileToken(context.Background(), encoded, key)
	if err != nil {
		t.Fatal(err)
	}
	if token != base64.StdEncoding.EncodeToString([]byte("token-value")) {
		t.Fatalf("unexpected token: %q", token)
	}
}
