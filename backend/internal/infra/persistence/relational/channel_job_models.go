package relational

import "time"

type channelImageJobModel struct {
	ID             string    `gorm:"size:64;primaryKey"`
	ClientKeyID    uint64    `gorm:"not null;index;uniqueIndex:idx_channel_job_idempotency,priority:1"`
	Provider       string    `gorm:"size:32;not null;index"`
	ModelRouteID   uint64    `gorm:"not null"`
	BindingsJSON   string    `gorm:"type:text;not null;default:'[]'"`
	RequestJSON    string    `gorm:"type:text;not null"`
	PriceJSON      string    `gorm:"type:text;not null"`
	Status         string    `gorm:"size:32;not null;index"`
	PayloadHash    string    `gorm:"size:64;not null"`
	IdempotencyKey *string   `gorm:"size:160;uniqueIndex:idx_channel_job_idempotency,priority:2"`
	CreatedAt      time.Time `gorm:"not null;index"`
	StartedAt      *time.Time
	ExpiresAt      time.Time       `gorm:"not null;index"`
	ClientKey      *clientKeyModel `gorm:"foreignKey:ClientKeyID;references:ID;constraint:OnDelete:RESTRICT"`
}

func (channelImageJobModel) TableName() string { return "channel_image_jobs" }

type channelImageItemModel struct {
	ID             string  `gorm:"size:64;primaryKey"`
	JobID          string  `gorm:"size:64;not null;uniqueIndex:idx_channel_image_position,priority:1;index"`
	Position       int     `gorm:"not null;uniqueIndex:idx_channel_image_position,priority:2"`
	AccountID      *uint64 `gorm:"index"`
	Status         string  `gorm:"size:32;not null;index:idx_channel_image_claim,priority:1"`
	Stage          string  `gorm:"size:32;not null"`
	Submitted      bool
	CheckpointJSON string `gorm:"type:text;not null"`
	AssetsJSON     string `gorm:"type:text;not null"`
	Width          int
	Height         int
	MIME           string `gorm:"size:80"`
	Error          string `gorm:"size:512"`
	ClaimToken     string `gorm:"size:64"`
	Generation     uint64
	LeaseUntil     *time.Time `gorm:"index:idx_channel_image_claim,priority:2"`
	StartedAt      *time.Time
	UpdatedAt      time.Time
	Job            *channelImageJobModel `gorm:"foreignKey:JobID;references:ID;constraint:OnDelete:CASCADE"`
	Account        *accountModel         `gorm:"foreignKey:AccountID;references:ID;constraint:OnDelete:RESTRICT"`
}

func (channelImageItemModel) TableName() string { return "channel_image_items" }

type channelAssetRefModel struct {
	JobID   string                `gorm:"size:64;primaryKey"`
	AssetID string                `gorm:"size:64;primaryKey"`
	Job     *channelImageJobModel `gorm:"foreignKey:JobID;references:ID;constraint:OnDelete:CASCADE"`
	Asset   *mediaAssetModel      `gorm:"foreignKey:AssetID;references:ID;constraint:OnDelete:RESTRICT"`
}

func (channelAssetRefModel) TableName() string { return "channel_asset_refs" }

type channelQuotaReservationModel struct {
	EventID    string `gorm:"size:64;primaryKey"`
	Feature    string `gorm:"size:64;primaryKey"`
	AccountID  uint64 `gorm:"not null;index"`
	Amount     int64
	SentAmount int64
	State      string `gorm:"size:32;not null"`
	CreatedAt  time.Time
	FinishedAt *time.Time
	Account    *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnDelete:CASCADE"`
}

func (channelQuotaReservationModel) TableName() string { return "channel_quota_reservations" }
