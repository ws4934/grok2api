package audit

import (
	"testing"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// The billing cursor must carry the same amount the repository sorts on.
func TestBillingCursorUsesCustomPriceOverUpstreamCost(t *testing.T) {
	for index, tc := range []struct {
		record auditdomain.Record
		want   string
	}{
		{auditdomain.Record{CostInUSDTicks: 5, EstimatedCostInUSDTicks: 2000, PriceID: 7}, "2000"},
		{auditdomain.Record{CostInUSDTicks: 20, EstimatedCostInUSDTicks: 90}, "20"},
		{auditdomain.Record{EstimatedCostInUSDTicks: 90}, "90"},
	} {
		if got := formatAuditCursorValue(tc.record, "billing"); got != tc.want {
			t.Fatalf("case %d: cursor = %s, want %s", index, got, tc.want)
		}
	}
}
