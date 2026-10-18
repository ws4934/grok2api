package channel

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

type Account struct {
	ID                uint64           `json:"id"`
	Provider          account.Provider `json:"provider"`
	Name              string           `json:"name"`
	Enabled           bool             `json:"enabled"`
	MaxConcurrent     int              `json:"max_concurrent"`
	Identity          Identity         `json:"identity"`
	Status            string           `json:"status"`
	Failure           string           `json:"failure,omitempty"`
	FailureCount      int              `json:"failure_count"`
	NextMaintenance   *time.Time       `json:"next_maintenance,omitempty"`
	CredentialVersion uint64           `json:"credential_version"`
	Refreshable       bool             `json:"refreshable"`
	ReplaceDuplicate  bool             `json:"-"`
	ExpiresAt         *time.Time       `json:"expires_at,omitempty"`
	EgressNodeID      *uint64          `json:"egress_node_id,omitempty"`
	SyncedAt          *time.Time       `json:"synced_at,omitempty"`
	Models            []string         `json:"models"`
	Quotas            []FeatureQuota   `json:"quotas"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
}
type AccountSecret struct {
	Account
	Fingerprint          string
	EncryptedCredentials string
}
type OAuthSession struct {
	ID             string
	Provider       account.Provider
	AdminID        uint64
	EncryptedStart string
	StateHash      string
	Status         string
	ExpiresAt      time.Time
}
type Price struct {
	ID                    uint64           `json:"id"`
	Provider              account.Provider `json:"provider"`
	Model                 string           `json:"model"`
	Operation             string           `json:"operation"`
	Version               uint64           `json:"version"`
	InputPerMillionTicks  int64            `json:"input_per_million_ticks"`
	OutputPerMillionTicks int64            `json:"output_per_million_ticks"`
	ImageTicks            int64            `json:"image_ticks"`
	Quality               string           `json:"quality,omitempty"`
	Size                  string           `json:"size,omitempty"`
	CreatedAt             time.Time        `json:"created_at"`
}

// Repository implementations atomically persist shared accounts and extension metadata.
// Credential writes use expectedVersion (0 creates); normal reads never load secrets.
type Repository interface {
	FindAccount(context.Context, account.Provider, string, Identity) (Account, error)
	Settings(context.Context, account.Provider) (Settings, error)
	SaveSettings(context.Context, account.Provider, Settings) error
	ListAccounts(context.Context, account.Provider, int, int) ([]Account, int64, error)
	GetAccount(context.Context, account.Provider, uint64) (Account, error)
	GetSecret(context.Context, account.Provider, uint64) (AccountSecret, error)
	SaveAccount(context.Context, AccountSecret, uint64) (Account, error)
	DeleteAccount(context.Context, account.Provider, uint64) error
	CreateOAuth(context.Context, OAuthSession) error
	ClaimOAuth(context.Context, account.Provider, string, uint64, string, time.Time) (OAuthSession, error)
	FinishOAuth(context.Context, string, string) error
	Grants(context.Context, account.Provider) ([]uint64, error)
	SetGrant(context.Context, account.Provider, uint64, bool) error
	Granted(context.Context, account.Provider, uint64) (bool, error)
	Prices(context.Context, account.Provider) ([]Price, error)
	SavePrice(context.Context, Price) (Price, error)
}
