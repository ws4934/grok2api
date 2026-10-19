package relational

import (
	"errors"
	"fmt"
	"strings"

	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func preserveModelRouteAlias(tx *gorm.DB, alias string, routeID uint64) error {
	alias = strings.TrimSpace(alias)
	if alias == "" || routeID == 0 {
		return nil
	}
	var route modelRouteModel
	if err := tx.Where("public_id = ? AND id <> ?", alias, routeID).First(&route).Error; err == nil {
		// Another target still owns the old public name, so the group remains
		// reachable without creating a compatibility alias for this renamed target.
		return nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var existing modelRouteAliasModel
	err := tx.Where("alias = ?", alias).First(&existing).Error
	if err == nil {
		if existing.ModelRouteID == routeID {
			return nil
		}
		var current modelRouteModel
		if err := tx.Select("origin").Where("id = ?", routeID).First(&current).Error; err != nil {
			return err
		}
		if current.Origin == string(modeldomain.OriginManual) {
			return nil
		}
		return fmt.Errorf("%w: 模型兼容名称 %q 已绑定路由 %d", repository.ErrConflict, alias, existing.ModelRouteID)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return tx.Create(&modelRouteAliasModel{Alias: alias, ModelRouteID: routeID}).Error
}

func ensureModelPublicIDNotAlias(tx *gorm.DB, publicID string, routeID uint64) error {
	var alias modelRouteAliasModel
	query := tx.Where("alias = ?", publicID)
	if routeID != 0 {
		query = query.Where("model_route_id <> ?", routeID)
	}
	err := query.First(&alias).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: 模型公开 ID %q 已被路由 %d 保留为兼容名称", repository.ErrConflict, publicID, alias.ModelRouteID)
}
