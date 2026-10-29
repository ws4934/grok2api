package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
)

type BuildConversionStrategy string

const (
	BuildConversionAll     BuildConversionStrategy = "all"
	BuildConversionMissing BuildConversionStrategy = "missing"
)

type WebConsoleSyncStrategy string

const (
	WebConsoleSyncAll     WebConsoleSyncStrategy = "all"
	WebConsoleSyncMissing WebConsoleSyncStrategy = "missing"
)

type BuildConversionResult struct {
	Created         int
	Linked          int
	Skipped         int
	Failed          int
	BuildAccountIDs []uint64
}

// SyncWebAccountsToConsoleWithProgress 使用 Web 账号的同一份 SSO 创建或更新 Console 账号。
func (s *Service) SyncWebAccountsToConsoleWithProgress(ctx context.Context, ids []uint64, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	return s.SyncWebAccountsToConsoleWithStrategy(ctx, ids, WebConsoleSyncAll, observer, progress)
}

func (s *Service) SyncWebAccountsToConsoleWithStrategy(ctx context.Context, ids []uint64, strategy WebConsoleSyncStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	if strategy != WebConsoleSyncAll && strategy != WebConsoleSyncMissing {
		return ImportResult{}, invalidInput("Grok Web 到 Console 同步策略无效")
	}
	ids, err := normalizeIDs(ids, maxWebConsoleSyncAccounts)
	if err != nil {
		return ImportResult{}, err
	}
	if strategy == WebConsoleSyncMissing {
		values, err := s.accounts.ListMissingConsoleSyncAccounts(ctx, ids)
		if err != nil {
			return ImportResult{}, mapRepositoryError(err)
		}
		result, err := s.syncWebCredentialsToConsole(ctx, values, observer, progress)
		result.Skipped = len(ids) - len(values)
		return result, err
	}
	values := make([]accountdomain.Credential, 0, len(ids))
	for _, id := range ids {
		value, getErr := s.accounts.Get(ctx, id)
		if getErr != nil {
			return ImportResult{}, mapRepositoryError(getErr)
		}
		values = append(values, value)
	}
	return s.syncWebCredentialsToConsole(ctx, values, observer, progress)
}

// SyncAllWebAccountsToConsoleWithProgress 同步完整 Web 号池，避免前端分页遗漏账号。
func (s *Service) SyncAllWebAccountsToConsoleWithProgress(ctx context.Context, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	return s.SyncAllWebAccountsToConsoleWithStrategy(ctx, WebConsoleSyncAll, observer, progress)
}

func (s *Service) SyncAllWebAccountsToConsoleWithStrategy(ctx context.Context, strategy WebConsoleSyncStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	if strategy != WebConsoleSyncAll && strategy != WebConsoleSyncMissing {
		return ImportResult{}, invalidInput("Grok Web 到 Console 同步策略无效")
	}
	batchSize := accountTaskBatchSize
	result := ImportResult{AccountIDs: make([]uint64, 0)}
	var afterID uint64
	completed := 0
	total := 0
	initialized := false
	for {
		var (
			values  []accountdomain.Credential
			count   int64
			skipped int64
			err     error
		)
		if strategy == WebConsoleSyncMissing {
			values, count, skipped, err = s.accounts.ListMissingConsoleSyncBatch(ctx, afterID, batchSize)
		} else {
			values, count, err = s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderWeb, afterID, batchSize)
		}
		if err != nil {
			return result, err
		}
		if !initialized {
			total = int(count)
			result.Skipped = int(skipped)
			initialized = true
			if progress != nil {
				if err := progress(0, total); err != nil {
					return result, err
				}
			}
		}
		if len(values) == 0 {
			return result, nil
		}
		current, err := s.syncWebCredentialsToConsole(ctx, values, observer, offsetBatchProgress(progress, completed, total))
		result.Created += current.Created
		result.Updated += current.Updated
		result.AccountIDs = append(result.AccountIDs, current.AccountIDs...)
		if err != nil {
			return result, err
		}
		completed += len(values)
		afterID = values[len(values)-1].ID
		if len(values) < batchSize {
			return result, nil
		}
	}
}

func (s *Service) syncWebCredentialsToConsole(ctx context.Context, values []accountdomain.Credential, observer ImportedAccountObserver, progress BatchProgressObserver) (ImportResult, error) {
	adapter, ok := s.providers.CredentialCodec(accountdomain.ProviderConsole)
	if !ok {
		return ImportResult{}, fmt.Errorf("Grok Console Provider 未注册")
	}
	seeds := make([]provider.CredentialSeed, 0, len(values))
	for _, value := range values {
		if value.Provider != accountdomain.ProviderWeb || value.AuthType != accountdomain.AuthTypeSSO {
			return ImportResult{}, fmt.Errorf("%w: 仅 Grok Web SSO 账号支持同步到 Console", ErrUnsupported)
		}
		token, err := s.cipher.Decrypt(value.EncryptedAccessToken)
		if err != nil {
			return ImportResult{}, fmt.Errorf("解密 Grok Web SSO: %w", err)
		}
		// 非法 UTF-8 会被 json.Marshal 静默改写为 U+FFFD，显式拒绝优于静默改动（不应回显 token 内容）。
		if !utf8.ValidString(token) {
			return ImportResult{}, fmt.Errorf("解密 Grok Web SSO: 凭据不是合法 UTF-8")
		}
		// 内部调用固定走 JSON 对象路径，避免 plain token 被格式嗅探（如「[」JSON 保留前缀）误判。
		payload, err := json.Marshal(map[string]string{"sso_token": token})
		if err != nil {
			return ImportResult{}, fmt.Errorf("生成 Grok Console SSO 凭据: %w", err)
		}
		parsed, err := adapter.ParseImportedCredentials(payload)
		if err != nil {
			return ImportResult{}, fmt.Errorf("生成 Grok Console SSO 凭据: %w", err)
		}
		if len(parsed) != 1 {
			return ImportResult{}, fmt.Errorf("生成 Grok Console SSO 凭据: 预期 1 个账号，实际 %d 个", len(parsed))
		}
		seed := parsed[0]
		seed.Provider = accountdomain.ProviderConsole
		seed.AuthType = accountdomain.AuthTypeSSO
		seed.Name = webConsoleAccountName(value.Name, value.Email, seed.Name)
		seed.Email = strings.TrimSpace(value.Email)
		if strings.TrimSpace(value.EncryptedCloudflareCookie) != "" {
			cookies, decryptErr := s.cipher.Decrypt(value.EncryptedCloudflareCookie)
			if decryptErr != nil {
				return ImportResult{}, fmt.Errorf("解密 Grok Web Cloudflare Cookie: %w", decryptErr)
			}
			seed.CloudflareCookies = cookies
		}
		seeds = append(seeds, seed)
	}
	return s.persistImportedSeeds(ctx, seeds, observer, progress)
}

func webConsoleAccountName(webName, email, fallback string) string {
	name := strings.TrimSpace(webName)
	if suffix, ok := strings.CutPrefix(name, "Grok Web "); ok {
		name = "Grok Console " + suffix
	}
	if (name == "" || name == fallback) && strings.TrimSpace(email) != "" {
		return strings.TrimSpace(email)
	}
	if name == "" {
		return fallback
	}
	return name
}

// ConvertWebAccountsToBuild 使用 Web SSO 自动完成 xAI Device Flow，并建立唯一的 Web/Build 账号关联。
func (s *Service) ConvertWebAccountsToBuild(ctx context.Context, ids []uint64) (BuildConversionResult, error) {
	return s.ConvertWebAccountsToBuildWithStrategy(ctx, ids, BuildConversionMissing, nil, nil)
}

func (s *Service) ConvertWebAccountsToBuildWithObserver(ctx context.Context, ids []uint64, observer ImportedAccountObserver) (BuildConversionResult, error) {
	return s.ConvertWebAccountsToBuildWithStrategy(ctx, ids, BuildConversionMissing, observer, nil)
}

// ConvertWebAccountsToBuildWithProgress 转换指定账号，并向调用方报告真实完成数。
func (s *Service) ConvertWebAccountsToBuildWithProgress(ctx context.Context, ids []uint64, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error) {
	return s.ConvertWebAccountsToBuildWithStrategy(ctx, ids, BuildConversionMissing, observer, progress)
}

func (s *Service) ConvertWebAccountsToBuildWithStrategy(ctx context.Context, ids []uint64, strategy BuildConversionStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error) {
	if strategy != BuildConversionAll && strategy != BuildConversionMissing {
		return BuildConversionResult{}, invalidInput("Grok Web 到 Build 转换策略无效")
	}
	ids, err := normalizeIDs(ids, maxBuildConversionAccounts)
	if err != nil {
		return BuildConversionResult{}, err
	}
	prefilteredSkipped := 0
	if strategy == BuildConversionMissing {
		candidates, err := s.accounts.FilterMissingBuildConversionIDs(ctx, ids)
		if err != nil {
			return BuildConversionResult{}, mapRepositoryError(err)
		}
		prefilteredSkipped = len(ids) - len(candidates)
		ids = candidates
	}
	result, err := s.convertWebAccountsToBuild(ctx, ids, strategy, observer, progress)
	result.Skipped += prefilteredSkipped
	return result, err
}

// ConvertAllWebAccountsToBuild 转换全部尚未建立 Build 关联的 Grok Web 账号。
func (s *Service) ConvertAllWebAccountsToBuild(ctx context.Context) (BuildConversionResult, error) {
	return s.ConvertAllWebAccountsToBuildWithStrategy(ctx, BuildConversionMissing, nil, nil)
}

func (s *Service) ConvertAllWebAccountsToBuildWithObserver(ctx context.Context, observer ImportedAccountObserver) (BuildConversionResult, error) {
	return s.ConvertAllWebAccountsToBuildWithStrategy(ctx, BuildConversionMissing, observer, nil)
}

// ConvertAllWebAccountsToBuildWithProgress 转换完整未关联号池，并向调用方报告真实完成数。
func (s *Service) ConvertAllWebAccountsToBuildWithProgress(ctx context.Context, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error) {
	return s.ConvertAllWebAccountsToBuildWithStrategy(ctx, BuildConversionMissing, observer, progress)
}

func (s *Service) ConvertAllWebAccountsToBuildWithStrategy(ctx context.Context, strategy BuildConversionStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error) {
	if strategy != BuildConversionAll && strategy != BuildConversionMissing {
		return BuildConversionResult{}, invalidInput("Grok Web 到 Build 转换策略无效")
	}
	batchSize := accountTaskBatchSize
	result := BuildConversionResult{BuildAccountIDs: make([]uint64, 0)}
	seenBuildIDs := make(map[uint64]struct{})
	var observed sync.Map
	batchObserver := observer
	if observer != nil {
		batchObserver = func(accountID uint64) error {
			if _, loaded := observed.LoadOrStore(accountID, struct{}{}); loaded {
				return nil
			}
			return observer(accountID)
		}
	}
	var afterID uint64
	completed := 0
	total := 0
	initialized := false
	for {
		var (
			ids   []uint64
			count int64
			err   error
		)
		if strategy == BuildConversionMissing {
			ids, count, err = s.accounts.ListUnlinkedWebAccountIDs(ctx, afterID, batchSize)
		} else {
			var values []accountdomain.Credential
			values, count, err = s.accounts.ListProviderAccountBatch(ctx, accountdomain.ProviderWeb, afterID, batchSize)
			ids = make([]uint64, 0, len(values))
			for _, value := range values {
				ids = append(ids, value.ID)
			}
		}
		if err != nil {
			return result, err
		}
		if !initialized {
			total = int(count)
			initialized = true
			if progress != nil {
				if err := progress(0, total); err != nil {
					return result, err
				}
			}
		}
		if len(ids) == 0 {
			return result, nil
		}
		current, err := s.convertWebAccountsToBuild(ctx, ids, strategy, batchObserver, offsetBatchProgress(progress, completed, total))
		result.Created += current.Created
		result.Linked += current.Linked
		result.Skipped += current.Skipped
		result.Failed += current.Failed
		for _, buildID := range current.BuildAccountIDs {
			if _, exists := seenBuildIDs[buildID]; exists {
				continue
			}
			seenBuildIDs[buildID] = struct{}{}
			result.BuildAccountIDs = append(result.BuildAccountIDs, buildID)
		}
		if err != nil {
			return result, err
		}
		completed += len(ids)
		afterID = ids[len(ids)-1]
		if len(ids) < batchSize {
			return result, nil
		}
	}
}

func offsetBatchProgress(progress BatchProgressObserver, offset, total int) BatchProgressObserver {
	if progress == nil {
		return nil
	}
	return func(completed, _ int) error {
		if completed == 0 {
			return nil
		}
		return progress(offset+completed, total)
	}
}

func (s *Service) convertWebAccountsToBuild(ctx context.Context, ids []uint64, strategy BuildConversionStrategy, observer ImportedAccountObserver, progress BatchProgressObserver) (BuildConversionResult, error) {
	if progress != nil {
		if err := progress(0, len(ids)); err != nil {
			return BuildConversionResult{}, err
		}
	}
	type outcome struct {
		accountID uint64
		buildID   uint64
		created   bool
		skipped   bool
		err       error
	}
	var observed sync.Map
	var observerMu sync.Mutex
	var observerErr error
	completed := 0
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results, summary, runErr := batch.MapObserved(runCtx, ids, batch.Options{Workers: s.conversionPool.Limit(), Pool: s.conversionPool}, func(workCtx context.Context, id uint64) (outcome, error) {
		buildID, created, skipped, convertErr := s.convertWebAccountToBuild(workCtx, id, strategy)
		return outcome{accountID: id, buildID: buildID, created: created, skipped: skipped, err: convertErr}, nil
	}, func(_ int, execution batch.Result[outcome]) {
		observerMu.Lock()
		defer observerMu.Unlock()
		defer func() {
			completed++
			if progress != nil {
				if err := progress(completed, len(ids)); err != nil && observerErr == nil {
					observerErr = err
					cancel()
				}
			}
		}()
		item := execution.Value
		if execution.Err != nil || item.err != nil || item.skipped || observer == nil {
			return
		}
		if _, loaded := observed.LoadOrStore(item.buildID, struct{}{}); loaded {
			return
		}
		if err := observer(item.buildID); err != nil {
			if observerErr == nil {
				observerErr = err
				cancel()
			}
		}
	})
	s.logBatchSummary("web_to_build", s.conversionPool, summary, runErr)
	result := BuildConversionResult{BuildAccountIDs: make([]uint64, 0, len(ids))}
	seen := make(map[uint64]struct{}, len(ids))
	for index, execution := range results {
		item := execution.Value
		if execution.Err != nil {
			item.accountID = ids[index]
			item.err = execution.Err
		}
		if item.err != nil {
			result.Failed++
			s.logger.Warn("web_account_build_conversion_failed", "account_id", item.accountID, "error", item.err)
			continue
		}
		if item.skipped {
			result.Skipped++
			continue
		}
		if item.created {
			result.Created++
		} else {
			result.Linked++
		}
		if _, ok := seen[item.buildID]; !ok {
			seen[item.buildID] = struct{}{}
			result.BuildAccountIDs = append(result.BuildAccountIDs, item.buildID)
		}
	}
	if runErr != nil {
		return result, runErr
	}
	if observerErr != nil {
		return result, observerErr
	}
	return result, nil
}

func (s *Service) convertWebAccountToBuild(ctx context.Context, id uint64, strategy BuildConversionStrategy) (uint64, bool, bool, error) {
	value, err := s.accounts.Get(ctx, id)
	if err != nil {
		return 0, false, false, mapRepositoryError(err)
	}
	if value.Provider != accountdomain.ProviderWeb || value.AuthType != accountdomain.AuthTypeSSO {
		return 0, false, false, ErrUnsupported
	}
	if value.LinkedAccountID != 0 && strategy == BuildConversionMissing {
		return value.LinkedAccountID, false, true, nil
	}
	release, acquired, err := s.refreshLock.Acquire(ctx, "web-build-conversion:"+strconv.FormatUint(id, 10), 2*time.Minute)
	if err != nil {
		return 0, false, false, err
	}
	if !acquired {
		return 0, false, false, ErrConversionBusy
	}
	defer release()
	value, err = s.accounts.Get(ctx, id)
	if err != nil {
		return 0, false, false, mapRepositoryError(err)
	}
	if value.LinkedAccountID != 0 && strategy == BuildConversionMissing {
		return value.LinkedAccountID, false, true, nil
	}
	linkedBuildSourceKey := ""
	if value.LinkedAccountID != 0 {
		linkedBuild, getErr := s.accounts.Get(ctx, value.LinkedAccountID)
		if getErr != nil {
			return 0, false, false, mapRepositoryError(getErr)
		}
		if linkedBuild.Provider != accountdomain.ProviderBuild || strings.TrimSpace(linkedBuild.SourceKey) == "" {
			return 0, false, false, fmt.Errorf("已关联 Grok Build 账号身份无效")
		}
		linkedBuildSourceKey = linkedBuild.SourceKey
	}
	converter, ok := s.providers.BuildConverter(accountdomain.ProviderWeb)
	if !ok {
		return 0, false, false, fmt.Errorf("Grok Web SSO 转换能力未注册")
	}
	seed, err := converter.ConvertToBuild(ctx, value)
	if err != nil {
		if errors.Is(err, provider.ErrUnauthorized) {
			err = errors.Join(err, s.markSSOCredentialRejected(ctx, value, "Grok Web SSO credential rejected"))
		}
		return 0, false, false, err
	}
	seed.Provider = accountdomain.ProviderBuild
	seed.AuthType = accountdomain.AuthTypeOAuth
	if linkedBuildSourceKey != "" {
		seed.SourceKey = linkedBuildSourceKey
	}
	buildAccount, created, err := s.persistSeed(ctx, seed)
	if err != nil {
		return 0, false, false, err
	}
	if value.LinkedAccountID != 0 && buildAccount.ID != value.LinkedAccountID {
		return 0, false, false, fmt.Errorf("重新转换后的 Grok Build 账号身份不一致")
	}
	if err := s.accounts.LinkWebToBuild(ctx, id, buildAccount.ID); err != nil {
		return 0, false, false, mapRepositoryError(err)
	}
	return buildAccount.ID, created, false, nil
}
