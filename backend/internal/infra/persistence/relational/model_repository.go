package relational

import (
	"context"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ModelRepository struct {
	db       *Database
	observer repository.InvalidationObserver
}

func NewModelRepository(db *Database) *ModelRepository { return &ModelRepository{db: db} }

func (r *ModelRepository) SetInvalidationObserver(observer repository.InvalidationObserver) {
	r.observer = observer
}

func (r *ModelRepository) notifyInvalidation(ctx context.Context, event repository.InvalidationEvent) {
	if r.observer != nil {
		r.observer(ctx, event)
	}
}

func (r *ModelRepository) Create(ctx context.Context, value model.Route, accountIDs []uint64) (model.Route, error) {
	publicID, ok := model.NormalizePublicID(value.Provider, value.PublicID)
	if !ok {
		return model.Route{}, fmt.Errorf("模型路由公开 ID 无效")
	}
	value.PublicID = publicID
	row := modelRouteModel{
		PublicID: value.PublicID, Provider: string(value.Provider), UpstreamModel: value.UpstreamModel,
		Capability: string(value.Capability), Origin: string(model.OriginManual), Enabled: value.Enabled,
	}
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return mapError(err)
		}
		if err := replaceModelRouteAccounts(tx, row.ID, accountIDs); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return model.Route{}, err
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: value.Provider, UpstreamModel: value.UpstreamModel})
	if len(accountIDs) > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationModelBindingChanged, Provider: value.Provider, UpstreamModel: value.UpstreamModel})
	}
	return r.Get(ctx, row.ID)
}

func (r *ModelRepository) Update(ctx context.Context, value model.Route, accountIDs *[]uint64) (model.Route, error) {
	var storedProvider account.Provider
	var storedUpstreamModel string
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing modelRouteModel
		if err := tx.Where("id = ?", value.ID).First(&existing).Error; err != nil {
			return mapError(err)
		}
		storedProvider = account.Provider(existing.Provider)
		storedUpstreamModel = existing.UpstreamModel
		publicID, ok := model.NormalizePublicID(storedProvider, value.PublicID)
		if !ok {
			return fmt.Errorf("模型路由公开 ID 无效")
		}
		value.PublicID = publicID
		if existing.Origin != string(model.OriginManual) {
			if err := ensureModelPublicIDNotAlias(tx, value.PublicID, existing.ID); err != nil {
				return err
			}
		}
		if existing.PublicID != value.PublicID {
			if err := preserveModelRouteAlias(tx, existing.PublicID, existing.ID); err != nil {
				return err
			}
		}
		result := tx.Model(&modelRouteModel{}).Where("id = ?", value.ID).Updates(map[string]any{
			"public_id": value.PublicID,
			"enabled":   value.Enabled,
		})
		if result.Error != nil {
			return mapError(result.Error)
		}
		if result.RowsAffected == 0 {
			var count int64
			if err := tx.Model(&modelRouteModel{}).Where("id = ?", value.ID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return repository.ErrNotFound
			}
		}
		if accountIDs != nil {
			return replaceModelRouteAccounts(tx, value.ID, *accountIDs)
		}
		return nil
	})
	if err != nil {
		return model.Route{}, err
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: storedProvider, UpstreamModel: storedUpstreamModel})
	if accountIDs != nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationModelBindingChanged, Provider: storedProvider, UpstreamModel: storedUpstreamModel})
	}
	return r.Get(ctx, value.ID)
}

func (r *ModelRepository) Delete(ctx context.Context, id uint64) error {
	var existing modelRouteModel
	if err := r.db.db.WithContext(ctx).First(&existing, id).Error; err != nil {
		return mapError(err)
	}
	result := r.db.db.WithContext(ctx).Delete(&modelRouteModel{}, id)
	if result.Error != nil {
		return mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: account.Provider(existing.Provider), UpstreamModel: existing.UpstreamModel})
	return nil
}

func (r *ModelRepository) DeleteMany(ctx context.Context, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var existing []modelRouteModel
	if err := r.db.db.WithContext(ctx).Where("id IN ?", ids).Find(&existing).Error; err != nil {
		return 0, err
	}
	result := r.db.db.WithContext(ctx).Where("id IN ?", ids).Delete(&modelRouteModel{})
	if result.Error == nil && result.RowsAffected > 0 {
		providers := make(map[account.Provider]struct{}, len(existing))
		for _, row := range existing {
			providers[account.Provider(row.Provider)] = struct{}{}
		}
		for provider := range providers {
			r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
		}
	}
	return result.RowsAffected, mapError(result.Error)
}

func replaceModelRouteAccounts(tx *gorm.DB, routeID uint64, accountIDs []uint64) error {
	if err := tx.Where("model_route_id = ?", routeID).Delete(&modelRouteAccountModel{}).Error; err != nil {
		return err
	}
	if len(accountIDs) == 0 {
		return nil
	}
	rows := make([]modelRouteAccountModel, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		rows = append(rows, modelRouteAccountModel{ModelRouteID: routeID, AccountID: accountID})
	}
	return tx.CreateInBatches(rows, 200).Error
}

func (r *ModelRepository) UpdateManyEnabled(ctx context.Context, ids []uint64, enabled bool) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	providers := make(map[account.Provider]struct{})
	var updated int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []modelRouteModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Order("id ASC").Find(&existing).Error; err != nil {
			return err
		}
		for _, row := range existing {
			providers[account.Provider(row.Provider)] = struct{}{}
		}
		result := tx.Model(&modelRouteModel{}).Where("id IN ? AND enabled <> ?", ids, enabled).Update("enabled", enabled)
		updated = result.RowsAffected
		return result.Error
	})
	if err == nil && updated > 0 {
		for provider := range providers {
			r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
		}
	}
	return updated, err
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func mapModelRows(rows []modelRouteModel) []model.Route {
	out := make([]model.Route, 0, len(rows))
	for _, row := range rows {
		out = append(out, toModelDomain(row))
	}
	return out
}
