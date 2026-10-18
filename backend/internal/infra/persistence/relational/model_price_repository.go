package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/pricing"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type ModelPriceRepository struct{ db *Database }

func NewModelPriceRepository(database *Database) *ModelPriceRepository {
	return &ModelPriceRepository{db: database}
}

func (r *ModelPriceRepository) List(ctx context.Context, query repository.ModelPriceQuery) ([]pricing.Price, error) {
	db := r.db.db.WithContext(ctx).Model(&modelPriceModel{})
	if query.Provider != "" {
		db = db.Where("provider = ?", query.Provider)
	}
	if query.PricingModel != "" {
		db = db.Where("pricing_model = ?", query.PricingModel)
	}
	if !query.IncludeHistory {
		db = db.Where("superseded_at IS NULL")
	}
	var rows []modelPriceModel
	if err := db.Order("provider, pricing_model, operation, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return toPrices(rows), nil
}

func (r *ModelPriceRepository) ListByIDs(ctx context.Context, ids []uint64) ([]pricing.Price, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []modelPriceModel
	if err := r.db.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	return toPrices(rows), nil
}

func (r *ModelPriceRepository) Replace(ctx context.Context, value pricing.Price, now time.Time) (pricing.Price, error) {
	row := modelPriceModel{
		Provider: value.Provider, PricingModel: value.PricingModel, Operation: string(value.Operation),
		InputTokenTicks: value.InputToken, CachedInputTokenTicks: value.CachedInputToken, OutputTokenTicks: value.OutputToken,
		InputImageTicks: value.InputImage, OutputImageTicks: value.OutputImage, OutputSecondTicks: value.OutputSecond,
		CharacterTicks: value.Character, CreatedAt: now,
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&modelPriceModel{}).
			Where("provider = ? AND pricing_model = ? AND operation = ? AND superseded_at IS NULL", row.Provider, row.PricingModel, row.Operation).
			Update("superseded_at", now).Error; err != nil {
			return err
		}
		// A concurrent replacement commits its own active row first; the
		// partial unique index then rejects this insert.
		return tx.Create(&row).Error
	})
	if err != nil {
		return pricing.Price{}, mapError(err)
	}
	return toPrice(row), nil
}

func (r *ModelPriceRepository) Supersede(ctx context.Context, id uint64, now time.Time) error {
	result := r.db.db.WithContext(ctx).Model(&modelPriceModel{}).Where("id = ? AND superseded_at IS NULL", id).Update("superseded_at", now)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func toPrices(rows []modelPriceModel) []pricing.Price {
	values := make([]pricing.Price, 0, len(rows))
	for _, row := range rows {
		values = append(values, toPrice(row))
	}
	return values
}

func toPrice(row modelPriceModel) pricing.Price {
	return pricing.Price{
		ID: row.ID, Provider: row.Provider, PricingModel: row.PricingModel, Operation: pricing.Operation(row.Operation),
		Rates: pricing.Rates{
			InputToken: row.InputTokenTicks, CachedInputToken: row.CachedInputTokenTicks, OutputToken: row.OutputTokenTicks,
			InputImage: row.InputImageTicks, OutputImage: row.OutputImageTicks, OutputSecond: row.OutputSecondTicks,
			Character: row.CharacterTicks,
		},
		CreatedAt: row.CreatedAt, SupersededAt: row.SupersededAt,
	}
}
