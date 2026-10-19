package account

import (
	"context"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const buildBotFlagCacheKey = "build-bot-flagged-account-ids"

type buildBotFlagIndexRepository interface {
	ListBuildBotFlaggedAccountIDs(ctx context.Context) ([]uint64, error)
	ListBuildBotFlagCredentialBatch(ctx context.Context, afterID uint64, limit int) ([]repository.BuildBotFlagCredential, error)
	UpdateBuildBotFlagSources(ctx context.Context, values []repository.BuildBotFlagSourceUpdate) error
	CountBuildBotFlagged(ctx context.Context) (int64, error)
	CountAvailableBuildBotFlagged(ctx context.Context, now time.Time) (int64, error)
}

func (s *Service) buildBotFlaggedAccountIDs(ctx context.Context) ([]uint64, error) {
	if s.buildBotFlagCache == nil {
		return s.loadBuildBotFlaggedAccountIDs(ctx)
	}
	return s.buildBotFlagCache.Load(ctx, buildBotFlagCacheKey, s.now(), func() ([]uint64, error) {
		return s.loadBuildBotFlaggedAccountIDs(ctx)
	})
}

// ListBuildBotFlaggedAccountIDs returns Build account IDs whose access-token claims
// mark bot_flag_source/bfs as 1 or 2. Used by routing to optionally exclude them.
func (s *Service) ListBuildBotFlaggedAccountIDs(ctx context.Context) ([]uint64, error) {
	return s.buildBotFlaggedAccountIDs(ctx)
}

// UpdateExcludeBuildBotFlaggedFromScheduling hot-updates whether bot-risk Build
// accounts are treated as non-schedulable in account summary available counts.
func (s *Service) UpdateExcludeBuildBotFlaggedFromScheduling(value bool) {
	s.autoCleanMu.Lock()
	s.excludeBuildBotFlagged = value
	s.autoCleanMu.Unlock()
}

func (s *Service) excludeBuildBotFlaggedFromSchedulingEnabled() bool {
	s.autoCleanMu.RLock()
	defer s.autoCleanMu.RUnlock()
	return s.excludeBuildBotFlagged
}

func (s *Service) loadBuildBotFlaggedAccountIDs(ctx context.Context) ([]uint64, error) {
	if indexed, ok := s.accounts.(buildBotFlagIndexRepository); ok {
		return indexed.ListBuildBotFlaggedAccountIDs(ctx)
	}
	const batchSize = 500
	result := make([]uint64, 0)
	var afterID uint64
	for {
		values, _, err := s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderBuild, afterID, batchSize)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			if s.credentialMetadata(value).BuildBotFlagged {
				result = append(result, value.ID)
			}
		}
		if len(values) < batchSize {
			return result, nil
		}
		afterID = values[len(values)-1].ID
	}
}

// RebuildBuildBotFlagIndex backfills persisted non-sensitive routing metadata
// before the gateway begins serving traffic. Subsequent imports and refreshes
// update the source atomically with the encrypted access token.
func (s *Service) RebuildBuildBotFlagIndex(ctx context.Context) error {
	indexed, ok := s.accounts.(buildBotFlagIndexRepository)
	if !ok {
		return nil
	}
	const batchSize = 500
	var afterID uint64
	for {
		values, err := indexed.ListBuildBotFlagCredentialBatch(ctx, afterID, batchSize)
		if err != nil {
			return err
		}
		updates := make([]repository.BuildBotFlagSourceUpdate, 0)
		for _, value := range values {
			credential := accountdomain.Credential{
				ID: value.AccountID, Provider: accountdomain.ProviderBuild, EncryptedAccessToken: value.EncryptedAccessToken,
			}
			metadata := s.credentialMetadata(credential)
			if !metadata.BuildBotFlagInspected {
				continue
			}
			source := metadata.BuildBotFlagSource
			if source != 1 && source != 2 {
				source = 0
			}
			if source != value.StoredSource {
				updates = append(updates, repository.BuildBotFlagSourceUpdate{
					AccountID: value.AccountID, ExpectedEncryptedAccessToken: value.EncryptedAccessToken, Source: source,
				})
			}
		}
		if err := indexed.UpdateBuildBotFlagSources(ctx, updates); err != nil {
			return err
		}
		if len(values) < batchSize {
			s.invalidateBuildBotFlagCache()
			return nil
		}
		afterID = values[len(values)-1].AccountID
	}
}

func (s *Service) invalidateBuildBotFlagCache() {
	if s.buildBotFlagCache != nil {
		s.buildBotFlagCache.Delete(buildBotFlagCacheKey)
	}
}
