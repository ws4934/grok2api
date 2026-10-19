package relational

import "time"

// modelPriceModel is one append-only administrator price version.
type modelPriceModel struct {
	ID                    uint64    `gorm:"primaryKey;autoIncrement"`
	Provider              string    `gorm:"size:32;not null;check:chk_model_prices_provider,length(provider) BETWEEN 1 AND 32"`
	PricingModel          string    `gorm:"size:255;not null;check:chk_model_prices_pricing_model,length(pricing_model) BETWEEN 1 AND 255"`
	Operation             string    `gorm:"size:16;not null;check:chk_model_prices_operation,operation IN ('text','image','image_edit','video','tts','stt')"`
	InputTokenTicks       int64     `gorm:"not null;default:0;check:chk_model_prices_rates,input_token_ticks >= 0 AND cached_input_token_ticks >= 0 AND output_token_ticks >= 0 AND input_image_ticks >= 0 AND output_image_ticks >= 0 AND output_second_ticks >= 0 AND character_ticks >= 0"`
	CachedInputTokenTicks int64     `gorm:"not null;default:0"`
	OutputTokenTicks      int64     `gorm:"not null;default:0"`
	InputImageTicks       int64     `gorm:"not null;default:0"`
	OutputImageTicks      int64     `gorm:"not null;default:0"`
	OutputSecondTicks     int64     `gorm:"not null;default:0"`
	CharacterTicks        int64     `gorm:"not null;default:0"`
	CreatedAt             time.Time `gorm:"not null"`
	SupersededAt          *time.Time
}

func (modelPriceModel) TableName() string { return "model_prices" }
