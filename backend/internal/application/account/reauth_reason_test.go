package account

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The reauth reason is cut to the column size on a rune boundary:
// PostgreSQL rejects invalid UTF-8.
func TestMarkReauthRequiredKeepsReasonValidUTF8(t *testing.T) {
	service, credential, _ := newCredentialRefreshTestService(t, time.Now().UTC())
	reason := strings.Repeat("a", 511) + strings.Repeat("é", 10)
	if err := service.MarkReauthRequired(context.Background(), credential.ID, reason); err != nil {
		t.Fatal(err)
	}
	stored, err := service.accounts.Get(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(stored.LastError) || stored.LastError != strings.Repeat("a", 511) {
		t.Fatalf("stored reason = %q (%d bytes)", stored.LastError, len(stored.LastError))
	}
}
