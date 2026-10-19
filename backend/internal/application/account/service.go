package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/resultcache"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

var (
	ErrInvalidFilter       = errors.New("账号筛选条件无效")
	ErrInvalidInput        = errors.New("账号参数无效")
	ErrInvalidImport       = errors.New("账号凭据格式无效")
	ErrImportLimit         = errors.New("导入账号数量超过限制")
	ErrExportLimit         = errors.New("导出账号数量超过限制")
	ErrNotFound            = errors.New("账号不存在")
	ErrUnsupported         = errors.New("账号来源不支持该操作")
	ErrConversionBusy      = errors.New("账号正在转换为 Grok Build")
	ErrConflict            = errors.New("账号操作存在冲突")
	ErrAccountPoolMismatch = errors.New("批量操作包含不属于当前号池的账号")
)

const (
	// estimatedFreeTokenLimit is only a fallback until an upstream exhaustion
	// response supplies the account-specific actual/limit pair.
	estimatedFreeTokenLimit         int64         = 500_000
	freeUsageWindow                 time.Duration = 24 * time.Hour
	forcedRefreshMinInterval        time.Duration = 30 * time.Second
	paidProbeRetryInterval          time.Duration = 15 * time.Minute
	credentialRefreshAdvance        time.Duration = 3 * time.Minute
	credentialRefreshSafetyPoll     time.Duration = time.Minute
	credentialRefreshTimeout        time.Duration = 30 * time.Second
	credentialRefreshStateTTL       time.Duration = 5 * time.Second
	credentialStateWriteTimeout     time.Duration = 5 * time.Second
	credentialConfigurationRetry    time.Duration = 30 * time.Minute
	credentialRefreshBatchSize                    = 100
	credentialUnclassifiedAuthLimit               = 5
	managedTaskWorkerCeiling                      = 50
	quotaRefreshQueueSize                         = 4096
	quotaRefreshTimeout                           = 30 * time.Second
	quotaRefreshDirtyTTL                          = 24 * time.Hour
	quotaRefreshPollInterval                      = 500 * time.Millisecond
	quotaRefreshSharedPoll                        = time.Second
	quotaRefreshBackoffBase                       = time.Second
	// A refresh that keeps failing backs off to 15 minutes; the 30-minute stale
	// catch-up still resyncs the account later.
	quotaRefreshBackoffMax         = 15 * time.Minute
	consoleQuotaRefreshMinInterval = 30 * time.Second
	// snapshotQuotaRefreshMinInterval spaces the request-driven syncs of a
	// snapshot provider (ChatGPT: conversation/init plus Codex usage).
	snapshotQuotaRefreshMinInterval               = 2 * time.Minute
	unknownRemoteQuotaProbeDelay    time.Duration = 5 * time.Minute
	consolePredictedQuotaProbeDelay time.Duration = 24 * time.Hour
	observedModelPersistInterval                  = 30 * time.Minute
	observedModelLocalCacheTTL                    = 5 * time.Second
	observedModelLockShards                       = 64
	maxCredentialExportAccounts                   = 10000
	maxCredentialImportAccounts                   = 10000
	credentialImportChunkSize                     = 100
	credentialImportPrepareWorkers                = 3
	maxQuotaResetAccounts                         = 10000
	quotaResetChunkSize                           = 500
	maxBatchUpdateAccounts                        = 10000
	maxBuildConversionAccounts                    = 1000
	maxWebConsoleSyncAccounts                     = 1000
	accountTaskBatchSize                          = 1000
	buildBotFlagCacheTTL            time.Duration = 30 * time.Second
	linkedDeleteRuntimeCleanupLimit               = 3 * time.Second
	// buildDetectModel 管理端「检测账号」固定使用的 Grok Build 模型。
	buildDetectModel               = "grok-4.5"
	buildDetectQuotaRecoveryPause  = 24 * time.Hour
	buildDetectModelDeniedCooldown = 5 * time.Minute
	// buildDetectPrompt 探测请求正文，仅用于验证凭据与上游可用性。
	buildDetectPrompt = "hello,test"
)

// BatchProgressObserver 在单个账号任务结束后报告批次完成数。
type BatchProgressObserver func(completed, total int) error

// Service 负责 OAuth 账号接入、刷新、额度和持久化生命周期。
type Service struct {
	accounts            repository.AccountRepository
	audits              repository.AuditRepository
	deviceSessions      repository.DeviceSessionRepository
	sticky              repository.StickySessionRepository
	refreshLock         repository.DistributedLock
	concurrency         repository.ConcurrencyLimiter
	quotaQueue          repository.QuotaRecoveryQueue
	providers           *provider.Registry
	cipher              *security.Cipher
	billingSyncs        singleflight.Group
	quotaSyncs          singleflight.Group
	identitySyncs       singleflight.Group
	observedModelWrites singleflight.Group
	observedModelStore  repository.ObservedModelStateRepository
	observedModelShards [observedModelLockShards]observedModelShard
	quotaRefresh        quotaRefreshQueue
	renewer             credentialRenewer
	conversionPool      *batch.Pool
	syncPool            *batch.Pool
	refreshPool         *batch.Pool
	// detectPool 专用于管理端「检测账号」，与额度同步/续期隔离，默认并发 32。
	detectPool             *batch.Pool
	credentialRefreshWake  chan struct{}
	autoCleanMu            sync.RWMutex
	autoClean              AutoCleanConfig
	autoCleanRevision      uint64
	autoCleanWake          chan struct{}
	excludeBuildBotFlagged bool
	buildBotFlagCache      *resultcache.Cache[string, []uint64]
	logger                 *slog.Logger
	now                    func() time.Time
}

func (s *Service) SetQuotaRecoveryQueue(queue repository.QuotaRecoveryQueue) {
	s.quotaQueue = queue
}

func (s *Service) SetQuotaRefreshCoordinator(value repository.QuotaRefreshCoordinator) {
	s.quotaRefresh.coordinator = value
}

// SetConcurrencyLimiter 让账号维护任务读取与推理路由相同的活动租约。
func (s *Service) SetConcurrencyLimiter(value repository.ConcurrencyLimiter) {
	s.concurrency = value
}

// SetObservedModelStore enables best-effort cross-instance duplicate suppression.
func (s *Service) SetObservedModelStore(value repository.ObservedModelStateRepository) {
	s.observedModelStore = value
}

func NewService(accounts repository.AccountRepository, audits repository.AuditRepository, deviceSessions repository.DeviceSessionRepository, sticky repository.StickySessionRepository, providers *provider.Registry, cipher *security.Cipher, refreshLock repository.DistributedLock) *Service {
	s := &Service{
		accounts: accounts, audits: audits, deviceSessions: deviceSessions, sticky: sticky,
		providers: providers, cipher: cipher, refreshLock: refreshLock,
		credentialRefreshWake: make(chan struct{}, 1),
		autoClean: AutoCleanConfig{
			Enabled: false, Interval: 10 * time.Minute, MinAge: time.Hour, IncludeDisabled: false,
		},
		autoCleanWake:     make(chan struct{}, 1),
		buildBotFlagCache: resultcache.New[string, []uint64](1, buildBotFlagCacheTTL),
		conversionPool:    batch.NewPool(25), syncPool: batch.NewPool(25), refreshPool: batch.NewPool(25), detectPool: batch.NewPool(32),
		logger: slog.Default(),
		now:    func() time.Time { return time.Now().UTC() },
	}
	// Read s.now on every call so a replaced clock reaches the components too.
	clock := func() time.Time { return s.now() }
	s.quotaRefresh = quotaRefreshQueue{
		states:   make(map[string]*quotaRefreshState),
		requests: make(chan quotaRefreshRequest, quotaRefreshQueueSize),
		wake:     make(chan struct{}, 1),
		accounts: accounts, lock: refreshLock,
		syncMode: s.syncQueuedQuota, isSnapshotMode: s.snapshotQuotaMode, isDeclaredMode: s.isDeclaredQuotaMode,
		logger: s.logger, now: clock,
	}
	s.renewer = credentialRenewer{
		lastRefreshAt: make(map[uint64]time.Time),
		accounts:      accounts, lock: refreshLock,
		markReauthRequired: s.MarkReauthRequired, invalidateBotFlags: s.invalidateBuildBotFlagCache, wakeScheduler: s.WakeCredentialRefresh,
		logger: s.logger, now: clock,
	}
	if providers != nil { // a nil registry must stay a nil interface
		s.renewer.providers = providers
	}
	return s
}

func (s *Service) SetBulkPool(pool *batch.Pool) {
	if pool != nil {
		s.conversionPool, s.syncPool, s.refreshPool = pool, pool, pool
	}
}

// SetTaskPools 为转换、同步和凭据刷新绑定独立分类并发池。
func (s *Service) SetTaskPools(conversion, syncPool, refresh *batch.Pool) {
	if conversion != nil {
		s.conversionPool = conversion
	}
	if syncPool != nil {
		s.syncPool = syncPool
	}
	if refresh != nil {
		s.refreshPool = refresh
	}
}

// SetDetectPool 绑定管理端「检测账号」专用并发池；nil 时保留现有池。
func (s *Service) SetDetectPool(pool *batch.Pool) {
	if pool != nil {
		s.detectPool = pool
	}
}

func (s *Service) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
		s.quotaRefresh.logger = logger
		s.renewer.logger = logger
	}
}

// ProviderDefinition 向账号同步编排层暴露只读生命周期策略，不泄露具体 Adapter。
func (s *Service) ProviderDefinition(value accountdomain.Provider) (provider.Definition, bool) {
	if s.providers == nil {
		return provider.Definition{}, false
	}
	return s.providers.Definition(value)
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func (s *Service) runAccountBatch(ctx context.Context, operation string, ids []uint64, pool *batch.Pool, progress BatchProgressObserver, work func(context.Context, uint64) error) (int, int, error) {
	if progress != nil {
		if err := progress(0, len(ids)); err != nil {
			return 0, 0, err
		}
	}
	var progressMu sync.Mutex
	var progressErr error
	completed := 0
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results, summary, err := batch.MapObserved(runCtx, ids, batch.Options{Workers: pool.Limit(), Pool: pool}, func(workCtx context.Context, id uint64) (struct{}, error) {
		return struct{}{}, work(workCtx, id)
	}, func(_ int, _ batch.Result[struct{}]) {
		progressMu.Lock()
		defer progressMu.Unlock()
		completed++
		if progress != nil {
			if notifyErr := progress(completed, len(ids)); notifyErr != nil && progressErr == nil {
				progressErr = notifyErr
				cancel()
			}
		}
	})
	for index, result := range results {
		var panicErr *batch.PanicError
		if errors.As(result.Err, &panicErr) {
			s.logger.Error("account_bulk_task_panicked", "operation", operation, "account_id", ids[index], "error", panicErr, "stack", string(panicErr.Stack))
		}
	}
	s.logBatchSummary(operation, pool, summary, err)
	return summary.Succeeded, summary.Failed, errors.Join(err, progressErr)
}

func (s *Service) logBatchSummary(operation string, pool *batch.Pool, summary batch.Summary, err error) {
	snapshot := pool.Snapshot()
	s.logger.Info("account_bulk_completed", "operation", operation, "total", summary.Total, "submitted", summary.Submitted, "succeeded", summary.Succeeded, "failed", summary.Failed, "panicked", summary.Panicked, "duration_ms", summary.Duration.Milliseconds(), "canceled", summary.Canceled, "pool_limit", snapshot.Limit, "pool_active", snapshot.Active, "pool_queued", snapshot.Queued, "pool_peak", snapshot.Peak, "error", err)
}

func normalizePage(page, pageSize int) (int, int) {
	return repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
}

func normalizeBatchIDs(ids []uint64) ([]uint64, error) {
	return normalizeIDs(ids, repository.MaxPageSize)
}

func normalizeIDs(ids []uint64, limit int) ([]uint64, error) {
	if len(ids) == 0 {
		return nil, invalidInput("至少选择一个账号")
	}
	if len(ids) > limit {
		return nil, invalidInput(fmt.Sprintf("单次最多处理 %d 个账号", limit))
	}
	seen := make(map[uint64]struct{}, len(ids))
	result := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, invalidInput("账号 ID 无效")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result, nil
}

// invalidInput 为可安全返回给管理端的账号参数错误附加稳定语义。
func invalidInput(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, message)
}

func mapRepositoryError(err error) error {
	if errors.Is(err, repository.ErrAccountPoolMismatch) {
		return ErrAccountPoolMismatch
	}
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, repository.ErrConflict) {
		return fmt.Errorf("%w: %s", ErrConflict, strings.TrimPrefix(err.Error(), repository.ErrConflict.Error()+": "))
	}
	return err
}
