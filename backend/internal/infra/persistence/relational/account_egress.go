package relational

import (
	"context"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UpdateEgressBindings assigns one egress node to multiple accounts of one
// provider. A nil node clears the binding and restores normal pool selection.
func (r *AccountRepository) UpdateEgressBindings(ctx context.Context, providerValue account.Provider, ids []uint64, nodeID *uint64, mode account.EgressAssignmentMode, assignedAt time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	values := map[string]any{
		"egress_node_id": nodeID,
	}
	if nodeID == nil {
		values["egress_assignment_mode"] = ""
		values["egress_assigned_at"] = nil
	} else {
		values["egress_assignment_mode"] = string(mode)
		values["egress_assigned_at"] = assignedAt.UTC()
	}
	var updated int64
	var clearedLeaseBlocks int64
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&accountModel{}).Where("provider = ? AND id IN ?", providerValue, ids).Updates(values)
		if result.Error != nil {
			return result.Error
		}
		updated = result.RowsAffected
		if providerValue != account.ProviderBuild {
			return nil
		}
		query := tx.Where("account_id IN ?", ids)
		if nodeID != nil {
			query = query.Where("node_id <> ?", *nodeID)
		}
		deleted := query.Delete(&accountEgressLeaseBlockModel{})
		if deleted.Error != nil {
			return deleted.Error
		}
		clearedLeaseBlocks = deleted.RowsAffected
		return nil
	})
	if err == nil && clearedLeaseBlocks > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountEgressLeaseChanged, Provider: providerValue})
	}
	return updated, mapError(err)
}

// ListEgressAssignments returns all accounts for one provider with their
// binding metadata. It deliberately includes disabled accounts so capacity
// reporting reflects every account that reserves a proxy slot.
func (r *AccountRepository) ListEgressAssignments(ctx context.Context, providerValue account.Provider) ([]account.Credential, error) {
	var rows []accountModel
	if err := r.db.db.WithContext(ctx).Preload("Credential").Preload("WebProfile").
		Where("provider = ?", providerValue).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, mapError(err)
	}
	values := make([]account.Credential, 0, len(rows))
	for _, row := range rows {
		values = append(values, toAccountDomain(row))
	}
	return values, nil
}

func (r *AccountRepository) ListEgressBindingProviders(ctx context.Context, nodeID uint64) ([]account.Provider, error) {
	if nodeID == 0 {
		return []account.Provider{}, nil
	}
	return r.listEgressBindingProviders(r.db.db.WithContext(ctx).Model(&accountModel{}).Where("egress_node_id = ?", nodeID))
}

func (r *AccountRepository) ListEgressSourceBindingProviders(ctx context.Context, sourceID uint64) ([]account.Provider, error) {
	if sourceID == 0 {
		return []account.Provider{}, nil
	}
	query := r.db.db.WithContext(ctx).Model(&accountModel{}).
		Joins("JOIN egress_nodes ON egress_nodes.id = provider_accounts.egress_node_id").
		Where("egress_nodes.source_id = ?", sourceID)
	return r.listEgressBindingProviders(query)
}

func (r *AccountRepository) listEgressBindingProviders(query *gorm.DB) ([]account.Provider, error) {
	var raw []string
	if err := query.Distinct("provider_accounts.provider").Order("provider_accounts.provider ASC").Pluck("provider_accounts.provider", &raw).Error; err != nil {
		return nil, mapError(err)
	}
	result := make([]account.Provider, 0, len(raw))
	for _, value := range raw {
		provider := account.Provider(value)
		if provider.IsValid() {
			result = append(result, provider)
		}
	}
	return result, nil
}

func egressLeaseBlockFromModel(row accountEgressLeaseBlockModel) account.EgressLeaseBlock {
	return account.EgressLeaseBlock{
		AccountID: row.AccountID, NodeID: row.NodeID, Reason: row.Reason, Version: row.Version,
		CooldownUntil: row.CooldownUntil.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}

// ListEgressLeaseBlocks returns the durable guard-owned lease state, including
// expired rows. The sidecar uses expired rows for recovery reconciliation; the
// selector independently ignores them after CooldownUntil as a fail-safe.
func (r *AccountRepository) ListEgressLeaseBlocks(ctx context.Context, limit int, after *account.EgressLeaseBlockCursor) ([]account.EgressLeaseBlock, error) {
	if limit <= 0 || limit > 1001 {
		return nil, repository.ErrConflict
	}
	var rows []accountEgressLeaseBlockModel
	query := r.db.db.WithContext(ctx).
		Table("account_egress_lease_blocks AS block").Select("block.*").
		Joins("JOIN provider_accounts AS account ON account.id = block.account_id").
		Joins("JOIN egress_nodes AS node ON node.id = block.node_id").
		Where("account.provider = ? AND account.enabled = ? AND account.auth_status = ? AND (account.egress_node_id IS NULL OR account.egress_node_id = block.node_id) AND node.enabled = ? AND node.scope = ?", account.ProviderBuild, true, account.AuthStatusActive, true, "grok_build").
		Order("block.cooldown_until ASC, block.account_id ASC, block.node_id ASC").Limit(limit)
	if after != nil {
		cursorTime := after.CooldownUntil.UTC()
		query = query.Where(
			"block.cooldown_until > ? OR (block.cooldown_until = ? AND (block.account_id > ? OR (block.account_id = ? AND block.node_id > ?)))",
			cursorTime, cursorTime, after.AccountID, after.AccountID, after.NodeID,
		)
	}
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]account.EgressLeaseBlock, 0, len(rows))
	for _, row := range rows {
		values = append(values, egressLeaseBlockFromModel(row))
	}
	return values, nil
}

func deleteInvalidEgressLeaseBlocksForAccount(tx *gorm.DB, row accountModel) (int64, error) {
	if account.Provider(row.Provider) != account.ProviderBuild {
		return 0, nil
	}
	query := tx.Where("account_id = ?", row.ID)
	if row.Enabled && account.AuthStatus(row.AuthStatus) == account.AuthStatusActive {
		if row.EgressNodeID == nil {
			return 0, nil
		}
		query = query.Where("node_id <> ?", *row.EgressNodeID)
	}
	result := query.Delete(&accountEgressLeaseBlockModel{})
	return result.RowsAffected, result.Error
}

func (r *AccountRepository) PruneInvalidEgressLeaseBlocks(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, repository.ErrConflict
	}
	var rows []accountEgressLeaseBlockModel
	err := r.db.db.WithContext(ctx).
		Table("account_egress_lease_blocks AS block").Select("block.*").
		Joins("LEFT JOIN provider_accounts AS account ON account.id = block.account_id").
		Joins("LEFT JOIN egress_nodes AS node ON node.id = block.node_id").
		Where("account.id IS NULL OR account.provider <> ? OR account.enabled <> ? OR account.auth_status <> ? OR (account.egress_node_id IS NOT NULL AND account.egress_node_id <> block.node_id) OR node.id IS NULL OR node.enabled <> ? OR node.scope <> ?", account.ProviderBuild, true, account.AuthStatusActive, true, "grok_build").
		Order("block.cooldown_until ASC, block.account_id ASC, block.node_id ASC").Limit(limit).Find(&rows).Error
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	var deleted int64
	err = r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(rows); start += 400 {
			end := min(start+400, len(rows))
			pairs := make([][]any, 0, end-start)
			for _, row := range rows[start:end] {
				pairs = append(pairs, []any{row.AccountID, row.NodeID})
			}
			result := tx.Where("(account_id, node_id) IN ?", pairs).Delete(&accountEgressLeaseBlockModel{})
			if result.Error != nil {
				return result.Error
			}
			deleted += result.RowsAffected
		}
		return nil
	})
	if err == nil && deleted > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountEgressLeaseChanged, Provider: account.ProviderBuild})
	}
	return deleted, err
}

func (r *AccountRepository) DeleteEgressLeaseBlocksByNodes(ctx context.Context, nodeIDs []uint64) (int64, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	result := r.db.db.WithContext(ctx).Where("node_id IN ?", nodeIDs).Delete(&accountEgressLeaseBlockModel{})
	if result.Error == nil && result.RowsAffected > 0 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountEgressLeaseChanged, Provider: account.ProviderBuild})
	}
	return result.RowsAffected, result.Error
}

// UpsertEgressLeaseBlock atomically verifies that the Build account may still
// use the observed node. Unbound accounts can receive a runtime-selected
// account-derived proxy; explicit bindings remain authoritative. A shorter
// concurrent hold cannot replace a longer one or rotate its CAS version.
func (r *AccountRepository) UpsertEgressLeaseBlock(ctx context.Context, value account.EgressLeaseBlock) (account.EgressLeaseBlock, error) {
	value.Reason = strings.TrimSpace(value.Reason)
	value.Version = strings.TrimSpace(value.Version)
	if value.AccountID == 0 || value.NodeID == 0 || value.Reason == "" || len(value.Version) < 16 || len(value.Version) > 64 || value.CooldownUntil.IsZero() {
		return account.EgressLeaseBlock{}, repository.ErrConflict
	}
	value.Reason = truncate(value.Reason, 100)
	value.CooldownUntil = value.CooldownUntil.UTC()
	value.UpdatedAt = time.Now().UTC()
	stored := value
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var owner accountModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "provider", "enabled", "auth_status", "egress_node_id").First(&owner, value.AccountID).Error; err != nil {
			return mapError(err)
		}
		if account.Provider(owner.Provider) != account.ProviderBuild || !owner.Enabled || account.AuthStatus(owner.AuthStatus) != account.AuthStatusActive || (owner.EgressNodeID != nil && *owner.EgressNodeID != value.NodeID) {
			return repository.ErrConflict
		}
		var existing accountEgressLeaseBlockModel
		load := tx.Where("account_id = ? AND node_id = ?", value.AccountID, value.NodeID).Limit(1).Find(&existing)
		if load.Error != nil {
			return load.Error
		}
		if load.RowsAffected > 0 && existing.CooldownUntil.After(value.CooldownUntil) {
			stored = egressLeaseBlockFromModel(existing)
			return nil
		}
		row := accountEgressLeaseBlockModel{
			AccountID: value.AccountID, NodeID: value.NodeID, Reason: value.Reason, Version: value.Version,
			CooldownUntil: value.CooldownUntil, UpdatedAt: value.UpdatedAt,
		}
		created := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}, {Name: "node_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"reason", "version", "cooldown_until", "updated_at"}),
			Where: clause.Where{Exprs: []clause.Expression{clause.Expr{
				SQL: "account_egress_lease_blocks.cooldown_until <= excluded.cooldown_until",
			}}},
		}).Create(&row)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 0 {
			if err := tx.Where("account_id = ? AND node_id = ?", value.AccountID, value.NodeID).First(&existing).Error; err != nil {
				return err
			}
			stored = egressLeaseBlockFromModel(existing)
			return nil
		}
		stored = egressLeaseBlockFromModel(row)
		return nil
	})
	if err == nil {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountEgressLeaseChanged, Provider: account.ProviderBuild, AccountID: value.AccountID})
	}
	return stored, err
}

// DeleteEgressLeaseBlock uses the opaque version as a compare-and-swap token,
// so a stale recovery probe cannot clear a newer quarantine.
func (r *AccountRepository) DeleteEgressLeaseBlock(ctx context.Context, accountID, nodeID uint64, version string) (bool, error) {
	version = strings.TrimSpace(version)
	if accountID == 0 || nodeID == 0 || version == "" {
		return false, repository.ErrConflict
	}
	result := r.db.db.WithContext(ctx).Where("account_id = ? AND node_id = ? AND version = ?", accountID, nodeID, version).Delete(&accountEgressLeaseBlockModel{})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 1 {
		r.notifyInvalidation(ctx, repository.InvalidationEvent{Kind: repository.InvalidationAccountEgressLeaseChanged, Provider: account.ProviderBuild, AccountID: accountID})
		return true, nil
	}
	return false, nil
}
