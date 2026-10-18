package relational

import (
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Fallback egress for extension channel scopes. The Grok scopes keep their
// fixed columns in egress_operations_config; extension scopes live in their own
// table so new channels never alter that upstream schema.
type egressExtensionFallbackModel struct {
	Scope     string `gorm:"size:32;primaryKey"`
	Mode      string `gorm:"size:16;not null;default:none"`
	NodeID    uint64 `gorm:"not null;default:0"`
	UpdatedAt time.Time
}

func (egressExtensionFallbackModel) TableName() string { return "egress_extension_fallbacks" }

func isGrokOperationsScope(scope egress.Scope) bool {
	switch scope {
	case egress.ScopeBuild, egress.ScopeWeb, egress.ScopeConsole, egress.ScopeWebAsset, egress.ScopeConsoleAsset:
		return true
	}
	return false
}

func withExtensionFallbacks(db *gorm.DB, config egress.OperationsConfig) (egress.OperationsConfig, error) {
	var rows []egressExtensionFallbackModel
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

// saveExtensionFallbacks upserts the extension entries of a configuration after
// locking any fixed node, with the same validity rules as the Grok columns.
func saveExtensionFallbacks(tx *gorm.DB, config egress.OperationsConfig) error {
	now := time.Now().UTC()
	for scope := range config.Fallbacks {
		if isGrokOperationsScope(scope) {
			continue
		}
		fallback := config.FallbackFor(scope)
		if fallback.Mode == egress.FallbackModeFixed {
			var node egressNodeModel
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&node, fallback.NodeID).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err != nil || !node.Enabled || node.ProxyPool || node.EncryptedProxyURL == "" || !egress.SupportsScope(egress.Scope(node.Scope), scope) {
				return repository.ErrEgressFallbackInUse
			}
		}
		row := egressExtensionFallbackModel{Scope: string(scope), Mode: string(fallback.Mode), NodeID: fallback.NodeID, UpdatedAt: now}
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

func extensionFallbackReferencesAny(tx *gorm.DB, ids []uint64) (bool, error) {
	var n int64
	err := tx.Model(&egressExtensionFallbackModel{}).Where("mode = ? AND node_id IN ?", string(egress.FallbackModeFixed), ids).Count(&n).Error
	return n > 0, err
}

func clearExtensionFallbackReferences(tx *gorm.DB, ids []uint64) error {
	return tx.Model(&egressExtensionFallbackModel{}).Where("node_id IN ?", ids).
		Updates(map[string]any{"mode": string(egress.FallbackModeNone), "node_id": 0, "updated_at": time.Now().UTC()}).Error
}

func clearInvalidExtensionFallbacks(tx *gorm.DB) error {
	var rows []egressExtensionFallbackModel
	if err := tx.Where("mode = ?", string(egress.FallbackModeFixed)).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		var node egressNodeModel
		err := tx.First(&node, row.NodeID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && node.Enabled && !node.ProxyPool && node.EncryptedProxyURL != "" && egress.SupportsScope(egress.Scope(node.Scope), egress.Scope(row.Scope)) {
			continue
		}
		if err := clearExtensionFallbackReferences(tx, []uint64{row.NodeID}); err != nil {
			return err
		}
	}
	return nil
}
