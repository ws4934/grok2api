package relational

import (
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Fallback egress per scope; a scope without a row has no fallback.
type egressFallbackModel struct {
	Scope     string `gorm:"size:32;primaryKey"`
	Mode      string `gorm:"size:16;not null;default:none"`
	NodeID    uint64 `gorm:"not null;default:0"`
	UpdatedAt time.Time
}

func (egressFallbackModel) TableName() string { return "egress_fallbacks" }

func withEgressFallbacks(db *gorm.DB, config egress.OperationsConfig) (egress.OperationsConfig, error) {
	var rows []egressFallbackModel
	if err := db.Find(&rows).Error; err != nil {
		return config, err
	}
	if len(rows) == 0 {
		return config, nil
	}
	fallbacks := make(map[egress.Scope]egress.FallbackConfig, len(config.Fallbacks)+len(rows))
	for scope, value := range config.Fallbacks {
		fallbacks[scope] = value
	}
	for _, row := range rows {
		fallbacks[egress.Scope(row.Scope)] = egress.FallbackConfig{Mode: egress.FallbackMode(row.Mode).Normalized(), NodeID: row.NodeID}
	}
	config.Fallbacks = fallbacks
	return config, nil
}

// saveEgressFallbacks upserts every scope of a configuration after locking its
// fixed nodes in ID order and checking that each can serve its scope.
func saveEgressFallbacks(tx *gorm.DB, config egress.OperationsConfig) error {
	ids := make([]uint64, 0, len(config.Fallbacks))
	for scope := range config.Fallbacks {
		if fallback := config.FallbackFor(scope); fallback.Mode == egress.FallbackModeFixed {
			ids = append(ids, fallback.NodeID)
		}
	}
	nodes := make(map[uint64]egressNodeModel, len(ids))
	if len(ids) > 0 {
		var rows []egressNodeModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Order("id ASC").Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			nodes[row.ID] = row
		}
	}
	now := time.Now().UTC()
	for scope := range config.Fallbacks {
		fallback := config.FallbackFor(scope)
		if node, exists := nodes[fallback.NodeID]; fallback.Mode == egress.FallbackModeFixed && (!exists || !servesEgressFallback(node, scope)) {
			return repository.ErrEgressFallbackInUse
		}
		row := egressFallbackModel{Scope: string(scope), Mode: string(fallback.Mode), NodeID: fallback.NodeID, UpdatedAt: now}
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func servesEgressFallback(node egressNodeModel, scope egress.Scope) bool {
	return node.Enabled && !node.ProxyPool && node.EncryptedProxyURL != "" && egress.SupportsScope(egress.Scope(node.Scope), scope)
}

func egressFallbackReferencesAny(tx *gorm.DB, ids []uint64) (bool, error) {
	var n int64
	err := tx.Model(&egressFallbackModel{}).Where("mode = ? AND node_id IN ?", string(egress.FallbackModeFixed), ids).Count(&n).Error
	return n > 0, err
}

func clearEgressFallbackNodeReferences(tx *gorm.DB, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	return tx.Model(&egressFallbackModel{}).Where("node_id IN ?", ids).
		Updates(map[string]any{"mode": string(egress.FallbackModeNone), "node_id": 0, "updated_at": time.Now().UTC()}).Error
}

// clearInvalidEgressFallbackNodeReferences resets each fixed fallback whose node
// can no longer serve its scope; other scopes using the node keep it.
func clearInvalidEgressFallbackNodeReferences(tx *gorm.DB) error {
	var rows []egressFallbackModel
	if err := tx.Where("mode = ?", string(egress.FallbackModeFixed)).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		var node egressNodeModel
		err := tx.First(&node, row.NodeID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && servesEgressFallback(node, egress.Scope(row.Scope)) {
			continue
		}
		if err := tx.Model(&egressFallbackModel{}).Where("scope = ?", row.Scope).
			Updates(map[string]any{"mode": string(egress.FallbackModeNone), "node_id": 0, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
	}
	return nil
}
