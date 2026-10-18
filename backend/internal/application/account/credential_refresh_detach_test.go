package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A client that disconnects while the token exchange is in flight must not
// drop the rotated refresh token: the upstream has already consumed the old one.
func TestEnsureCredentialFinishesExchangeAfterCallerLeaves(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	service, credential, adapter := newCredentialRefreshTestService(t, now)
	service.now = func() time.Time { return now }
	adapter.delay = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := service.EnsureCredential(ctx, credential, true)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 80*time.Millisecond {
		t.Fatalf("caller waited for the whole exchange: %s", elapsed)
	}
	stored := waitForStoredRefreshToken(t, service, credential.ID, "refresh-1")
	if stored.EncryptedAccessToken != "access-1" || stored.RefreshFailureCount != 0 {
		t.Fatalf("stored credential = %#v", stored)
	}
}
