package account

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

type observedModelState struct {
	model       string
	persistedAt time.Time
}

type observedModelShard struct {
	sync.Mutex
	values        map[uint64]observedModelState
	lastCleanupAt time.Time
}

func (s *Service) ObserveResponseModel(ctx context.Context, id uint64, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	_, err, _ := s.observedModelWrites.Do(strconv.FormatUint(id, 10)+"\x00"+model, func() (any, error) {
		now := s.now()
		shard := s.observedModelShard(id)
		shard.Lock()
		if shard.values == nil {
			shard.values = make(map[uint64]observedModelState)
		}
		if shard.lastCleanupAt.IsZero() || now.Sub(shard.lastCleanupAt) >= observedModelPersistInterval {
			for accountID, state := range shard.values {
				if now.Sub(state.persistedAt) >= observedModelPersistInterval {
					delete(shard.values, accountID)
				}
			}
			shard.lastCleanupAt = now
		}
		state, exists := shard.values[id]
		localFresh := exists && state.model == model && observedModelStateIsFresh(now, state.persistedAt)
		if localFresh && (s.observedModelStore == nil || now.Sub(state.persistedAt) < observedModelLocalCacheTTL) {
			shard.Unlock()
			return nil, nil
		}
		shard.Unlock()
		if s.observedModelStore != nil {
			shared, ok, sharedErr := s.observedModelStore.GetObservedModelState(ctx, id)
			if sharedErr == nil && ok && shared.Model == model && observedModelStateIsFresh(now, shared.ObservedAt) {
				shard.Lock()
				shard.values[id] = observedModelState{model: model, persistedAt: now}
				shard.Unlock()
				return nil, nil
			}
		}
		updated := true
		if writer, ok := s.accounts.(repository.ObservedModelWriter); ok {
			var err error
			updated, err = writer.UpdateObservedModelIfNewer(ctx, id, model, now)
			if err != nil {
				return nil, err
			}
		} else if err := s.accounts.UpdateObservedModel(ctx, id, model, now); err != nil {
			return nil, err
		}
		if updated && s.observedModelStore != nil {
			_ = s.observedModelStore.SetObservedModelState(ctx, id, repository.ObservedModelState{Model: model, ObservedAt: now}, observedModelPersistInterval)
		}
		shard.Lock()
		current, exists := shard.values[id]
		if !exists || !current.persistedAt.After(now) {
			shard.values[id] = observedModelState{model: model, persistedAt: now}
		}
		shard.Unlock()
		return nil, nil
	})
	return err
}

func (s *Service) observedModelShard(id uint64) *observedModelShard {
	return &s.observedModelShards[id%observedModelLockShards]
}

func observedModelStateIsFresh(now, persistedAt time.Time) bool {
	elapsed := now.Sub(persistedAt)
	return elapsed >= 0 && elapsed < observedModelPersistInterval
}
