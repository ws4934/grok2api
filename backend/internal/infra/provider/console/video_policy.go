package console

import (
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func (*Adapter) VideoQuotaUnits(mode string, o provider.VideoRequestOptions) (int, bool) {
	if mode != QuotaModeVideo || o.Operation != provider.VideoOperationGenerate {
		return 0, false
	}
	images := o.ReferenceImages
	if o.HasImage {
		images++
	}
	if o.HasLastFrame {
		images++
	}
	cost, ok := audit.EstimateOfficialVideoCost(o.UpstreamModel, o.Resolution, o.Duration, images)
	const ticksPerCent int64 = 100_000_000
	return int((cost.CostInUSDTicks + ticksPerCent - 1) / ticksPerCent), ok
}
