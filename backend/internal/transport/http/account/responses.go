package account

import (
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

type accountResponse struct {
	ID                       uint64     `json:"id,string"`
	Provider                 string     `json:"provider"`
	AuthType                 string     `json:"authType"`
	Plan                     string     `json:"plan"`
	WebTier                  string     `json:"webTier,omitempty"`
	WebTierSyncedAt          *time.Time `json:"webTierSyncedAt,omitempty"`
	QuotaSyncedAt            *time.Time `json:"quotaSyncedAt,omitempty"`
	WebNSFWEnabledAt         *time.Time `json:"nsfwEnabledAt,omitempty"`
	WebTermsAcceptedAt       *time.Time `json:"termsAcceptedAt,omitempty"`
	Name                     string     `json:"name"`
	Email                    string     `json:"email,omitempty"`
	UserID                   string     `json:"userId,omitempty"`
	TeamID                   string     `json:"teamId,omitempty"`
	Enabled                  bool       `json:"enabled"`
	AuthStatus               string     `json:"authStatus"`
	ExpiresAt                *time.Time `json:"expiresAt,omitempty"`
	Refreshable              bool       `json:"refreshable"`
	CredentialKind           string     `json:"credentialKind,omitempty"`
	RefreshDueAt             *time.Time `json:"refreshDueAt,omitempty"`
	LastRefreshAt            *time.Time `json:"lastRefreshAt,omitempty"`
	RefreshFailures          int        `json:"refreshFailureCount"`
	LastRefreshErrorStatus   int        `json:"lastRefreshErrorStatus,omitempty"`
	LastRefreshError         string     `json:"lastRefreshErrorCode,omitempty"`
	LastRefreshErrorMessage  string     `json:"lastRefreshErrorMessage,omitempty"`
	LastRefreshErrorResponse string     `json:"lastRefreshErrorResponse,omitempty"`
	Priority                 int        `json:"priority"`
	MaxConcurrent            int        `json:"maxConcurrent"`
	MinimumRemaining         float64    `json:"minimumRemaining"`
	FailureCount             int        `json:"failureCount"`
	CooldownUntil            *time.Time `json:"cooldownUntil,omitempty"`
	LastError                string     `json:"lastError,omitempty"`
	// EnabledDoesNotClearCooldown is set on PATCH when enabled was changed
	// while the account is still cooling. Toggling enabled is not a health reset.
	EnabledDoesNotClearCooldown bool                    `json:"enabledDoesNotClearCooldown,omitempty"`
	LastUsedAt                  *time.Time              `json:"lastUsedAt,omitempty"`
	LinkedAccountID             uint64                  `json:"linkedAccountId,omitempty,string"`
	LinkedName                  string                  `json:"linkedAccountName,omitempty"`
	LinkedProvider              string                  `json:"linkedProvider,omitempty"`
	LinkedAccounts              []linkedAccountResponse `json:"linkedAccounts,omitempty"`
	CreatedAt                   time.Time               `json:"createdAt"`
	ObservedModel               string                  `json:"observedModel,omitempty"`
	ObservedModelAt             *time.Time              `json:"observedModelAt,omitempty"`
	CloudflareCookieConfigured  bool                    `json:"cloudflareCookieConfigured"`
	BuildSuperEntitled          bool                    `json:"buildSuperEntitled"`
	BuildRouteMode              string                  `json:"buildRouteMode"`
	BuildBotFlagged             bool                    `json:"buildBotFlagged"`
	BuildBotFlagSource          int                     `json:"buildBotFlagSource,omitempty"`
	EgressNodeID                uint64                  `json:"egressNodeId,omitempty,string"`
	EgressAssignmentMode        string                  `json:"egressAssignmentMode,omitempty"`
	ModelSyncFailed             bool                    `json:"modelSyncFailed,omitempty"`
	Billing                     *billingResponse        `json:"billing,omitempty"`
	Quota                       quotaResponse           `json:"quota"`
	QuotaWindows                []quotaWindowResponse   `json:"quotaWindows,omitempty"`
}

type linkedAccountResponse struct {
	ID       uint64 `json:"id,string"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	UserID   string `json:"userId,omitempty"`
}

type quotaWindowResponse struct {
	Mode          string                   `json:"mode"`
	Remaining     int                      `json:"remaining"`
	Total         int                      `json:"total"`
	UsagePercent  float64                  `json:"usagePercent"`
	Breakdown     []quotaBreakdownResponse `json:"breakdown,omitempty"`
	WindowSeconds int                      `json:"windowSeconds"`
	ResetAt       *time.Time               `json:"resetAt,omitempty"`
	SyncedAt      *time.Time               `json:"syncedAt,omitempty"`
	Source        string                   `json:"source"`
}

type quotaBreakdownResponse struct {
	ProductCode  int     `json:"productCode"`
	UsagePercent float64 `json:"usagePercent"`
}

type billingResponse struct {
	PlanCode             string                   `json:"planCode,omitempty"`
	PlanName             string                   `json:"planName,omitempty"`
	MonthlyLimit         float64                  `json:"monthlyLimit"`
	Used                 float64                  `json:"used"`
	Remaining            float64                  `json:"remaining"`
	OnDemandCap          float64                  `json:"onDemandCap"`
	OnDemandUsed         float64                  `json:"onDemandUsed"`
	PrepaidBalance       float64                  `json:"prepaidBalance"`
	CreditUsagePercent   float64                  `json:"creditUsagePercent"`
	IsUnifiedBillingUser bool                     `json:"isUnifiedBillingUser"`
	OnDemandEnabled      *bool                    `json:"onDemandEnabled,omitempty"`
	TopUpMethod          string                   `json:"topUpMethod,omitempty"`
	UsagePeriodType      string                   `json:"usagePeriodType,omitempty"`
	UsagePeriodStart     string                   `json:"usagePeriodStart,omitempty"`
	UsagePeriodEnd       string                   `json:"usagePeriodEnd,omitempty"`
	BillingPeriodStart   string                   `json:"billingPeriodStart,omitempty"`
	BillingPeriodEnd     string                   `json:"billingPeriodEnd,omitempty"`
	History              []billingHistoryResponse `json:"history,omitempty"`
	SyncedAt             time.Time                `json:"syncedAt"`
}

type billingHistoryResponse struct {
	Year         int     `json:"year"`
	Month        int     `json:"month"`
	PeriodType   string  `json:"periodType,omitempty"`
	PeriodStart  string  `json:"periodStart,omitempty"`
	PeriodEnd    string  `json:"periodEnd,omitempty"`
	IncludedUsed float64 `json:"includedUsed"`
	OnDemandUsed float64 `json:"onDemandUsed"`
	TotalUsed    float64 `json:"totalUsed"`
}

type quotaResponse struct {
	Type            string     `json:"type"`
	Source          string     `json:"source"`
	Confidence      string     `json:"confidence"`
	Unit            string     `json:"unit,omitempty"`
	Used            float64    `json:"used"`
	Limit           float64    `json:"limit"`
	Remaining       float64    `json:"remaining"`
	UsagePercent    float64    `json:"usagePercent"`
	LimitKnown      bool       `json:"limitKnown"`
	WindowHours     int        `json:"windowHours,omitempty"`
	Observed        bool       `json:"observed"`
	Confirmed       bool       `json:"confirmed"`
	Status          string     `json:"status"`
	PeriodStart     string     `json:"periodStart,omitempty"`
	PeriodEnd       string     `json:"periodEnd,omitempty"`
	ExhaustedAt     *time.Time `json:"exhaustedAt,omitempty"`
	NextProbeAt     *time.Time `json:"nextProbeAt,omitempty"`
	LastConfirmedAt *time.Time `json:"lastConfirmedAt,omitempty"`
}

func newAccountResponse(value accountapp.View) accountResponse {
	c := value.Credential
	buildRouteMode := c.BuildRouteMode
	if c.Provider != accountdomain.ProviderBuild || !buildRouteMode.IsValid() {
		buildRouteMode = accountdomain.BuildRouteAuto
	}
	result := accountResponse{
		ID: c.ID, Provider: string(c.Provider), AuthType: string(c.AuthType), Plan: c.Plan, WebTier: string(c.WebTier),
		WebTierSyncedAt: c.WebTierSyncedAt, QuotaSyncedAt: c.QuotaSyncedAt, WebNSFWEnabledAt: c.WebNSFWEnabledAt, WebTermsAcceptedAt: c.WebTermsAcceptedAt, Name: c.Name, Email: c.Email, UserID: c.UserID, TeamID: c.TeamID,
		Enabled: c.Enabled, AuthStatus: string(c.AuthStatus), Refreshable: c.EncryptedRefreshToken != "" && !value.RefreshUnsupported, CredentialKind: value.CredentialKind,
		RefreshDueAt: c.RefreshDueAt, LastRefreshAt: c.LastRefreshAt,
		RefreshFailures: c.RefreshFailureCount, LastRefreshErrorStatus: c.LastRefreshErrorStatus, LastRefreshError: c.LastRefreshErrorCode, LastRefreshErrorMessage: c.LastRefreshErrorMessage, LastRefreshErrorResponse: c.LastRefreshErrorResponse,
		Priority: c.Priority, MaxConcurrent: c.MaxConcurrent, MinimumRemaining: c.MinimumRemaining,
		FailureCount: c.FailureCount, CooldownUntil: c.CooldownUntil, LastError: c.LastError,
		LastUsedAt: c.LastUsedAt, LinkedAccountID: c.LinkedAccountID, LinkedName: c.LinkedAccountName, LinkedProvider: string(c.LinkedProvider),
		CreatedAt: c.CreatedAt, ObservedModel: c.ObservedModel, ObservedModelAt: c.ObservedModelAt,
		CloudflareCookieConfigured: c.EncryptedCloudflareCookie != "",
		BuildSuperEntitled:         c.BuildSuperEntitled && c.Provider == accountdomain.ProviderBuild,
		BuildRouteMode:             string(buildRouteMode),
		BuildBotFlagged:            value.BuildBotFlagged && c.Provider == accountdomain.ProviderBuild,
		BuildBotFlagSource:         buildBotFlagSourceResponse(c.Provider, value.BuildBotFlagged, value.BuildBotFlagSource),
		EgressNodeID:               c.EgressNodeID,
		EgressAssignmentMode:       string(c.EgressAssignmentMode),
		Quota:                      newQuotaResponse(value.Quota), QuotaWindows: make([]quotaWindowResponse, 0, len(value.QuotaWindows)),
	}
	for _, linked := range c.LinkedAccounts {
		result.LinkedAccounts = append(result.LinkedAccounts, linkedAccountResponse{ID: linked.ID, Provider: string(linked.Provider), Name: linked.Name, Email: linked.Email, UserID: linked.UserID})
	}
	for _, window := range value.QuotaWindows {
		breakdown := make([]quotaBreakdownResponse, 0, len(window.Breakdown))
		for _, item := range window.Breakdown {
			breakdown = append(breakdown, quotaBreakdownResponse{ProductCode: item.ProductCode, UsagePercent: item.UsagePercent})
		}
		result.QuotaWindows = append(result.QuotaWindows, quotaWindowResponse{
			Mode: window.Mode, Remaining: window.Remaining, Total: window.Total,
			UsagePercent: window.UsagePercent, Breakdown: breakdown,
			WindowSeconds: window.WindowSeconds, ResetAt: window.ResetAt, SyncedAt: window.SyncedAt,
			Source: string(window.Source),
		})
	}
	if !c.ExpiresAt.IsZero() {
		expiresAt := c.ExpiresAt
		result.ExpiresAt = &expiresAt
	}
	if value.Billing != nil {
		billing := newBillingResponse(*value.Billing)
		result.Billing = &billing
	}
	return result
}

func buildBotFlagSourceResponse(provider accountdomain.Provider, flagged bool, source int) int {
	if provider != accountdomain.ProviderBuild || !flagged {
		return 0
	}
	if source != 1 && source != 2 {
		return 0
	}
	return source
}

func newQuotaResponse(value accountapp.QuotaView) quotaResponse {
	return quotaResponse{Type: string(value.Type), Source: value.Source, Confidence: value.Confidence, Unit: value.Unit, Used: value.Used, Limit: value.Limit, Remaining: value.Remaining, UsagePercent: value.UsagePercent, LimitKnown: value.LimitKnown, WindowHours: value.WindowHours, Observed: value.Observed, Confirmed: value.Confirmed, Status: string(value.Status), PeriodStart: value.PeriodStart, PeriodEnd: value.PeriodEnd, ExhaustedAt: value.ExhaustedAt, NextProbeAt: value.NextProbeAt, LastConfirmedAt: value.LastConfirmedAt}
}

func newBillingResponse(value accountdomain.Billing) billingResponse {
	history := make([]billingHistoryResponse, 0, len(value.History))
	for _, entry := range value.History {
		history = append(history, billingHistoryResponse{
			Year: entry.Year, Month: entry.Month,
			PeriodType: entry.PeriodType, PeriodStart: entry.PeriodStart, PeriodEnd: entry.PeriodEnd,
			IncludedUsed: entry.IncludedUsed, OnDemandUsed: entry.OnDemandUsed, TotalUsed: entry.TotalUsed,
		})
	}
	return billingResponse{PlanCode: value.PlanCode, PlanName: value.PlanName, MonthlyLimit: value.MonthlyLimit, Used: value.Used, Remaining: value.Remaining(), OnDemandCap: value.OnDemandCap, OnDemandUsed: value.OnDemandUsed, PrepaidBalance: value.PrepaidBalance, CreditUsagePercent: value.CreditUsagePercent, IsUnifiedBillingUser: value.IsUnifiedBillingUser, OnDemandEnabled: value.OnDemandEnabled, TopUpMethod: value.TopUpMethod, UsagePeriodType: value.UsagePeriodType, UsagePeriodStart: value.UsagePeriodStart, UsagePeriodEnd: value.UsagePeriodEnd, BillingPeriodStart: value.BillingPeriodStart, BillingPeriodEnd: value.BillingPeriodEnd, History: history, SyncedAt: value.SyncedAt}
}
