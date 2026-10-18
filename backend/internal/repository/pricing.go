package repository

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/pricing"
)

// ModelPriceQuery filters price rows; empty fields match everything.
type ModelPriceQuery struct {
	Provider       string
	PricingModel   string
	IncludeHistory bool
}

// ModelPriceRepository stores append-only administrator prices. At most one
// row per provider, pricing model and operation is active.
type ModelPriceRepository interface {
	List(ctx context.Context, query ModelPriceQuery) ([]pricing.Price, error)
	ListByIDs(ctx context.Context, ids []uint64) ([]pricing.Price, error)
	// Replace supersedes the active row of the same key and inserts value in
	// one transaction; a concurrent replacement returns ErrConflict.
	Replace(ctx context.Context, value pricing.Price, now time.Time) (pricing.Price, error)
	// Supersede deactivates an active row; ErrNotFound when none matches.
	Supersede(ctx context.Context, id uint64, now time.Time) error
}
