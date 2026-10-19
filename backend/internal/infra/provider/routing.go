package provider

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

type QuotaSnapshot struct {
	Tier     account.WebTier
	Windows  []account.QuotaWindow
	SyncedAt time.Time
}

// QuotaGroupSnapshot is an authoritative snapshot for a group of quota modes
// returned by one upstream request. Modes lists the complete local scope so
// callers can atomically remove products that the upstream explicitly reports
// as unavailable without touching unrelated quota windows.
type QuotaGroupSnapshot struct {
	Group    string
	Modes    []string
	Windows  []account.QuotaWindow
	SyncedAt time.Time
}

type QuotaAdapter interface {
	Adapter
	SyncQuota(ctx context.Context, credential account.Credential) (QuotaSnapshot, error)
	SyncQuotaMode(ctx context.Context, credential account.Credential, mode string) (account.QuotaWindow, error)
}

// QuotaGroupAdapter is optional. It is used when one upstream endpoint returns
// several related quota products as one authoritative response.
type QuotaGroupAdapter interface {
	Adapter
	SyncQuotaGroup(ctx context.Context, credential account.Credential, group string) (QuotaGroupSnapshot, error)
}

// QuotaRefreshMetadataAdapter maps a model to an internal refresh group. The
// group is scheduling metadata, not a routable quota mode.
type QuotaRefreshMetadataAdapter interface {
	Adapter
	QuotaRefreshGroup(upstreamModel string) string
}

type RoutingMetadataAdapter interface {
	Adapter
	QuotaMode(upstreamModel string) string
	TierOrder(upstreamModel string) []account.WebTier
}

// QuotaTierOrderAdapter optionally narrows account tiers for a concrete quota
// product. It is used when one public model exposes parameter variants backed
// by different upstream entitlements.
type QuotaTierOrderAdapter interface {
	TierOrderForQuotaMode(upstreamModel, quotaMode string) []account.WebTier
}

// AccountTierAdapter classifies accounts for client-key tier scopes. Without it
// every account is AccountTierUnknown unless the provider is TierExempt.
type AccountTierAdapter interface {
	Adapter
	AccountTier(candidate account.RoutingCandidate) clientkey.AccountTier
}

// CandidateQuotaModeAdapter picks the quota window an account spends when it
// depends on the account (for example its tier) and not only on the model.
type CandidateQuotaModeAdapter interface {
	Adapter
	CandidateQuotaMode(candidate account.RoutingCandidate, quotaMode string) string
}

// StaticModelAdapter reports quota modes whose models every account of the
// provider serves, so routing skips the per-account model capability snapshot.
type StaticModelAdapter interface {
	Adapter
	StaticModel(quotaMode string) bool
}

// SchedulingRiskAdapter flags accounts an administrator may keep out of
// ordinary scheduling; pinned requests still reach them.
type SchedulingRiskAdapter interface {
	Adapter
	SchedulingRisk(credential account.Credential) bool
}

// PricingMetadataAdapter maps Provider-private model identifiers to public billing models.
type PricingMetadataAdapter interface {
	Adapter
	PricingModel(upstreamModel string) string
}

func (r *Registry) Quota(value account.Provider) (QuotaAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(QuotaAdapter)
	return result, ok
}

func (r *Registry) QuotaGroup(value account.Provider) (QuotaGroupAdapter, bool) {
	adapter, ok := r.Get(value)
	if !ok {
		return nil, false
	}
	result, ok := adapter.(QuotaGroupAdapter)
	return result, ok
}

func (r *Registry) QuotaMode(value account.Provider, upstreamModel string) string {
	adapter, ok := r.Get(value)
	if !ok {
		return ""
	}
	metadata, ok := adapter.(RoutingMetadataAdapter)
	if !ok {
		return ""
	}
	return metadata.QuotaMode(upstreamModel)
}

func (r *Registry) QuotaRefreshGroup(value account.Provider, upstreamModel string) string {
	adapter, ok := r.Get(value)
	if !ok {
		return ""
	}
	metadata, ok := adapter.(QuotaRefreshMetadataAdapter)
	if !ok {
		return ""
	}
	return metadata.QuotaRefreshGroup(upstreamModel)
}

// QuotaSnapshotProvider reports a provider whose quota sync reads every
// window at once: after a sync, a mode without a window is not limited.
func (r *Registry) QuotaSnapshotProvider(value account.Provider) bool {
	adapter, ok := r.Quota(value)
	if !ok {
		return false
	}
	_, snapshot := adapter.(interface{ QuotaModeSyncReadsSnapshot() })
	return snapshot
}

func (r *Registry) TierOrder(value account.Provider, upstreamModel string) []account.WebTier {
	adapter, ok := r.Get(value)
	if !ok {
		return nil
	}
	metadata, ok := adapter.(RoutingMetadataAdapter)
	if !ok {
		return nil
	}
	return metadata.TierOrder(upstreamModel)
}

func (r *Registry) TierOrderForQuotaMode(value account.Provider, upstreamModel, quotaMode string) []account.WebTier {
	adapter, ok := r.Get(value)
	if !ok {
		return nil
	}
	if metadata, ok := adapter.(QuotaTierOrderAdapter); ok {
		return metadata.TierOrderForQuotaMode(upstreamModel, quotaMode)
	}
	metadata, ok := adapter.(RoutingMetadataAdapter)
	if !ok {
		return nil
	}
	return metadata.TierOrder(upstreamModel)
}

// AccountTier classifies a routing candidate for client-key tier scopes.
func (r *Registry) AccountTier(value account.Provider, candidate account.RoutingCandidate) clientkey.AccountTier {
	if adapter, ok := adapterAs[AccountTierAdapter](r, value); ok {
		return adapter.AccountTier(candidate)
	}
	return clientkey.AccountTierUnknown
}

// CandidateQuotaMode returns the quota window the candidate spends for quotaMode.
func (r *Registry) CandidateQuotaMode(value account.Provider, candidate account.RoutingCandidate, quotaMode string) string {
	if adapter, ok := adapterAs[CandidateQuotaModeAdapter](r, value); ok {
		return adapter.CandidateQuotaMode(candidate, quotaMode)
	}
	return quotaMode
}

// StaticModel reports whether every account of the provider serves models of quotaMode.
func (r *Registry) StaticModel(value account.Provider, quotaMode string) bool {
	adapter, ok := adapterAs[StaticModelAdapter](r, value)
	return ok && adapter.StaticModel(quotaMode)
}

// SchedulingRisk reports whether the provider flags the account as a scheduling risk.
func (r *Registry) SchedulingRisk(value account.Provider, credential account.Credential) bool {
	adapter, ok := adapterAs[SchedulingRiskAdapter](r, value)
	return ok && adapter.SchedulingRisk(credential)
}

func (r *Registry) PricingModel(value account.Provider, upstreamModel string) string {
	adapter, ok := r.Get(value)
	if !ok {
		return upstreamModel
	}
	metadata, ok := adapter.(PricingMetadataAdapter)
	if !ok {
		return upstreamModel
	}
	if model := metadata.PricingModel(upstreamModel); model != "" {
		return model
	}
	return upstreamModel
}
