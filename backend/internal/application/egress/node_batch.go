package egress

import (
	"context"
	"errors"
	"fmt"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type UnhealthyCleanupPreview struct {
	Nodes               int64
	BoundAccounts       int64
	SubscriptionManaged int64
}

// BatchNodeDeleter is optional so lightweight repository adapters only need
// the single-node contract unless they can provide an atomic bulk operation.
type BatchNodeDeleter interface {
	DeleteEgressNodes(context.Context, []uint64) (int, error)
}

type BatchNodeEnabledUpdater interface {
	UpdateEgressNodesEnabled(context.Context, []uint64, bool) (int, error)
}

// UpdateManyEnabled changes only the scheduling state, leaving proxy secrets,
// health, probes, and account bindings untouched.
func (s *Service) UpdateManyEnabled(ctx context.Context, nodeIDs []uint64, enabled bool) (int, error) {
	ids := uniqueIDs(nodeIDs)
	if len(ids) == 0 {
		return 0, fmt.Errorf("%w: 代理节点参数无效", ErrInvalidInput)
	}

	// Disabling a fixed fallback would make the persisted routing policy
	// invalid. At most five fallback nodes need point lookups, regardless of
	// the batch size.
	if !enabled && s.operations != nil {
		config, err := s.operations.GetEgressOperationsConfig(ctx)
		if err != nil {
			return 0, err
		}
		selected := make(map[uint64]struct{}, len(ids))
		for _, id := range ids {
			selected[id] = struct{}{}
		}
		fallbackNodeIDs := make(map[uint64]struct{}, len(allServiceScopes()))
		for _, scope := range allServiceScopes() {
			fallback := config.FallbackFor(scope)
			if fallback.Mode == domain.FallbackModeFixed {
				if _, exists := selected[fallback.NodeID]; exists {
					fallbackNodeIDs[fallback.NodeID] = struct{}{}
				}
			}
		}
		for id := range fallbackNodeIDs {
			node, err := s.repository.GetEgressNode(ctx, id)
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			if err != nil {
				return 0, err
			}
			node.Enabled = false
			if err := s.validateFallbackNodeUpdateWithConfig(node, config); err != nil {
				return 0, err
			}
		}
	}

	if batch, ok := s.repository.(BatchNodeEnabledUpdater); ok {
		updated, err := batch.UpdateEgressNodesEnabled(ctx, ids, enabled)
		if errors.Is(err, repository.ErrEgressFallbackInUse) {
			return 0, fmt.Errorf("%w: 固定回退节点不能被批量禁用", ErrInvalidInput)
		}
		if err != nil {
			return 0, err
		}
		if updated > 0 {
			s.forgetClearances(ids)
			if !enabled {
				if clearErr := s.clearQualityLeasesForNodes(ctx, ids); clearErr != nil {
					return updated, clearErr
				}
			}
		}
		return updated, nil
	}

	updated := 0
	for _, id := range ids {
		node, err := s.repository.GetEgressNode(ctx, id)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return updated, err
		}
		if node.Enabled == enabled {
			continue
		}
		node.Enabled = enabled
		if _, err := s.repository.UpdateEgressNode(ctx, node); err != nil {
			return updated, err
		}
		s.forgetClearance(id)
		updated++
	}
	if !enabled && updated > 0 {
		if err := s.clearQualityLeasesForNodes(ctx, ids); err != nil {
			return updated, err
		}
	}
	return updated, nil
}

// DeleteMany removes nodes in one repository operation when available. The
// relational implementation also clears any account bindings in that same
// transaction, so a deleted node can never remain referenced by an account.
func (s *Service) DeleteMany(ctx context.Context, nodeIDs []uint64) (int, error) {
	ids := uniqueIDs(nodeIDs)
	if len(ids) == 0 {
		return 0, fmt.Errorf("%w: 代理节点参数无效", ErrInvalidInput)
	}
	if batch, ok := s.repository.(BatchNodeDeleter); ok {
		deleted, err := batch.DeleteEgressNodes(ctx, ids)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			s.forgetClearance(id)
		}
		s.invalidateOperationsConfig()
		return deleted, nil
	}

	deleted := 0
	for _, id := range ids {
		if err := s.Delete(ctx, id); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func (s *Service) PreviewUnhealthyCleanup(ctx context.Context) (UnhealthyCleanupPreview, error) {
	if cleaner, ok := s.repository.(repository.EgressNodeUnhealthyCleaner); ok {
		value, err := cleaner.PreviewUnhealthyEgressNodes(ctx)
		return UnhealthyCleanupPreview{
			Nodes: value.Nodes, BoundAccounts: value.BoundAccounts, SubscriptionManaged: value.SubscriptionManaged,
		}, err
	}
	values, err := s.repository.ListEgressNodes(ctx, "", repository.SortQuery{})
	if err != nil {
		return UnhealthyCleanupPreview{}, err
	}
	result := UnhealthyCleanupPreview{}
	for _, value := range values {
		if value.IPv4Probe.Status != domain.ProbeStatusUnhealthy || value.IPv6Probe.Status != domain.ProbeStatusUnhealthy {
			continue
		}
		result.Nodes++
		result.BoundAccounts += int64(value.AssignedAccountCount)
		if value.SourceID != 0 {
			result.SubscriptionManaged++
		}
	}
	return result, nil
}

func (s *Service) DeleteUnhealthy(ctx context.Context) (int, error) {
	if cleaner, ok := s.repository.(repository.EgressNodeUnhealthyCleaner); ok {
		ids, err := cleaner.DeleteUnhealthyEgressNodes(ctx)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			s.forgetClearance(id)
		}
		if len(ids) > 0 {
			s.invalidateOperationsConfig()
		}
		return len(ids), nil
	}
	values, err := s.repository.ListEgressNodes(ctx, "", repository.SortQuery{})
	if err != nil {
		return 0, err
	}
	ids := make([]uint64, 0)
	for _, value := range values {
		if value.IPv4Probe.Status == domain.ProbeStatusUnhealthy && value.IPv6Probe.Status == domain.ProbeStatusUnhealthy {
			ids = append(ids, value.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return s.DeleteMany(ctx, ids)
}
