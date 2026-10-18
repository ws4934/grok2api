package audit

import (
	"testing"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// The breakdown must name what settlement billed: a custom price beats the
// upstream-reported cost.
func TestBillingBreakdownPrefersCustomPriceOverUpstreamCost(t *testing.T) {
	response := newAuditResponse(auditdomain.Record{
		InputTokens: 1000, OutputTokens: 1000, CostInUSDTicks: 5, EstimatedCostInUSDTicks: 2_000_000_000,
		PricingModel: "grok-4.3", PricingVersion: auditdomain.CustomPricingVersion, PriceID: 7,
	}, nil)
	if response.Billing == nil || response.Billing.Source != "custom" || response.Billing.TotalInUSDTicks != 2_000_000_000 {
		t.Fatalf("custom billing = %#v", response.Billing)
	}
	if response.CostInUSDTicks != 5 {
		t.Fatalf("upstream cost must still be reported, got %d", response.CostInUSDTicks)
	}
}
