package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type ImportResult struct {
	Created    int
	Updated    int
	Skipped    int
	Failed     int
	AccountIDs []uint64
}

type ImportedAccountObserver func(accountID uint64) error

// ImportCredentialDocumentsWithProgress 使用 Provider 自身的凭据编解码器合并解析多个文件，并作为一个批次写入和同步。
func (s *Service) ImportCredentialDocumentsWithProgress(ctx context.Context, providerValue accountdomain.Provider, documents [][]byte, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	adapter, ok := s.providers.CredentialCodec(providerValue)
	if !ok {
		return ImportResult{}, fmt.Errorf("%w: Provider %s 不支持凭据导入", ErrUnsupported, providerValue)
	}
	if len(documents) == 0 {
		return ImportResult{}, fmt.Errorf("%w: 没有可导入的账号文件", ErrInvalidImport)
	}
	seeds := make([]provider.CredentialSeed, 0)
	seen := make(map[string]struct{})
	parsedAccounts := 0
	skipped := 0
	for index, document := range documents {
		values, err := adapter.ParseImportedCredentials(document)
		if err != nil {
			if errors.Is(err, provider.ErrCredentialLimit) {
				return ImportResult{}, fmt.Errorf("%w: 单次最多导入 %d 个账号", ErrImportLimit, maxCredentialImportAccounts)
			}
			return ImportResult{}, fmt.Errorf("%w: 第 %d 个文件: %v", ErrInvalidImport, index+1, err)
		}
		parsedAccounts += len(values)
		if parsedAccounts > maxCredentialImportAccounts {
			return ImportResult{}, fmt.Errorf("%w: 单次最多导入 %d 个账号", ErrImportLimit, maxCredentialImportAccounts)
		}
		for _, value := range values {
			if value.Provider == "" {
				value.Provider = providerValue
			}
			if value.Provider != providerValue {
				return ImportResult{}, fmt.Errorf("%w: 第 %d 个文件包含其他 Provider 的账号", ErrInvalidImport, index+1)
			}
			if value.SourceKey != "" {
				key := string(value.Provider) + "\x00" + value.SourceKey
				if _, exists := seen[key]; exists {
					skipped++
					continue
				}
				seen[key] = struct{}{}
			}
			seeds = append(seeds, value)
		}
	}
	var result ImportResult
	var err error
	if preparer, ok := adapter.(provider.CredentialImportPreparer); ok {
		result, err = s.persistPreparedImportedSeeds(ctx, seeds, preparer, observer, progress)
	} else {
		result, err = s.persistImportedSeeds(ctx, seeds, observer, progress)
	}
	result.Skipped += skipped
	return result, err
}

func (s *Service) persistPreparedImportedSeeds(ctx context.Context, seeds []provider.CredentialSeed, preparer provider.CredentialImportPreparer, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	result := ImportResult{AccountIDs: make([]uint64, 0, len(seeds))}
	if progress != nil {
		if err := progress(0, len(seeds)); err != nil {
			return result, err
		}
	}
	prepareCtx, cancelPrepare := context.WithCancel(ctx)
	defer cancelPrepare()
	var (
		mu        sync.Mutex
		firstErr  error
		completed int
		persisted bool
		seen      = make(map[string]struct{}, len(seeds))
	)
	// Every seed goes through the preparer: it may exchange tokens or resolve identity.
	prepare := func(ctx context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
		// A re-import exchanges through the stored account's egress binding.
		if seed.SourceKey != "" {
			if existing, err := s.accounts.FindBySourceKey(ctx, seed.Provider, seed.SourceKey); err == nil {
				ctx = infraegress.WithExistingAccount(ctx, existing)
			}
		}
		return preparer.PrepareImportedCredential(ctx, seed)
	}
	_, batchErr := batch.ForEachObserved(prepareCtx, seeds, batch.Options{Workers: credentialImportPrepareWorkers}, prepare, func(index int, item batch.Result[provider.CredentialSeed]) {
		mu.Lock()
		defer mu.Unlock()
		completed++
		if !item.Completed || item.Err != nil {
			result.Failed++
			if item.Err != nil {
				s.logger.Warn("account_import_prepare_failed", "index", index+1, "error", item.Err)
			}
			reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
			return
		}
		seed := item.Value
		if seed.SourceKey != "" {
			key := string(seed.Provider) + "\x00" + seed.SourceKey
			if _, exists := seen[key]; exists {
				result.Skipped++
				reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
				return
			}
			seen[key] = struct{}{}
		}

		// OAuth providers may invalidate the submitted refresh token as soon as
		// they return its replacement. Persist that replacement before any
		// request-scoped observer or progress callback can abort the import.
		persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), credentialStateWriteTimeout)
		stored, err := s.persistImportedSeed(persistCtx, seed)
		cancelPersist()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			cancelPrepare()
			return
		}
		persisted = true
		result.AccountIDs = append(result.AccountIDs, stored.ID)
		if stored.Created {
			result.Created++
		} else {
			result.Updated++
		}
		if firstErr == nil && observer != nil {
			if err := observer(stored.ID); err != nil {
				firstErr = err
				cancelPrepare()
			}
		}
		reportCredentialImportProgress(progress, completed, len(seeds), &firstErr, cancelPrepare)
	})
	if persisted {
		s.WakeCredentialRefresh()
	}
	return result, errors.Join(firstErr, batchErr)
}

func reportCredentialImportProgress(progress BatchProgressObserver, completed, total int, firstErr *error, cancel context.CancelFunc) {
	if progress == nil || *firstErr != nil {
		return
	}
	if err := progress(completed, total); err != nil {
		*firstErr = err
		cancel()
	}
}

func (s *Service) persistImportedSeed(ctx context.Context, seed provider.CredentialSeed) (repository.AccountUpsertResult, error) {
	value, err := s.credentialFromSeed(seed)
	if err != nil {
		return repository.AccountUpsertResult{}, err
	}
	stored, err := s.accounts.UpsertManyByIdentity(ctx, []accountdomain.Credential{value})
	if err != nil {
		return repository.AccountUpsertResult{}, err
	}
	if len(stored) != 1 {
		return repository.AccountUpsertResult{}, fmt.Errorf("导入账号持久化结果数量无效: %d", len(stored))
	}
	s.reconcileProviderLinksBestEffort(ctx, stored[0].ID)
	return stored[0], nil
}

func (s *Service) persistImportedSeeds(ctx context.Context, seeds []provider.CredentialSeed, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	return s.persistImportedSeedsFromProgress(ctx, seeds, observer, progress, 0, len(seeds), true)
}

func (s *Service) persistImportedSeedsFromProgress(ctx context.Context, seeds []provider.CredentialSeed, observer ImportedAccountObserver, progress BatchProgressObserver, completed, total int, reportInitial bool) (ImportResult, error) {
	result := ImportResult{AccountIDs: make([]uint64, 0, len(seeds))}
	if progress != nil && reportInitial {
		if err := progress(completed, total); err != nil {
			return ImportResult{}, err
		}
	}
	for start := 0; start < len(seeds); start += credentialImportChunkSize {
		end := min(start+credentialImportChunkSize, len(seeds))
		values := make([]accountdomain.Credential, 0, end-start)
		for _, seed := range seeds[start:end] {
			value, err := s.credentialFromSeed(seed)
			if err != nil {
				return ImportResult{}, err
			}
			values = append(values, value)
		}
		stored, err := s.accounts.UpsertManyByIdentity(ctx, values)
		if err != nil {
			return ImportResult{}, err
		}
		for _, value := range stored {
			result.AccountIDs = append(result.AccountIDs, value.ID)
			s.reconcileProviderLinksBestEffort(ctx, value.ID)
			if observer != nil {
				if err := observer(value.ID); err != nil {
					return ImportResult{}, err
				}
			}
			completed++
			if progress != nil {
				if err := progress(completed, total); err != nil {
					return ImportResult{}, err
				}
			}
			if value.Created {
				result.Created++
			} else {
				result.Updated++
			}
		}
	}
	s.WakeCredentialRefresh()
	return result, nil
}

func (s *Service) persistSeed(ctx context.Context, seed provider.CredentialSeed) (accountdomain.Credential, bool, error) {
	value, err := s.credentialFromSeed(seed)
	if err != nil {
		return accountdomain.Credential{}, false, err
	}
	stored, created, err := s.accounts.UpsertByIdentity(ctx, value)
	if err == nil {
		s.invalidateBuildBotFlagCache()
		s.WakeCredentialRefresh()
	}
	return stored, created, err
}

// encryptCredentialState stores provider state as encrypted JSON; an empty map stores "".
func (s *Service) encryptCredentialState(state map[string]string) (string, error) {
	if len(state) == 0 {
		return "", nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return s.cipher.Encrypt(string(data))
}

func (s *Service) decryptCredentialState(value string) (map[string]string, error) {
	if value == "" {
		return nil, nil
	}
	plain, err := s.cipher.Decrypt(value)
	if err != nil {
		return nil, err
	}
	var state map[string]string
	if err := json.Unmarshal([]byte(plain), &state); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *Service) credentialFromSeed(seed provider.CredentialSeed) (accountdomain.Credential, error) {
	accessEncrypted, err := s.cipher.Encrypt(seed.AccessToken)
	if err != nil {
		return accountdomain.Credential{}, err
	}
	refreshEncrypted, err := s.cipher.Encrypt(seed.RefreshToken)
	if err != nil {
		return accountdomain.Credential{}, err
	}
	cloudflareEncrypted := ""
	if strings.TrimSpace(seed.CloudflareCookies) != "" {
		cookies := egressapp.SanitizeCloudflareCookies(seed.CloudflareCookies)
		if cookies == "" {
			return accountdomain.Credential{}, invalidInput("Cloudflare Cookie 中没有有效字段")
		}
		cloudflareEncrypted, err = s.cipher.Encrypt(cookies)
		if err != nil {
			return accountdomain.Credential{}, err
		}
	}
	sourceKey := seed.SourceKey
	if sourceKey == "" {
		sourceKey = "device:" + security.HashToken(seed.AccessToken)
	}
	stateEncrypted, err := s.encryptCredentialState(seed.State)
	if err != nil {
		return accountdomain.Credential{}, err
	}
	providerValue := seed.Provider
	if providerValue == "" {
		return accountdomain.Credential{}, fmt.Errorf("账号凭据缺少 Provider")
	}
	var definition provider.Definition
	registered := false
	if s.providers != nil {
		definition, registered = s.providers.Definition(providerValue)
	}
	authType := seed.AuthType
	if authType == "" {
		if !registered {
			return accountdomain.Credential{}, fmt.Errorf("Provider %s 未注册", providerValue)
		}
		authType = definition.Credential.AuthType
	}
	maxConcurrent := accountdomain.DefaultMaxConcurrent
	if definition.Profile.DefaultMaxConcurrent > 0 {
		maxConcurrent = definition.Profile.DefaultMaxConcurrent
	}
	value := accountdomain.Credential{Provider: providerValue, AuthType: authType, WebTier: seed.WebTier, Name: seed.Name, Email: seed.Email, UserID: seed.UserID, TeamID: seed.TeamID, SourceKey: sourceKey, OIDCClientID: seed.OIDCClientID, EncryptedAccessToken: accessEncrypted, EncryptedRefreshToken: refreshEncrypted, EncryptedCloudflareCookie: cloudflareEncrypted, EncryptedState: stateEncrypted, Plan: seed.Plan, ExpiresAt: seed.ExpiresAt, Enabled: true, AuthStatus: accountdomain.AuthStatusActive, Priority: accountdomain.DefaultPriority, MaxConcurrent: maxConcurrent, MinimumRemaining: accountdomain.DefaultMinimumRemaining, WebNSFWEnabledAt: seed.WebNSFWEnabledAt, WebTermsAcceptedAt: seed.WebTermsAcceptedAt, WebTermsAcceptedVersion: seed.WebTermsAcceptedVersion, WebBirthDateSetAt: seed.WebBirthDateSetAt}
	value.BuildBotFlagSource = s.credentialMetadata(value).BuildBotFlagSource
	if providerValue == accountdomain.ProviderWeb && strings.TrimSpace(seed.AccessToken) != "" {
		value.EgressIdentity = "sso_" + security.HashToken(seed.AccessToken)[:32]
	}
	return value, nil
}
