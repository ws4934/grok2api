package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	"github.com/chenyme/grok2api/backend/internal/application/adminauth"
	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	dashboardapp "github.com/chenyme/grok2api/backend/internal/application/dashboard"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	invalidationapp "github.com/chenyme/grok2api/backend/internal/application/invalidation"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	"github.com/chenyme/grok2api/backend/internal/application/providercatalog"
	quotarecoveryapp "github.com/chenyme/grok2api/backend/internal/application/quotarecovery"
	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	updatecheckapp "github.com/chenyme/grok2api/backend/internal/application/updatecheck"
	"github.com/chenyme/grok2api/backend/internal/buildinfo"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	inframedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/adobe"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt"
	cliprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	consoleprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	infraqualityguard "github.com/chenyme/grok2api/backend/internal/infra/qualityguard"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/pkg/reasoningreplay"
	"github.com/chenyme/grok2api/backend/internal/repository"
	httpserver "github.com/chenyme/grok2api/backend/internal/transport/http"
	mediatransport "github.com/chenyme/grok2api/backend/internal/transport/http/media"
	httpmiddleware "github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
)

const (
	responseOwnershipCleanupBatchSize = 1000
	webResponseStateCleanupBatchSize  = 50
	responseCleanupBatchPause         = 10 * time.Millisecond
	responseCleanupInterval           = 5 * time.Minute
	responseCleanupBudget             = 30 * time.Second
	responseCleanupLockTTL            = 2 * time.Minute
)

// Application 管理后端进程生命周期和本地后台任务。
type Application struct {
	chatgpt         *chatgpt.Adapter
	adobe           *adobe.Adapter
	logger          *slog.Logger
	database        *relational.Database
	server          *http.Server
	inferenceGate   *httpmiddleware.ConcurrencyGate
	audits          *auditapp.Service
	responses       repository.ResponseRepository
	cleanupLock     repository.DistributedLock
	runtime         io.Closer
	settingsBus     repository.SettingsChangeBus
	invalidationBus repository.InvalidationBus
	settings        *settingsapp.Service
	gateway         *gateway.Service
	media           *mediaapp.Service
	quotaRecovery   *quotarecoveryapp.Service
	accounts        *accountapp.Service
	models          *modelapp.Service
	clientKeys      *clientkeyapp.Service
	updates         *updatecheckapp.Service
	invalidations   *invalidationapp.Service
	accountRepo     repository.AccountRepository
	modelRepo       repository.ModelRepository
	providers       *provider.Registry
	web             *webprovider.Adapter
	egress          *infraegress.Manager
	egressOps       *egressapp.Service
	startup         *startupState
}

// New 完成数据库、Provider、应用服务和 HTTP 路由装配。
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Application, error) {
	qualityGuardDirectory := strings.TrimSpace(os.Getenv("GROK2API_QUALITY_GUARD_DIR"))
	qualityGuardPath := func(name string) string {
		if qualityGuardDirectory == "" {
			return ""
		}
		return filepath.Join(qualityGuardDirectory, name)
	}
	var database *relational.Database
	var err error
	switch cfg.Database.Driver {
	case "sqlite":
		database, err = relational.OpenSQLite(ctx, cfg.Database.SQLite.Path)
	case "postgres":
		database, err = relational.OpenPostgres(ctx, cfg.Database.Postgres.DSN, cfg.Database.Postgres.MaxOpenConns, cfg.Database.Postgres.MaxIdleConns)
	default:
		return nil, fmt.Errorf("不支持的数据库驱动: %s", cfg.Database.Driver)
	}
	if err != nil {
		return nil, err
	}
	if err := database.InitializeSchema(ctx); err != nil {
		database.Close()
		return nil, err
	}
	cipher, err := security.NewCipher(cfg.Secrets.CredentialEncryptionKey)
	if err != nil {
		database.Close()
		return nil, err
	}

	adminRepo := relational.NewAdminRepository(database)
	sessionRepo := relational.NewAdminSessionRepository(database)
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	clientKeyRepo := relational.NewClientKeyRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	responseRepo := relational.NewResponseRepository(database)
	dashboardRepo := relational.NewDashboardRepository(database)
	runtimeSettingsRepo := relational.NewRuntimeSettingsRepository(database, cipher)
	egressRepo := relational.NewEgressRepository(database)
	mediaJobRepo := relational.NewMediaJobRepository(database)
	mediaAssetRepo := relational.NewMediaAssetRepository(database)
	mediaUploadTicketRepo := relational.NewMediaUploadTicketRepository(database)
	loadedConfig, settingsUpdatedAt, settingsRevision, err := settingsapp.LoadPersisted(ctx, cfg, runtimeSettingsRepo)
	if err != nil {
		database.Close()
		return nil, err
	}
	cfg = loadedConfig
	localMediaStore, err := inframedia.NewLocalStore(cfg.Media.Local.Path)
	if err != nil {
		database.Close()
		return nil, err
	}
	if err := preflightDeployment(cfg); err != nil {
		database.Close()
		return nil, err
	}
	var rateLimiter repository.RateLimiter
	var concurrency repository.ConcurrencyLimiter
	var sticky repository.StickySessionRepository
	var reasoningReplayStore repository.ReasoningReplayRepository
	var deviceSessions repository.DeviceSessionRepository
	var refreshLock repository.DistributedLock
	var settingsBus repository.SettingsChangeBus
	var quotaQueue repository.QuotaRecoveryQueue
	var quotaRefreshState repository.QuotaRefreshCoordinator
	var observedModelStore repository.ObservedModelStateRepository
	var invalidationBus repository.InvalidationBus
	var runtimeStore io.Closer
	runtimeHealth := func(context.Context) error { return nil }
	switch cfg.RuntimeStore.Driver {
	case "redis":
		redisStore, openErr := redisruntime.Open(ctx, redisruntime.Config{
			Address: cfg.RuntimeStore.Redis.Address, Username: cfg.RuntimeStore.Redis.Username,
			Password: cfg.RuntimeStore.Redis.Password, Database: cfg.RuntimeStore.Redis.Database,
			KeyPrefix: cfg.RuntimeStore.Redis.KeyPrefix, TLS: cfg.RuntimeStore.Redis.TLS,
			ConcurrencyLease: cfg.Server.RequestTimeout.Value() + time.Minute,
		})
		if openErr != nil {
			database.Close()
			return nil, openErr
		}
		runtimeStore = redisStore
		invalidationBus = redisStore
		runtimeHealth = redisStore.Ping
		rateLimiter = redisStore
		concurrency = redisruntime.NewConcurrencyLimiter(redisStore)
		sticky = redisStore
		reasoningReplayStore = redisruntime.NewReasoningReplayStore(redisStore)
		deviceSessions = redisruntime.NewDeviceSessionStore(redisStore)
		refreshLock = redisruntime.NewLockStore(redisStore)
		settingsBus = redisStore
		quotaQueue = redisStore
		quotaRefreshState = redisStore
		observedModelStore = redisStore
	case "memory":
		rateLimiter = memory.NewRateLimiter()
		concurrency = memory.NewConcurrencyLimiter()
		sticky = memory.NewStickyStore()
		reasoningReplayStore = memory.NewReasoningReplayStore(cfg.Routing.ReasoningReplayMaxEntries)
		deviceSessions = memory.NewDeviceSessionStore()
		refreshLock = memory.NewLockStore()
		quotaQueue = memory.NewQuotaRecoveryQueue()
		quotaRefreshState = memory.NewQuotaRefreshCoordinator()
	default:
		database.Close()
		return nil, fmt.Errorf("不支持的运行态驱动: %s", cfg.RuntimeStore.Driver)
	}
	logger.Info("deployment_topology", "replicas", cfg.Deployment.Replicas, "instance_id", cfg.Deployment.InstanceID, "cluster_id", cfg.Deployment.ClusterID, "database", cfg.Database.Driver, "runtime_store", cfg.RuntimeStore.Driver, "media_driver", cfg.Media.Driver, "shared_media", cfg.Deployment.SharedMedia)
	mediaService := mediaapp.NewServiceWithTickets(mediaAssetRepo, mediaJobRepo, mediaUploadTicketRepo, localMediaStore, refreshLock, mediaConfig(cfg))

	egressManager := infraegress.NewManager(egressRepo, cipher)
	egressManager.SetLogger(logger)
	egressManager.SetClearanceLock(refreshLock)
	egressManager.UpdateClearanceConfig(clearanceConfig(cfg))
	egressManager.UpdateBuildResponseHeaderTimeout(cfg.Provider.Build.ResponseHeaderTimeout.Value())
	egressManager.UpdateBuildStreamIdleTimeout(cfg.Provider.Build.StreamIdleTimeout.Value())
	cliAdapter := cliprovider.NewAdapter(cliprovider.Config{
		BaseURL: cfg.Provider.Build.BaseURL, FallbackBaseURL: config.NormalizeBuildFallbackBaseURL(cfg.Provider.Build.FallbackBaseURL),
		ClientVersion: cfg.Provider.Build.ClientVersion, ClientIdentifier: cfg.Provider.Build.ClientIdentifier,
		TokenAuth: cfg.Provider.Build.TokenAuth, UserAgent: cfg.Provider.Build.UserAgent,
		ResponseHeaderTimeout: cfg.Provider.Build.ResponseHeaderTimeout.Value(),
		StreamIdleTimeout:     cfg.Provider.Build.StreamIdleTimeout.Value(),
	}, cipher)
	cliAdapter.SetLogger(logger)
	cliAdapter.SetEgress(egressManager)
	cliAdapter.SetVideoUploadIssuer(mediaService)
	reasoningReplay := reasoningreplay.New(reasoningReplayStore, reasoningreplay.Config{
		Enabled: cfg.Routing.ReasoningReplayEnabled,
		TTL:     cfg.Routing.ReasoningReplayTTL.Value(),
	}, logger)
	cliAdapter.SetReasoningReplay(reasoningReplay)
	webAdapter := webprovider.NewAdapter(webProviderConfig(cfg), egressManager, cipher, responseRepo, mediaService)
	webAdapter.SetLogger(logger)
	consoleAdapter := consoleprovider.NewAdapter(consoleProviderConfig(cfg), egressManager, cipher, mediaService)
	chatgptAdapter := chatgpt.NewAdapter(chatgptProviderConfig(cfg), egressManager, cipher, mediaService)
	chatgptAdapter.SetLogger(logger)
	egressManager.SetExtensionClearance(egressdomain.Scope(account.ProviderChatGPT), chatgptClearanceTarget(cfg))
	adobeAdapter := adobe.NewAdapter(adobeProviderConfig(cfg), egressManager, cipher, mediaService, mediatransport.FetchRemoteImage)
	adobeAdapter.SetLogger(logger)
	providers := provider.NewRegistry(cliAdapter, webAdapter, consoleAdapter, chatgptAdapter, adobeAdapter)
	if err := providers.Validate(); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("校验 Provider 注册表: %w", err)
	}
	adminService := adminauth.NewService(adminRepo, sessionRepo, security.NewTokenService(cfg.Secrets.JWTSecret), cfg.Auth.AccessTokenTTL.Value(), cfg.Auth.RefreshTokenTTL.Value())
	adminService.SetLoginRateLimiter(rateLimiter)
	if err := adminService.Bootstrap(ctx, cfg.BootstrapAdmin.Username, cfg.BootstrapAdmin.Password); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, err
	}
	bulkPool := batch.NewSharedPool(maxBatchConcurrency(cfg.Batch), concurrency, "bulk:upstream")
	importPool := batch.NewSharedChildPool(cfg.Batch.ImportConcurrency, concurrency, "bulk:import", bulkPool)
	conversionPool := batch.NewSharedChildPool(cfg.Batch.ConversionConcurrency, concurrency, "bulk:conversion", bulkPool)
	syncPool := batch.NewSharedChildPool(cfg.Batch.SyncConcurrency, concurrency, "bulk:sync", bulkPool)
	refreshPool := batch.NewSharedChildPool(cfg.Batch.RefreshConcurrency, concurrency, "bulk:refresh", bulkPool)
	// detectPool 固定 32 并发，与额度同步/续期隔离，避免全量探测挤占维护任务。
	detectPool := batch.NewSharedChildPool(32, concurrency, "bulk:detect", bulkPool)
	for _, pool := range []*batch.Pool{importPool, conversionPool, syncPool, refreshPool, detectPool} {
		pool.UpdateJitter(cfg.Batch.RandomDelay.Value())
	}
	accountService := accountapp.NewService(accountRepo, auditRepo, deviceSessions, sticky, providers, cipher, refreshLock)
	cliAdapter.SetFallbackMarker(accountService)
	accountService.SetLogger(logger)
	accountService.UpdateAutoCleanConfig(accountAutoCleanConfig(cfg.Accounts))
	accountService.SetConcurrencyLimiter(concurrency)
	accountService.SetQuotaRecoveryQueue(quotaQueue)
	accountService.SetQuotaRefreshCoordinator(quotaRefreshState)
	accountService.SetObservedModelStore(observedModelStore)
	accountService.SetTaskPools(conversionPool, syncPool, refreshPool)
	accountService.SetDetectPool(detectPool)
	if err := accountService.RebuildBuildBotFlagIndex(ctx); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("重建 Build 风控路由索引: %w", err)
	}
	windows, err := accountRepo.ListQuotaRecoveryWindows(ctx, 100000)
	if err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("加载 Web 额度恢复事件: %w", err)
	}
	for _, window := range windows {
		if window.ResetAt != nil {
			if err := quotaQueue.ScheduleQuotaRecovery(ctx, account.QuotaRecoveryEvent{AccountID: window.AccountID, Mode: window.Mode, DueAt: *window.ResetAt}); err != nil {
				if runtimeStore != nil {
					_ = runtimeStore.Close()
				}
				database.Close()
				return nil, fmt.Errorf("恢复 Web 额度事件: %w", err)
			}
		}
	}
	modelService := modelapp.NewService(modelRepo, accountRepo, accountService, providers)
	modelService.SetBulkPool(syncPool)
	modelService.SetLogger(logger)
	if err := modelRepo.ReplaceProviderRoutes(ctx, account.ProviderWeb, webprovider.Routes()); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("初始化 Grok Web 模型目录: %w", err)
	}
	if err := modelRepo.ReplaceProviderRoutes(ctx, account.ProviderConsole, consoleprovider.Routes()); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("初始化 Grok Console 模型目录: %w", err)
	}
	if err := modelRepo.UpsertRoutes(ctx, chatgpt.Routes()); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("初始化 ChatGPT 图片模型: %w", err)
	}
	// Adobe 目录由后台任务按线上 discovery 替换（见 refreshAdobeCatalog）；启动时只用
	// 内嵌快照补齐缺失的路由，不删除：重启不能抹掉线上新增模型的路由及其绑定。
	if err := modelRepo.UpsertRoutes(ctx, adobe.SnapshotRoutes()); err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, fmt.Errorf("初始化 Adobe 模型目录: %w", err)
	}
	accountSyncService := accountsyncapp.NewService(logger, accountService, accountService, accountService, modelService)
	accountSyncService.SetBulkPool(importPool)
	accountSyncService.UpdateConcurrency(cfg.Batch.ImportConcurrency)
	egressService := egressapp.NewService(egressRepo, cipher, infraegress.DefaultUserAgent, accountRepo)
	egressService.ConfigureAutoAssignBounds(cfg.Routing.AutoAssignMaxNodeShare, cfg.Routing.AutoAssignMaxMigrationShare)
	egressService.SetClearanceManager(egressManager)
	egressService.SetNodeProber(egressManager)
	egressService.SetOperationsConfigInvalidator(egressManager)
	egressManager.SetFailureProber(egressService.TestNode)
	clientKeyService := clientkeyapp.NewService(clientKeyRepo, rateLimiter, concurrency, cfg.ClientKeyDefaults.RPMLimit, cfg.ClientKeyDefaults.MaxConcurrent, cipher)
	clientKeyService.SetProviderValidator(func(value account.Provider) bool { _, ok := providers.Definition(value); return ok })
	qualityGuardIdentity, err := clientKeyService.EnsureQualityGuardIdentity(ctx, cfg.QualityGuard.Enabled)
	if err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, err
	}
	qualityGuardToken, err := infraqualityguard.Prepare(qualityGuardPath("bootstrap.json"), cfg.QualityGuard, cfg.Secrets.JWTSecret)
	if err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, err
	}
	auditService := auditapp.NewService(auditRepo, logger, cfg.Audit.BufferSize, cfg.Audit.BatchSize, cfg.Audit.FlushInterval.Value())
	auditService.UpdateWriterConfig(cfg.Audit.BatchSize, cfg.Audit.FlushInterval.Value(), cfg.Audit.CommitDelay.Value())
	auditService.UpdateLedgerConfig(auditLedgerConfig(cfg.Audit))
	auditService.SetCommitObserver(clientKeyService.CompleteBillingBatch)
	auditService.SetDropObserver(clientKeyService.ReleaseBillingProtectionBatch)
	dashboardService := dashboardapp.NewService(dashboardRepo)
	pricingService, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(value account.Provider) bool { _, ok := providers.Definition(value); return ok }, providers.PricingModel)
	if err != nil {
		if runtimeStore != nil {
			_ = runtimeStore.Close()
		}
		database.Close()
		return nil, err
	}
	selector := gateway.NewSelector(accountRepo, concurrency, sticky, providers, cfg.Routing.StickyTTL.Value(), cfg.Routing.CooldownBase.Value(), cfg.Routing.CooldownMax.Value(), cfg.Routing.CapacityWait.Value())
	selector.SetLogger(logger)
	selector.UpdatePreferFreeBuild(cfg.Routing.PreferFreeBuild)
	selector.UpdateSegmentedSelector(cfg.Routing.SegmentedSelectorEnabled, cfg.Routing.SegmentedMinCandidates, cfg.Routing.SegmentedWindowSize)
	selector.UpdateExcludeBuildBotFlaggedFromScheduling(cfg.Accounts.ExcludeBuildBotFlaggedFromScheduling)
	accountService.UpdateExcludeBuildBotFlaggedFromScheduling(cfg.Accounts.ExcludeBuildBotFlaggedFromScheduling)
	egressManager.UpdateAccountIsolatedConnections(cfg.Routing.AccountIsolatedConnections)
	invalidationService := invalidationapp.NewService(invalidationBus, invalidationSourceInstance(cfg), func(event repository.InvalidationEvent) {
		selector.ApplyInvalidation(event)
		clientKeyService.ApplyInvalidation(event)
		pricingService.ApplyInvalidation(event)
		modelService.ApplyInvalidation(event)
	}, logger)
	accountRepo.SetInvalidationObserver(invalidationService.Notify)
	modelRepo.SetInvalidationObserver(invalidationService.Notify)
	clientKeyRepo.SetInvalidationObserver(invalidationService.Notify)
	pricingService.SetInvalidationObserver(invalidationService.Notify)
	pricingService.SetOfficialRates(func(value account.Provider) bool {
		definition, ok := providers.Definition(value)
		return ok && definition.Profile.OfficialPricing
	})
	gatewayService := gateway.NewService(modelService, auditService, accountService, clientKeyService, providers, selector, responseRepo, cfg.Routing.MaxAttempts)
	gatewayService.UpdateQualityRetry(qualityRetryRuntime(cfg.QualityGuard))
	gatewayService.UpdateVideoMaxAttempts(cfg.Routing.VideoMaxAttempts)
	gatewayService.UpdateMarkBuildChatDeniedAsReauth(cfg.Routing.MarkBuildChatDeniedAsReauth)
	gatewayService.SetLogger(logger)
	gatewayService.SetPricing(pricingService)
	egressService.SetQualityProber(gatewayService)
	gatewayService.UpdateBuildForbiddenReauthPolicy(cfg.Accounts.MarkBuildForbiddenReauth, cfg.Accounts.BuildForbiddenReauthCodes)
	gatewayService.UpdateRequestTimeout(cfg.Server.RequestTimeout.Value())
	gatewayService.ConfigureMedia(mediaJobRepo, cfg.Provider.Web.MediaConcurrency)
	gatewayService.ConfigureMediaAssets(mediaService)
	gatewayService.UpdateDefaultImageModel(cmp.Or(cfg.Provider.ChatGPT.DefaultImageModel, chatgpt.DefaultImageModel))
	quotaRecoveryService := quotarecoveryapp.NewService(logger, quotaQueue, accountService, cfg.Provider.Web.RecoveryBackoffBase.Value(), cfg.Provider.Web.RecoveryBackoffMax.Value())
	quotaRecoveryService.SetBulkPool(syncPool)
	inferenceConcurrency := httpmiddleware.NewConcurrencyGate(cfg.Server.MaxConcurrentRequests)
	var notifySettings func(context.Context)
	if settingsBus != nil {
		notifySettings = func(notifyCtx context.Context) {
			publishCtx, cancel := context.WithTimeout(context.WithoutCancel(notifyCtx), 3*time.Second)
			defer cancel()
			if err := settingsBus.PublishSettingsChanged(publishCtx); err != nil {
				logger.Warn("settings_change_publish_failed", "error", err)
			}
		}
	}
	settingsService := settingsapp.NewService(cfg, settingsUpdatedAt, settingsRevision, runtimeSettingsRepo, notifySettings, func(next config.Config) {
		inferenceConcurrency.UpdateLimit(next.Server.MaxConcurrentRequests)
		bulkPool.UpdateLimit(maxBatchConcurrency(next.Batch))
		importPool.UpdateLimit(next.Batch.ImportConcurrency)
		conversionPool.UpdateLimit(next.Batch.ConversionConcurrency)
		syncPool.UpdateLimit(next.Batch.SyncConcurrency)
		refreshPool.UpdateLimit(next.Batch.RefreshConcurrency)
		detectPool.UpdateLimit(32)
		for _, pool := range []*batch.Pool{importPool, conversionPool, syncPool, refreshPool, detectPool} {
			pool.UpdateJitter(next.Batch.RandomDelay.Value())
		}
		cliAdapter.UpdateConfig(cliprovider.Config{
			BaseURL: next.Provider.Build.BaseURL, FallbackBaseURL: config.NormalizeBuildFallbackBaseURL(next.Provider.Build.FallbackBaseURL),
			ClientVersion: next.Provider.Build.ClientVersion, ClientIdentifier: next.Provider.Build.ClientIdentifier,
			TokenAuth: next.Provider.Build.TokenAuth, UserAgent: next.Provider.Build.UserAgent,
			ResponseHeaderTimeout: next.Provider.Build.ResponseHeaderTimeout.Value(),
			StreamIdleTimeout:     next.Provider.Build.StreamIdleTimeout.Value(),
		})
		egressManager.UpdateBuildResponseHeaderTimeout(next.Provider.Build.ResponseHeaderTimeout.Value())
		egressManager.UpdateBuildStreamIdleTimeout(next.Provider.Build.StreamIdleTimeout.Value())
		webAdapter.UpdateConfig(webProviderConfig(next))
		egressManager.UpdateClearanceConfig(clearanceConfig(next))
		consoleAdapter.UpdateConfig(consoleProviderConfig(next))
		chatgptAdapter.UpdateConfig(chatgptProviderConfig(next))
		adobeAdapter.UpdateConfig(adobeProviderConfig(next))
		gatewayService.UpdateDefaultImageModel(cmp.Or(next.Provider.ChatGPT.DefaultImageModel, chatgpt.DefaultImageModel))
		egressManager.SetExtensionClearance(egressdomain.Scope(account.ProviderChatGPT), chatgptClearanceTarget(next))
		mediaService.UpdateConfig(mediaConfig(next))
		quotaRecoveryService.UpdateConfig(next.Provider.Web.RecoveryBackoffBase.Value(), next.Provider.Web.RecoveryBackoffMax.Value())
		accountSyncService.UpdateConcurrency(next.Batch.ImportConcurrency)
		selector.UpdateConfig(next.Routing.StickyTTL.Value(), next.Routing.CooldownBase.Value(), next.Routing.CooldownMax.Value(), next.Routing.CapacityWait.Value())
		selector.UpdatePreferFreeBuild(next.Routing.PreferFreeBuild)
		selector.UpdateSegmentedSelector(next.Routing.SegmentedSelectorEnabled, next.Routing.SegmentedMinCandidates, next.Routing.SegmentedWindowSize)
		egressService.ConfigureAutoAssignBounds(next.Routing.AutoAssignMaxNodeShare, next.Routing.AutoAssignMaxMigrationShare)
		selector.UpdateExcludeBuildBotFlaggedFromScheduling(next.Accounts.ExcludeBuildBotFlaggedFromScheduling)
		accountService.UpdateExcludeBuildBotFlaggedFromScheduling(next.Accounts.ExcludeBuildBotFlaggedFromScheduling)
		egressManager.UpdateAccountIsolatedConnections(next.Routing.AccountIsolatedConnections)
		reasoningReplay.UpdateConfig(reasoningreplay.Config{Enabled: next.Routing.ReasoningReplayEnabled, TTL: next.Routing.ReasoningReplayTTL.Value()})
		gatewayService.UpdateMaxAttempts(next.Routing.MaxAttempts)
		gatewayService.UpdateQualityRetry(qualityRetryRuntime(next.QualityGuard))
		gatewayService.UpdateVideoMaxAttempts(next.Routing.VideoMaxAttempts)
		gatewayService.UpdateMarkBuildChatDeniedAsReauth(next.Routing.MarkBuildChatDeniedAsReauth)
		gatewayService.UpdateBuildForbiddenReauthPolicy(next.Accounts.MarkBuildForbiddenReauth, next.Accounts.BuildForbiddenReauthCodes)
		auditService.UpdateWriterConfig(next.Audit.BatchSize, next.Audit.FlushInterval.Value(), next.Audit.CommitDelay.Value())
		auditService.UpdateLedgerConfig(auditLedgerConfig(next.Audit))
		clientKeyService.UpdateDefaults(next.ClientKeyDefaults.RPMLimit, next.ClientKeyDefaults.MaxConcurrent)
		accountService.UpdateAutoCleanConfig(accountAutoCleanConfig(next.Accounts))
	})
	updateService := updatecheckapp.NewService(buildinfo.CurrentVersion(), cfg.Frontend.StaticPath, nil)

	startup := newStartupState(len(windows))
	readiness := func(readyCtx context.Context) httpserver.ReadinessSnapshot {
		return readinessSnapshot(readyCtx, startup, runtimeHealth, modelRepo, accountRepo, providers, auditService)
	}
	qualityGuardProbe := egressapp.QualityProbeInput{}
	if cfg.QualityGuard.Enabled {
		qualityGuardProbe = egressapp.QualityProbeInput{
			ClientKeyID: qualityGuardIdentity.ID, Model: cfg.QualityGuard.Model,
			Prompt: infraqualityguard.ProbePrompt, Expected: infraqualityguard.ProbeExpected,
			MaxOutputTokens: cfg.QualityGuard.MaxOutputTokens,
		}
	}
	providerCatalog := providercatalog.New(providers)
	builtinScopes := map[egressdomain.Scope]bool{egressdomain.ScopeBuild: true, egressdomain.ScopeWeb: true, egressdomain.ScopeConsole: true, egressdomain.ScopeWebAsset: true, egressdomain.ScopeConsoleAsset: true}
	extensionScopes := []egressdomain.Scope{}
	for _, entry := range providerCatalog.Entries() {
		for _, scope := range entry.EgressScopes {
			if !builtinScopes[egressdomain.Scope(scope)] {
				extensionScopes = append(extensionScopes, egressdomain.Scope(scope))
			}
		}
	}
	egressdomain.SetExtensionScopes(extensionScopes)
	router := httpserver.New(httpserver.Dependencies{Providers: providerCatalog, Pricing: pricingService, Logger: logger, RequestTimeout: cfg.Server.RequestTimeout.Value(), MaxBodyBytes: cfg.Server.MaxBodyBytes, TrustedProxies: cfg.Server.TrustedProxies, ConcurrencyGate: inferenceConcurrency, SecureCookies: cfg.Auth.SecureCookies, SwaggerEnabled: cfg.Server.SwaggerEnabled, PublicAPIBaseURL: cfg.Frontend.EffectivePublicAPIBaseURL(), FrontendStaticPath: cfg.Frontend.StaticPath, Readiness: readiness, TrafficReady: startup.acceptsTraffic, AdminAuth: adminService, Accounts: accountService, AccountSync: accountSyncService, Models: modelService, ClientKeys: clientKeyService, Audits: auditService, Dashboard: dashboardService, Gateway: gatewayService, Media: mediaService, Settings: settingsService, Egress: egressService, QualityGuardStatePath: qualityGuardPath("state.json"), QualityGuardConfigPath: qualityGuardPath("runtime-config.json"), QualityGuardToken: qualityGuardToken, QualityGuardProbe: qualityGuardProbe, Updates: updateService})
	server := &http.Server{Addr: cfg.Server.Listen, Handler: router, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: cfg.Server.ReadTimeout.Value(), IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	return &Application{chatgpt: chatgptAdapter, adobe: adobeAdapter,
		logger: logger, database: database, server: server, inferenceGate: inferenceConcurrency,
		audits: auditService, responses: responseRepo, cleanupLock: refreshLock, runtime: runtimeStore,
		settingsBus: settingsBus, invalidationBus: invalidationBus, settings: settingsService, gateway: gatewayService, media: mediaService, quotaRecovery: quotaRecoveryService, accounts: accountService, models: modelService, clientKeys: clientKeyService, updates: updateService, invalidations: invalidationService,
		accountRepo: accountRepo, modelRepo: modelRepo, providers: providers, web: webAdapter, egress: egressManager, egressOps: egressService, startup: startup,
	}, nil
}

func invalidationSourceInstance(cfg config.Config) string {
	if value := strings.TrimSpace(cfg.Deployment.InstanceID); value != "" {
		return value
	}
	return fmt.Sprintf("process-%d", time.Now().UnixNano())
}

func maxBatchConcurrency(value config.BatchConfig) int {
	return max(value.ImportConcurrency, value.ConversionConcurrency, value.SyncConcurrency, value.RefreshConcurrency)
}

func webProviderConfig(cfg config.Config) webprovider.Config {
	return webprovider.Config{
		BaseURL: cfg.Provider.Web.BaseURL, QuotaTimeoutSeconds: int(cfg.Provider.Web.QuotaTimeout.Value().Seconds()),
		StatsigMode: cfg.Provider.Web.StatsigMode, StatsigManualValue: cfg.Provider.Web.StatsigManualValue,
		StatsigSignerURL:   cfg.Provider.Web.StatsigSignerURL,
		ChatTimeoutSeconds: int(cfg.Provider.Web.ChatTimeout.Value().Seconds()), StreamIdleTimeoutSeconds: int(cfg.Provider.Web.StreamIdleTimeout.Value().Seconds()),
		ImageTimeoutSeconds: int(cfg.Provider.Web.ImageTimeout.Value().Seconds()),
		VideoTimeoutSeconds: int(cfg.Provider.Web.VideoTimeout.Value().Seconds()), MaxInputImageBytes: cfg.Media.MaxImageBytes,
		AllowNSFW:            cfg.Provider.Web.AllowNSFW,
		FreeVideoDurationCap: cfg.Provider.Web.FreeVideoDurationCap,
	}
}

func clearanceConfig(cfg config.Config) infraegress.ClearanceConfig {
	return infraegress.ClearanceConfig{
		Mode: cfg.Provider.Web.ClearanceMode, FlareSolverrURL: cfg.Provider.Web.FlareSolverrURL,
		TargetURL: cfg.Provider.Web.BaseURL, Timeout: cfg.Provider.Web.ClearanceTimeout.Value(),
		RefreshInterval: cfg.Provider.Web.ClearanceRefresh.Value(),
	}
}

func chatgptProviderConfig(cfg config.Config) chatgpt.Config {
	value := cfg.Provider.ChatGPT
	return chatgpt.Config{ImageConversationModel: value.ImageConversationModel, OutputTokenLimit: value.OutputTokenLimit, VisionTokens: value.VisionTokens}
}

func adobeProviderConfig(cfg config.Config) adobe.Config {
	value := cfg.Provider.Adobe
	return adobe.Config{RequestTimeout: value.Timeout.Value(), SherlockMintAPI: value.SherlockMintAPI}
}

// chatgptClearanceTarget is the site FlareSolverr solves for ChatGPT egresses;
// empty disables managed clearance.
func chatgptClearanceTarget(cfg config.Config) string {
	if cfg.Provider.ChatGPT.CloudflareClearance {
		return "https://chatgpt.com"
	}
	return ""
}

func consoleProviderConfig(cfg config.Config) consoleprovider.Config {
	return consoleprovider.Config{
		BaseURL: cfg.Provider.Console.BaseURL, SessionBaseURL: cfg.Provider.Web.BaseURL,
		TimeoutSeconds: int(cfg.Provider.Console.ChatTimeout.Value().Seconds()), StreamIdleTimeoutSeconds: int(cfg.Provider.Console.StreamIdleTimeout.Value().Seconds()),
	}
}

func accountAutoCleanConfig(value config.AccountsConfig) accountapp.AutoCleanConfig {
	return accountapp.AutoCleanConfig{
		Enabled:         value.AutoCleanReauthEnabled,
		Interval:        value.AutoCleanReauthInterval.Value(),
		MinAge:          value.AutoCleanReauthMinAge.Value(),
		IncludeDisabled: value.AutoCleanIncludeDisabled,
	}
}

func qualityRetryRuntime(value config.QualityGuardConfig) gateway.QualityRetryRuntime {
	retry := value.RequestRetry
	return gateway.QualityRetryRuntime{
		// Auto-disable needs request-path detection even when the YAML retry
		// switch is off. Derive this at runtime so turning auto-disable off
		// restores the original policy without persisting a second switch.
		Enabled:                         retry.Enabled || retry.AutoDisable,
		MaxAttempts:                     retry.MaxAttempts,
		HoldTimeout:                     retry.HoldTimeout.Value(),
		MinOutputTokens:                 int64(retry.MinOutputTokens),
		OnExhausted:                     retry.OnExhausted,
		AutoDisable:                     retry.AutoDisable,
		AccountCooldown:                 retry.AccountCooldown.Value(),
		IdleAccountCooldown:             retry.IdleAccountCooldown.Value(),
		MinEncryptedBytes:               retry.MinEncryptedBytes,
		EncryptedBytesPerReasoningToken: retry.EncryptedBytesPerReasoningToken,
		SoftTPS:                         value.SoftTPS,
		HardTPS:                         value.HardTPS,
		MinGenerationMS:                 value.MinimumGenerationWindow.Value().Milliseconds(),
		FailClosed:                      value.FailClosed,
	}
}

func auditLedgerConfig(value config.AuditConfig) auditapp.LedgerConfig {
	return auditapp.LedgerConfig{
		Mode:                      auditapp.LedgerMode(value.LedgerMode),
		FailureThreshold:          value.LedgerFailureThreshold,
		UnhealthyGrace:            value.LedgerUnhealthyGrace.Value(),
		QueueHighWatermarkPercent: value.LedgerQueueHighWatermarkPct,
	}
}

func mediaConfig(cfg config.Config) mediaapp.Config {
	return mediaapp.Config{
		PublicBaseURL: cfg.Frontend.EffectivePublicAPIBaseURL(),
		MaxImageBytes: cfg.Media.MaxImageBytes, MaxTotalBytes: cfg.Media.MaxTotalBytes,
		CleanupThresholdPercent: cfg.Media.CleanupThresholdPercent, CleanupInterval: cfg.Media.CleanupInterval.Value(),
	}
}

// Run 启动 HTTP 服务和本地后台维护任务。
func (a *Application) Run(ctx context.Context) error {
	a.audits.Start()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.audits.Close(closeCtx); err != nil {
			a.logger.Warn("audit_shutdown_failed", "error", err)
		}
	}()
	runCtx, cancelBackground := context.WithCancel(ctx)
	var background sync.WaitGroup
	defer func() {
		cancelBackground()
		background.Wait()
	}()
	cancelRequests := cancelableRequests(a.server)
	defer cancelRequests()
	errCh := make(chan error, 1)
	go func() {
		a.logger.Info("server_started", "listen", a.server.Addr)
		errCh <- a.server.ListenAndServe()
	}()
	a.reconcileStartup(runCtx)
	startBackground := func(name string, task func(context.Context) error) {
		background.Add(1)
		go func() {
			defer background.Done()
			a.runSupervisedTask(runCtx, name, task)
		}()
	}
	if a.invalidationBus != nil {
		startBackground("invalidation_publisher", a.invalidations.RunPublisher)
		startBackground("invalidation_subscriber", a.invalidations.RunSubscriber)
	}
	startBackground("settings_reconcile", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 30*time.Second, "settings_reconcile", func(runCtx context.Context) error {
			return a.settings.ReloadPersisted(runCtx)
		})
		return nil
	})
	startBackground("performance_metrics", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, time.Minute, "performance_metrics", func(context.Context) error {
			a.logPerformanceMetrics()
			return nil
		})
		return nil
	})
	startBackground("release_check", func(taskCtx context.Context) error {
		a.updates.Check(taskCtx)
		a.runPeriodicTask(taskCtx, 24*time.Hour, "release_check", func(checkCtx context.Context) error {
			a.updates.Check(checkCtx)
			return nil
		})
		return nil
	})
	startBackground("billing_reservation_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 10*time.Minute, "billing_reservation_cleanup", func(runCtx context.Context) error {
			_, err := a.clientKeys.CleanupExpiredBilling(runCtx, 1000)
			return err
		})
		return nil
	})
	startBackground("model_cooldown_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, 10*time.Minute, "model_cooldown_cleanup", func(runCtx context.Context) error {
			_, err := a.accountRepo.PruneExpiredModelQuotaBlocks(runCtx, time.Now().UTC(), 1000)
			return err
		})
		return nil
	})
	startBackground("response_ownership_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, responseCleanupInterval, "response_ownership_cleanup", func(runCtx context.Context) error {
			return a.cleanupExpiredResponses(runCtx, time.Now().UTC())
		})
		return nil
	})
	startBackground("audit_retention_cleanup", func(taskCtx context.Context) error {
		a.runPeriodicTask(taskCtx, time.Hour, "audit_retention_cleanup", func(runCtx context.Context) error {
			retentionDays := a.settings.Get().Config.Audit.RetentionDays
			if retentionDays == 0 {
				return nil
			}
			_, err := a.audits.PurgeOutdated(runCtx, retentionDays)
			return err
		})
		return nil
	})
	startBackground("quota_recovery", func(taskCtx context.Context) error {
		a.quotaRecovery.Run(taskCtx)
		return nil
	})
	startBackground("quota_refresh", func(taskCtx context.Context) error {
		a.accounts.RunQuotaRefresh(taskCtx)
		return nil
	})
	startBackground("credential_refresh", func(taskCtx context.Context) error {
		a.accounts.RunCredentialRefresh(taskCtx)
		return nil
	})
	startBackground("account_auto_clean", func(taskCtx context.Context) error {
		a.accounts.RunAccountAutoClean(taskCtx)
		return nil
	})
	startBackground("statsig_warmup", func(taskCtx context.Context) error {
		a.runStatsigWarmup(taskCtx)
		return nil
	})
	startBackground("web_quota_startup_catchup", func(taskCtx context.Context) error {
		a.runWebQuotaCatchup(taskCtx)
		return nil
	})
	startBackground("console_usage_migration", func(taskCtx context.Context) error {
		a.runConsoleUsageMigration(taskCtx)
		return nil
	})
	startBackground("console_quota_stale_catchup", func(taskCtx context.Context) error {
		a.runConsoleQuotaCatchup(taskCtx)
		return nil
	})
	startBackground("model_catalog_startup_catchup", func(taskCtx context.Context) error {
		a.runModelCatalogCatchup(taskCtx)
		return nil
	})
	startBackground("media_job_recovery", func(taskCtx context.Context) error {
		a.gateway.RunMediaRecovery(taskCtx)
		return nil
	})
	startBackground("media_job_workers", func(taskCtx context.Context) error {
		a.gateway.RunMediaWorkers(taskCtx)
		return nil
	})
	startBackground("media_cleanup", func(taskCtx context.Context) error {
		a.media.RunCleanup(taskCtx, func(err error) {
			a.logger.Warn("media_cleanup_failed", "error", err)
		})
		return nil
	})
	startBackground("clearance_refresh", func(taskCtx context.Context) error {
		if err := a.egress.RefreshDueClearances(taskCtx, false); err != nil {
			a.logger.Warn("clearance_initial_refresh_failed", "error", err)
		}
		a.runPeriodicTask(taskCtx, time.Minute, "clearance_refresh", func(runCtx context.Context) error {
			if err := a.egress.RefreshDueClearances(runCtx, false); err != nil {
				a.logger.Warn("clearance_refresh_failed", "error", err)
			}
			return nil
		})
		return nil
	})
	startBackground("adobe_model_catalog", func(taskCtx context.Context) error {
		// Adobe 的模型列表很少变化：启动时拉一次，之后每天一次；失败时 1 小时后重试。
		refresh := a.refreshAdobeCatalog()
		for {
			runCtx, cancel := context.WithTimeout(taskCtx, time.Minute)
			err := refresh(runCtx)
			cancel()
			wait := 24 * time.Hour
			if err != nil {
				a.logger.Warn("adobe_model_catalog_failed", "error", err)
				wait = time.Hour
			}
			timer := time.NewTimer(wait)
			select {
			case <-taskCtx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
	})
	startBackground("egress_operations", func(taskCtx context.Context) error {
		if err := a.egressOps.RunMaintenance(taskCtx); err != nil {
			a.logger.Warn("egress_operations_initial_run_failed", "error", err)
		}
		a.runPeriodicTask(taskCtx, time.Minute, "egress_operations", a.egressOps.RunMaintenance)
		return nil
	})
	if a.settingsBus != nil {
		startBackground("settings_change_listener", func(taskCtx context.Context) error {
			return a.settingsBus.ListenSettingsChanges(taskCtx, func(eventCtx context.Context) error {
				reloadCtx, cancel := context.WithTimeout(eventCtx, 5*time.Second)
				defer cancel()
				if err := a.settings.ReloadPersisted(reloadCtx); err != nil {
					a.logger.Warn("settings_reload_failed", "error", err)
				}
				return nil
			})
		})
	}
	a.queueDueWebQuotaRefresh(runCtx)
	select {
	case <-ctx.Done():
		// The deferred audit close and the caller's storage close run after
		// this, so every handler must have returned and finalized first. The
		// drain period covers the gateway's 5s finalization budget.
		if err := drainServer(a.server, cancelRequests, a.inferenceGate, 10*time.Second, 10*time.Second); err != nil {
			return fmt.Errorf("关闭 HTTP 服务: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// cancelableRequests derives every request context of server from one context
// and returns its cancel, so shutdown can end streams that outlive the grace
// period through their normal cancellation path.
func cancelableRequests(server *http.Server) context.CancelFunc {
	requestCtx, cancel := context.WithCancel(context.Background())
	server.BaseContext = func(net.Listener) context.Context { return requestCtx }
	return cancel
}

// drainServer gives requests the grace period to finish, then cancels every
// request context and waits up to drain for the handlers to return, including
// hijacked WebSockets that Shutdown never waits for. Only handlers that are
// still running after that make it an error.
func drainServer(server *http.Server, cancelRequests context.CancelFunc, gate *httpmiddleware.ConcurrencyGate, grace, drain time.Duration) error {
	graceCtx, cancelGrace := context.WithTimeout(context.Background(), grace)
	defer cancelGrace()
	err := server.Shutdown(graceCtx)
	cancelRequests()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drain)
	defer cancelDrain()
	if errors.Is(err, context.DeadlineExceeded) {
		err = server.Shutdown(drainCtx)
	}
	if err == nil {
		err = gate.WaitIdle(drainCtx)
	}
	if err != nil {
		return errors.Join(err, server.Close())
	}
	return nil
}

func (a *Application) cleanupExpiredResponses(ctx context.Context, now time.Time) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, responseCleanupBudget)
	defer cancel()
	if a.cleanupLock != nil {
		release, acquired, err := a.cleanupLock.Acquire(cleanupCtx, "response-ownership-cleanup", responseCleanupLockTTL)
		if err != nil {
			return err
		}
		if !acquired {
			return nil
		}
		defer release()
	}
	var totalOwnership, totalWebState int64
	// Delete batches until one comes back short or the budget runs out.
	for {
		if err := cleanupCtx.Err(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.recordResponseCleanup(totalOwnership, totalWebState, true)
			return nil
		}
		result, err := a.responses.DeleteExpired(cleanupCtx, now, responseOwnershipCleanupBatchSize, webResponseStateCleanupBatchSize)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				a.recordResponseCleanup(totalOwnership, totalWebState, true)
				return nil
			}
			return err
		}
		totalOwnership += result.OwnershipDeleted
		totalWebState += result.WebStateDeleted
		if !result.HasMore {
			a.recordResponseCleanup(totalOwnership, totalWebState, false)
			return nil
		}
		// Pause so request-path writers can take the SQLite write lock between batches.
		select {
		case <-cleanupCtx.Done():
		case <-time.After(responseCleanupBatchPause):
		}
	}
}

func (a *Application) recordResponseCleanup(ownershipDeleted, webStateDeleted int64, backlog bool) {
	outcome := "complete"
	if backlog {
		outcome = "backlog"
		a.logger.Warn("response_cleanup_backlog", "ownership_deleted", ownershipDeleted, "web_state_deleted", webStateDeleted)
	}
	labels := perfmetrics.Labels{Subsystem: "response", Operation: "cleanup", Outcome: outcome}
	perfmetrics.Default.Add("response_cleanup_ownership_rows", labels, ownershipDeleted)
	perfmetrics.Default.Add("response_cleanup_web_state_rows", labels, webStateDeleted)
}

func (a *Application) logPerformanceMetrics() {
	stats := a.database.Stats()
	databaseLabels := perfmetrics.Labels{Subsystem: "database", Operation: a.database.Dialect()}
	perfmetrics.Default.SetGauge("db_open_connections", databaseLabels, int64(stats.OpenConnections))
	perfmetrics.Default.SetGauge("db_in_use_connections", databaseLabels, int64(stats.InUse))
	perfmetrics.Default.SetGauge("db_idle_connections", databaseLabels, int64(stats.Idle))
	perfmetrics.Default.SetGauge("db_wait_count", databaseLabels, stats.WaitCount)
	perfmetrics.Default.SetGauge("db_wait_duration_us", databaseLabels, stats.WaitDuration.Microseconds())
	if a.audits != nil {
		a.audits.LedgerSnapshot()
	}
	if a.accounts != nil {
		quota := a.accounts.QuotaRefreshStats()
		labels := perfmetrics.Labels{Subsystem: "quota", Operation: "refresh"}
		perfmetrics.Default.SetGauge("quota_refresh_pending", labels, int64(quota.Pending))
		perfmetrics.Default.SetGauge("quota_refresh_queued", labels, int64(quota.Queued))
		perfmetrics.Default.SetGauge("quota_refresh_running", labels, int64(quota.Running))
	}
	for _, sample := range perfmetrics.Default.CollectAndReset() {
		a.logger.Info("performance_metric",
			"name", sample.Name,
			"subsystem", sample.Labels.Subsystem,
			"operation", sample.Labels.Operation,
			"provider", sample.Labels.Provider,
			"plane", sample.Labels.Plane,
			"stage", sample.Labels.Stage,
			"ordinal", sample.Labels.Ordinal,
			"outcome", sample.Labels.Outcome,
			"count", sample.Count,
			"total", sample.Total,
			"maximum", sample.Maximum,
			"gauge", sample.Gauge,
			"has_gauge", sample.HasGauge,
		)
	}
}

func (a *Application) Close() error {
	if a.chatgpt != nil {
		_ = a.chatgpt.Close()
	}
	var runtimeErr error
	if a.runtime != nil {
		runtimeErr = a.runtime.Close()
	}
	return errors.Join(runtimeErr, a.database.Close())
}

func (a *Application) runPeriodicTask(ctx context.Context, interval time.Duration, name string, task func(context.Context) error) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			runCtx, cancel := context.WithTimeout(ctx, minDuration(interval, 5*time.Minute))
			err := task(runCtx)
			cancel()
			if err != nil {
				a.logger.Warn(name+"_failed", "error", err)
			}
			resetTimer(timer, interval)
		}
	}
}

func (a *Application) runSupervisedTask(ctx context.Context, name string, task func(context.Context) error) {
	backoff := time.Second
	for {
		err := batch.Do(ctx, task)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("后台任务意外退出")
		}
		var panicErr *batch.PanicError
		if errors.As(err, &panicErr) {
			a.logger.Error("background_task_restarting", "task", name, "backoff", backoff, "error", panicErr, "stack", string(panicErr.Stack))
		} else {
			a.logger.Error("background_task_restarting", "task", name, "backoff", backoff, "error", err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

// refreshAdobeCatalog 拉取 Adobe 官方 discovery 并让路由跟随它：新增模型自动登记，
// 下架模型的路由删除。目录没变化时不写库（ReplaceProviderRoutes 每次都会广播失效）。
// 拉取失败时内存与数据库都保留上一次的结果。
func (a *Application) refreshAdobeCatalog() func(context.Context) error {
	var applied []modeldomain.Route // 只在本 goroutine 使用；进程重启后首次成功总会对账一次
	return func(ctx context.Context) error {
		routes, err := a.adobe.RefreshCatalog(ctx)
		if err != nil {
			return err
		}
		if applied != nil && slices.EqualFunc(routes, applied, func(x, y modeldomain.Route) bool {
			return x.PublicID == y.PublicID && x.UpstreamModel == y.UpstreamModel && x.Capability == y.Capability
		}) {
			return nil
		}
		if err := a.modelRepo.ReplaceProviderRoutes(ctx, account.ProviderAdobe, routes); err != nil {
			return err
		}
		applied = routes
		return nil
	}
}
