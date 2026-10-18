package relational

import "time"

type channelSettingsModel struct {
	Provider  string `gorm:"size:32;primaryKey"`
	Version   int
	ValueJSON string `gorm:"type:text;not null"`
	UpdatedAt time.Time
}

func (channelSettingsModel) TableName() string { return "channel_settings" }

type channelAccountModel struct {
	AccountID         uint64 `gorm:"primaryKey"`
	Provider          string `gorm:"size:32;not null;index"`
	Fingerprint       string `gorm:"size:64;index"`
	CredentialVersion uint64 `gorm:"not null"`
	Status            string `gorm:"size:32;not null;index"`
	Failure           string `gorm:"size:512"`
	FailureCount      int
	NextMaintenance   *time.Time
	Plan              string `gorm:"size:160"`
	Refreshable       bool
	ReplaceDuplicate  bool
	ModelsJSON        string `gorm:"type:text;not null"`
	QuotasJSON        string `gorm:"type:text;not null"`
	SyncedAt          *time.Time
	Account           *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (channelAccountModel) TableName() string { return "channel_accounts" }

type channelOAuthModel struct {
	ID             string    `gorm:"size:64;primaryKey"`
	Provider       string    `gorm:"size:32;not null"`
	AdminID        uint64    `gorm:"not null"`
	EncryptedStart string    `gorm:"type:text;not null"`
	StateHash      string    `gorm:"size:64;not null"`
	Status         string    `gorm:"size:32;not null"`
	ExpiresAt      time.Time `gorm:"index;not null"`
	CreatedAt      time.Time
	Admin          *adminModel `gorm:"foreignKey:AdminID;references:ID;constraint:OnDelete:CASCADE"`
}

func (channelOAuthModel) TableName() string { return "channel_oauth_sessions" }

type channelGrantModel struct {
	Provider    string `gorm:"size:32;primaryKey"`
	ClientKeyID uint64 `gorm:"primaryKey"`
	CreatedAt   time.Time
	ClientKey   *clientKeyModel `gorm:"foreignKey:ClientKeyID;references:ID;constraint:OnDelete:CASCADE"`
}

func (channelGrantModel) TableName() string { return "channel_client_key_grants" }

type channelPriceModel struct {
	ID                    uint64 `gorm:"primaryKey"`
	Provider              string `gorm:"size:32;not null;index:idx_channel_prices_target,priority:1"`
	Model                 string `gorm:"size:255;not null;index:idx_channel_prices_target,priority:2"`
	Operation             string `gorm:"size:32;not null;index:idx_channel_prices_target,priority:3"`
	InputPerMillionTicks  int64
	OutputPerMillionTicks int64
	ImageTicks            int64
	Quality               string `gorm:"size:32"`
	Size                  string `gorm:"size:32"`
	CreatedAt             time.Time
}

func (channelPriceModel) TableName() string { return "channel_prices" }
