package clientkey

import (
	"context"
	"errors"
	"testing"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type failingCancelRepo struct {
	repository.ClientKeyRepository
	protected []string
}

func (r *failingCancelRepo) ReserveBillingUsage(context.Context, uint64, string, int64, time.Time) (bool, error) {
	return true, nil
}

func (r *failingCancelRepo) CancelBillingReservation(context.Context, string) error {
	return errors.New("database is locked")
}

func (r *failingCancelRepo) CleanupExpiredBillingReservations(_ context.Context, _ time.Time, _ int, protected ...[]string) (int, error) {
	for _, set := range protected {
		r.protected = append(r.protected, set...)
	}
	return 0, nil
}

// A reservation whose cancel failed is left to the expiry cleanup instead of
// staying protected for the life of the process.
func TestFailedCancelLeavesReservationToExpiryCleanup(t *testing.T) {
	repo := &failingCancelRepo{}
	service := NewService(repo, nil, nil, 0, 0, nil)
	key := clientkeydomain.Key{ID: 1, BillingLimitUSDTicks: 1000}
	if _, err := service.ReserveBilling(context.Background(), key, "evt_1", 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := service.CancelBilling(context.Background(), "evt_1"); err == nil {
		t.Fatal("cancel error was swallowed")
	}
	if _, err := service.CleanupExpiredBilling(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if len(repo.protected) != 0 {
		t.Fatalf("protected from cleanup: %v", repo.protected)
	}
}
