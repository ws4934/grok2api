package relational

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *ModelRepository) UpsertDiscovered(ctx context.Context, provider account.Provider, upstreamModels []string) error {
	changed := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing []modelRouteModel
		if err := tx.Where("provider = ?", provider).Find(&existing).Error; err != nil {
			return err
		}
		type managedRouteKey struct {
			publicID   string
			capability model.Capability
		}
		publicIDs := make(map[managedRouteKey]bool, len(existing))
		for _, row := range existing {
			if row.Origin != string(model.OriginManual) {
				publicIDs[managedRouteKey{publicID: row.PublicID, capability: model.Capability(row.Capability)}] = true
			}
		}
		rows := make([]modelRouteModel, 0, len(upstreamModels))
		for _, upstreamModel := range upstreamModels {
			localID, capability := discoveredRouteDefaults(provider, upstreamModel)
			publicID, ok := model.NormalizePublicID(provider, localID)
			if !ok {
				return fmt.Errorf("Provider %s 发现了无效模型 ID %q", provider, localID)
			}
			// Managed routes are idempotent by canonical public_id and capability.
			// Manual targets with the same name remain independent pool members.
			key := managedRouteKey{publicID: publicID, capability: capability}
			if publicIDs[key] {
				continue
			}
			// A discovered model whose canonical public ID is already reserved as
			// another route compatibility alias cannot be inserted. Skip it and
			// keep the rest of the batch: aborting here lets one historical rename
			// permanently hide every future model of this provider.
			if err := ensureModelPublicIDNotAlias(tx, publicID, 0); err != nil {
				if errors.Is(err, repository.ErrConflict) {
					continue
				}
				return err
			}
			publicIDs[key] = true
			rows = append(rows, modelRouteModel{PublicID: publicID, Provider: string(provider), UpstreamModel: upstreamModel, Capability: string(capability), Origin: string(model.OriginDiscovered), Enabled: true})
		}
		if len(rows) > 0 {
			// Concurrent discovery is guarded by the managed-route partial unique index.
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 200)
			changed = result.Error == nil && result.RowsAffected > 0
			return result.Error
		}
		return nil
	})
	if err == nil && changed {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
	}
	return err
}

func discoveredRouteDefaults(provider account.Provider, upstreamModel string) (string, model.Capability) {
	switch provider {
	case account.ProviderWeb:
		switch upstreamModel {
		case "grok-imagine-image":
			return "grok-imagine-image-lite", model.CapabilityImage
		case "grok-imagine-image-quality":
			return "grok-imagine-image", model.CapabilityImage
		case "grok-imagine-image-2.0":
			return upstreamModel, model.CapabilityImage
		case "imagine-image-edit":
			return "grok-imagine-image-edit", model.CapabilityImageEdit
		case "grok-imagine-video":
			return upstreamModel, model.CapabilityVideo
		default:
			return upstreamModel, model.CapabilityChat
		}
	case account.ProviderBuild:
		if upstreamModel == "grok-imagine-video-1.5" {
			return upstreamModel, model.CapabilityVideo
		}
		return upstreamModel, model.CapabilityResponses
	case account.ProviderChatGPT:
		// The image routes come from the ChatGPT catalog; discovery only sees
		// their upstream IDs (gpt-image-2, gpt-image-2.5*, codex-gpt-image-*)
		// through the account capability overlay and must never turn them into
		// chat routes.
		switch {
		case strings.HasPrefix(strings.TrimPrefix(upstreamModel, "codex-"), "gpt-image-"):
			return upstreamModel, model.CapabilityImage
		case upstreamModel == "chatgpt-tts":
			return upstreamModel, model.CapabilityTTS
		case upstreamModel == "chatgpt-transcribe":
			return upstreamModel, model.CapabilitySTT
		}
		return upstreamModel, model.CapabilityResponses
	case account.ProviderConsole:
		switch upstreamModel {
		case "grok-imagine-image", "grok-imagine-image-quality", "grok-imagine-image-2.0":
			// The catalog also registers image_edit for the same public model.
			// Discovery only needs one existing managed capability to remain
			// idempotent and must never synthesize a Responses route.
			return upstreamModel, model.CapabilityImage
		case "grok-imagine-video", "grok-imagine-video-1.5":
			return upstreamModel, model.CapabilityVideo
		case "grok-voice-latest", "grok-voice-think-fast-2.0", "grok-voice-think-fast-1.0":
			return upstreamModel, model.CapabilityRealtime
		case "grok-stt":
			return upstreamModel, model.CapabilitySTT
		default:
			return upstreamModel, model.CapabilityResponses
		}
	default:
		return upstreamModel, model.CapabilityResponses
	}
}

func (r *ModelRepository) UpsertRoutes(ctx context.Context, values []model.Route) error {
	providers := make(map[account.Provider]struct{}, len(values))
	for _, value := range values {
		providers[value.Provider] = struct{}{}
	}
	changedProviders := make(map[account.Provider]struct{}, len(providers))
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, original := range values {
			value := original
			publicID, ok := model.NormalizePublicID(value.Provider, value.PublicID)
			if !ok {
				return fmt.Errorf("模型路由目录包含无效公开 ID %q", value.PublicID)
			}
			value.PublicID = publicID
			if strings.TrimSpace(value.PublicID) == "" || strings.TrimSpace(value.UpstreamModel) == "" || value.Provider == "" || value.Capability == "" {
				return fmt.Errorf("模型路由目录包含无效条目")
			}
			var existing modelRouteModel
			err := tx.Where("public_id = ? AND capability = ? AND origin <> ?", value.PublicID, value.Capability, model.OriginManual).First(&existing).Error
			if err == nil {
				continue
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err := ensureModelPublicIDNotAlias(tx, value.PublicID, 0); err != nil {
				return err
			}
			fallbackOrigin := model.OriginDiscovered
			if value.Provider == account.ProviderWeb {
				fallbackOrigin = model.OriginCatalog
			}
			row := modelRouteModel{PublicID: value.PublicID, Provider: string(value.Provider), UpstreamModel: value.UpstreamModel, Capability: string(value.Capability), Origin: string(normalizeRouteOrigin(value.Origin, fallbackOrigin)), Enabled: value.Enabled}
			if err := tx.Create(&row).Error; err != nil {
				return mapError(err)
			}
			changedProviders[value.Provider] = struct{}{}
		}
		return nil
	})
	if err == nil {
		for provider := range changedProviders {
			r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
		}
	}
	return err
}

func (r *ModelRepository) ReplaceProviderRoutes(ctx context.Context, provider account.Provider, values []model.Route) error {
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		normalizedValues := make([]model.Route, len(values))
		for index, value := range values {
			publicID, ok := model.NormalizePublicID(provider, value.PublicID)
			if !ok {
				return fmt.Errorf("模型路由目录包含无效公开 ID %q", value.PublicID)
			}
			value.PublicID = publicID
			if strings.TrimSpace(value.PublicID) == "" || strings.TrimSpace(value.UpstreamModel) == "" || value.Provider != provider || value.Capability == "" {
				return fmt.Errorf("模型路由目录包含无效条目")
			}
			normalizedValues[index] = value
		}
		values = normalizedValues
		var existing []modelRouteModel
		if err := tx.Where("provider = ?", provider).Find(&existing).Error; err != nil {
			return err
		}

		type catalogRouteKey struct {
			value      string
			capability model.Capability
		}
		byPublicID := make(map[catalogRouteKey]modelRouteModel, len(existing))
		byUpstreamNonManual := make(map[catalogRouteKey][]modelRouteModel, len(existing))
		for _, row := range existing {
			if row.Origin == string(model.OriginManual) {
				continue
			}
			publicKey := catalogRouteKey{value: row.PublicID, capability: model.Capability(row.Capability)}
			if current, exists := byPublicID[publicKey]; !exists || row.ID < current.ID {
				byPublicID[publicKey] = row
			}
			upstreamKey := catalogRouteKey{value: row.UpstreamModel, capability: model.Capability(row.Capability)}
			byUpstreamNonManual[upstreamKey] = append(byUpstreamNonManual[upstreamKey], row)
		}
		matched := make(map[int]modelRouteModel, len(values))
		usedIDs := make(map[uint64]bool, len(values))
		for index, value := range values {
			publicKey := catalogRouteKey{value: value.PublicID, capability: value.Capability}
			if row, ok := byPublicID[publicKey]; ok && !usedIDs[row.ID] {
				matched[index] = row
				usedIDs[row.ID] = true
				continue
			}
			// 仅回退匹配非手动路由，避免把手动别名路由改写成目录项。
			candidates := byUpstreamNonManual[catalogRouteKey{value: value.UpstreamModel, capability: value.Capability}]
			var chosen modelRouteModel
			found := false
			for _, candidate := range candidates {
				if usedIDs[candidate.ID] {
					continue
				}
				if !found || candidate.ID < chosen.ID {
					chosen = candidate
					found = true
				}
			}
			if found {
				matched[index] = chosen
				usedIDs[chosen.ID] = true
			}
		}
		for _, row := range existing {
			if usedIDs[row.ID] || row.Origin == string(model.OriginManual) {
				continue
			}
			if err := tx.Delete(&modelRouteModel{}, row.ID).Error; err != nil {
				return err
			}
		}
		// A catalog name may have multiple capability rows. When a previous
		// catalog rename preserved that name as an alias to one member, restoring
		// it must promote the alias back to the formal group name before every
		// matched capability row is validated. A catalog rename may also move a
		// historical alias from one reconciled route to another (for example when
		// an upstream protocol gains distinct public products). This is safe only
		// when the current owner is itself retained by this same catalog update;
		// aliases owned by manual or unrelated routes remain conflicts.
		matchedIDsByPublicID := make(map[string]map[uint64]struct{}, len(values))
		for index, value := range values {
			ids := matchedIDsByPublicID[value.PublicID]
			if ids == nil {
				ids = make(map[uint64]struct{})
				matchedIDsByPublicID[value.PublicID] = ids
			}
			if row, ok := matched[index]; ok {
				ids[row.ID] = struct{}{}
			}
		}
		for publicID, routeIDs := range matchedIDsByPublicID {
			var alias modelRouteAliasModel
			err := tx.Where("alias = ?", publicID).First(&alias).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if _, owned := routeIDs[alias.ModelRouteID]; !owned && !usedIDs[alias.ModelRouteID] {
				return fmt.Errorf("%w: 模型公开 ID %q 已被路由 %d 保留为兼容名称", repository.ErrConflict, publicID, alias.ModelRouteID)
			}
			if err := tx.Delete(&modelRouteAliasModel{}, "alias = ?", publicID).Error; err != nil {
				return err
			}
		}
		for index, value := range values {
			routeID := uint64(0)
			if row, ok := matched[index]; ok {
				routeID = row.ID
			}
			if err := ensureModelPublicIDNotAlias(tx, value.PublicID, routeID); err != nil {
				return err
			}
		}
		// Temporary values allow public IDs or upstream identifiers to be swapped while
		// stable route IDs and key permissions survive.
		for index, row := range matched {
			if row.PublicID != values[index].PublicID {
				if err := preserveModelRouteAlias(tx, row.PublicID, row.ID); err != nil {
					return err
				}
			}
		}
		for _, row := range matched {
			updates := map[string]any{
				"public_id":      fmt.Sprintf("__grok2api_reconcile_%d", row.ID),
				"upstream_model": fmt.Sprintf("__grok2api_upstream_reconcile_%d", row.ID),
			}
			if err := tx.Model(&modelRouteModel{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
				return mapError(err)
			}
		}
		for index, value := range values {
			updates := map[string]any{
				"public_id":      value.PublicID,
				"upstream_model": value.UpstreamModel,
				"capability":     value.Capability,
				"origin":         model.OriginCatalog,
			}
			if row, ok := matched[index]; ok {
				if err := tx.Model(&modelRouteModel{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
					return mapError(err)
				}
				if row.UpstreamModel != value.UpstreamModel {
					if err := renameAccountModelCapability(tx, provider, row.UpstreamModel, value.UpstreamModel); err != nil {
						return err
					}
				}
				continue
			}
			row := modelRouteModel{PublicID: value.PublicID, Provider: string(provider), UpstreamModel: value.UpstreamModel, Capability: string(value.Capability), Origin: string(model.OriginCatalog), Enabled: value.Enabled}
			if err := tx.Create(&row).Error; err != nil {
				return mapError(err)
			}
		}
		return nil
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationRouteChanged, Provider: provider})
	}
	return err
}

func renameAccountModelCapability(tx *gorm.DB, provider account.Provider, oldModel, newModel string) error {
	providerAccounts := tx.Model(&accountModel{}).Select("id").Where("provider = ?", provider)
	duplicates := tx.Model(&accountModelCapabilityModel{}).
		Select("account_id").
		Where("upstream_model = ? AND account_id IN (?)", newModel, providerAccounts)
	if err := tx.Where("upstream_model = ? AND account_id IN (?) AND account_id IN (?)", oldModel, providerAccounts, duplicates).
		Delete(&accountModelCapabilityModel{}).Error; err != nil {
		return err
	}
	return tx.Model(&accountModelCapabilityModel{}).
		Where("upstream_model = ? AND account_id IN (?)", oldModel, providerAccounts).
		Update("upstream_model", newModel).Error
}

func normalizeRouteOrigin(value, fallback model.Origin) model.Origin {
	if value == model.OriginCatalog || value == model.OriginDiscovered || value == model.OriginManual {
		return value
	}
	return fallback
}
