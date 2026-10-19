package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrModelNotFound              = errors.New("模型不存在或未启用")
	ErrNoAvailableAccount         = errors.New("没有可用上游账号")
	ErrResponseNotFound           = errors.New("Response 不存在或已过期")
	ErrResponseAccountUnavailable = errors.New("Response 绑定的上游账号不可用")
	ErrResponseStateUnsupported   = errors.New("目标模型不支持有状态 Response")
	ErrConversationUnsupported    = errors.New("目标模型不支持当前对话协议")
	ErrVideoInputTooLarge         = errors.New("视频参考图片编码后总输入超过 32 MiB")
	ErrVideoInputUnavailable      = errors.New("视频临时输入不存在或已过期")
	ErrVideoParameterInvalid      = provider.ErrVideoParameterInvalid
	ErrVideoOperationUnsupported  = provider.ErrVideoOperationUnsupported
	ErrLedgerUnavailable          = errors.New("计费账本暂不可用")
)

const finalizationTimeout = 5 * time.Second
const minimumTextBillingReservationTTL = 2 * time.Hour
const billingReservationCrashGrace = 10 * time.Minute
const mediaBillingReservationTTL = 24 * time.Hour
const accountStateWriteTimeout = 3 * time.Second

type Input struct {
	RequestID       string
	ClientKey       clientkey.Key
	PublicModel     string
	Body            []byte
	Streaming       bool
	PromptCacheKey  string
	PromptCacheSeed string
	// AllowClientToolCacheRoute indicates that the client request is compatible with the Build mixed-tool cache route.
	// It only controls whether native x_search is added to existing client tools; it is not an authentication result.
	AllowClientToolCacheRoute bool
	PreviousResponseID        string
	// GrokTurnIndex forwards only the turn supplied by a real Grok Shell client; the server never infers or increments it.
	GrokTurnIndex string
	Operation     audit.Operation
	Method        string
	Path          string
	Headers       map[string][]string
	// auditOperation may classify a normal protocol request differently for
	// operator visibility without changing routing or Provider semantics.
	auditOperation audit.Operation
	// skipQualityHold is set only by trusted gateway-side request classifiers.
	skipQualityHold bool
	// ForcedEgressNodeID is an internal-only administrator probe constraint.
	// Public inference handlers never populate it.
	ForcedEgressNodeID uint64
	// ForcedAccountID is paired with ForcedEgressNodeID only by the internal
	// Quality Guard recovery path. It never accepts public request input.
	ForcedAccountID uint64
}

type Usage struct {
	// Reported distinguishes a real upstream/estimated usage object from the
	// zero value used when a response fails before usage is available. Token
	// counts may legitimately all be zero, so the numeric fields cannot carry
	// this presence information by themselves.
	Reported bool
	// OutputObserved records that the transport actually forwarded generated
	// content even when an interrupted upstream never emitted final usage.
	OutputObserved         bool
	InputTokens            int64
	CachedInputTokens      int64
	OutputTokens           int64
	ReasoningTokens        int64
	TotalTokens            int64
	CostInUSDTicks         int64
	NumSourcesUsed         int64
	NumServerSideToolsUsed int64
	ContextInputTokens     int64
	ContextOutputTokens    int64
	ResponseModel          string
	// OutputImages counts the final images an image stream forwarded.
	OutputImages int64
}

type Result struct {
	StatusCode          int
	Status              string
	Header              http.Header
	Body                io.ReadCloser
	MarkFirstToken      func()
	RecordStreamFailure func(StreamFailureDiagnostic)
	Finalize            func(usage Usage, responseID, errorCode string)
}

// StreamFailureDiagnostic safely projects a failure termination event returned in-stream after downstream 2xx headers.
// Body contains only transport-extracted error fields and still receives the standard redaction and size limits.
type StreamFailureDiagnostic struct {
	Body          []byte
	BodyTruncated bool
}

type auditRecorder interface {
	Create(ctx context.Context, value audit.Record) error
}

type ledgerReadinessChecker interface {
	CheckLedgerReady() error
}

type routeResolver interface {
	Get(ctx context.Context, id uint64) (modeldomain.Route, error)
	GetByPublicID(ctx context.Context, publicID string) (modeldomain.Route, error)
	GetByPublicIDCandidates(ctx context.Context, publicID string) ([]modeldomain.Route, error)
	GetByProviderUpstream(ctx context.Context, providerValue accountdomain.Provider, upstreamModel string) (modeldomain.Route, error)
}

// mediaAssetStore archives and reads media results produced by media jobs.
type mediaAssetStore interface {
	SaveVideo(ctx context.Context, jobID, contentType string, body io.Reader) (mediadomain.Asset, error)
	OpenVideo(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error)
	SaveImage(ctx context.Context, data []byte) (mediadomain.Asset, error)
	OpenImage(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error)
	OpenInputAsset(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error)
	ReleaseInputAssets(ctx context.Context, references []string) error
}

// Service handles model routing, account selection, failover, and audit finalization.
type Service struct {
	models                      routeResolver
	audits                      auditRecorder
	accounts                    *accountapp.Service
	clientKeys                  *clientkeyapp.Service
	providers                   *provider.Registry
	selector                    *Selector
	responses                   repository.ResponseRepository
	maxAttempts                 atomic.Int64
	videoMaxAttempts            atomic.Int64
	buildForbiddenReauth        atomic.Pointer[buildForbiddenReauthPolicy]
	requestTimeout              atomic.Int64
	mediaJobs                   repository.MediaJobRepository
	mediaAssets                 mediaAssetStore
	defaultImageModel           atomic.Pointer[string]
	mediaQueue                  chan string
	imageQueue                  chan string
	mediaMu                     sync.Mutex
	mediaQueued                 map[string]struct{}
	mediaWorker                 int
	mediaInputSlots             chan struct{}
	mediaQueueFull              atomic.Uint64
	logger                      *slog.Logger
	rateLimitMu                 sync.Mutex
	rateLimitActive             atomic.Bool
	rateLimitNextExpiry         atomic.Int64
	rateLimits                  map[string]teamModelRateLimit
	rateLimitTeams              map[uint64]teamRateLimitObservation
	modelSyncMu                 sync.Mutex
	modelSyncing                map[uint64]struct{}
	markBuildChatDeniedAsReauth atomic.Bool
	qualityRetry                atomic.Pointer[QualityRetryRuntime]
	pricing                     *pricingapp.Service
}

func (s *Service) ConfigureMedia(repository repository.MediaJobRepository, concurrency int) {
	if concurrency <= 0 {
		concurrency = 4
	}
	s.mediaJobs = repository
	s.mediaWorker = concurrency
	s.mediaQueue = make(chan string, min(2048, max(64, concurrency*32)))
	// Image jobs get their own pool of the same size: a long video and a draw
	// never wait for each other's workers.
	s.imageQueue = make(chan string, cap(s.mediaQueue))
	s.mediaInputSlots = make(chan struct{}, min(concurrency, videoInputMaterializeConcurrency))
	s.mediaQueued = make(map[string]struct{})
}

// ConfigureMediaAssets injects optional local media asset archival and reading.
func (s *Service) ConfigureMediaAssets(store mediaAssetStore) {
	s.mediaAssets = store
}

func NewService(models routeResolver, audits auditRecorder, accounts *accountapp.Service, clientKeys *clientkeyapp.Service, providers *provider.Registry, selector *Selector, responses repository.ResponseRepository, maxAttempts int) *Service {
	service := &Service{
		models: models, audits: audits, accounts: accounts, clientKeys: clientKeys, providers: providers,
		selector: selector, responses: responses, logger: slog.Default(),
		rateLimits: make(map[string]teamModelRateLimit), rateLimitTeams: make(map[uint64]teamRateLimitObservation),
		modelSyncing: make(map[uint64]struct{}),
	}
	service.UpdateMaxAttempts(maxAttempts)
	return service
}

// SetPricing enables administrator prices; without it official rates apply.
func (s *Service) SetPricing(pricing *pricingapp.Service) { s.pricing = pricing }

func (s *Service) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

func (s *Service) UpdateMaxAttempts(maxAttempts int) { s.maxAttempts.Store(int64(maxAttempts)) }

// ProviderDefinition returns the registered declaration of a provider.
func (s *Service) ProviderDefinition(value accountdomain.Provider) (provider.Definition, bool) {
	return s.providers.Definition(value)
}

// UpdateVideoMaxAttempts configures create-phase account failover for video jobs.
// 0 is treated as the general default pool size for legacy configs.
func (s *Service) UpdateVideoMaxAttempts(maxAttempts int) {
	s.videoMaxAttempts.Store(int64(maxAttempts))
}

// UpdateMarkBuildChatDeniedAsReauth 热更新 Build chat 永久拒绝是否标 reauthRequired。
// 默认 false：仅模型级冷却；true 时按旧逻辑将账号标为失效并出池。
func (s *Service) UpdateMarkBuildChatDeniedAsReauth(enabled bool) {
	s.markBuildChatDeniedAsReauth.Store(enabled)
}

func (s *Service) UpdateRequestTimeout(value time.Duration) {
	if value <= 0 {
		value = minimumTextBillingReservationTTL
	}
	s.requestTimeout.Store(int64(value))
}

func (s *Service) textBillingReservationTTL() time.Duration {
	ttl := time.Duration(s.requestTimeout.Load()) + finalizationTimeout + billingReservationCrashGrace
	return max(minimumTextBillingReservationTTL, ttl)
}

func (s *Service) checkLedgerReady() error {
	checker, ok := s.audits.(ledgerReadinessChecker)
	if !ok {
		return nil
	}
	if err := checker.CheckLedgerReady(); err != nil {
		return ErrLedgerUnavailable
	}
	return nil
}

// auditRequestSucceeded keeps transport truth (the HTTP status) separate from
// the terminal request outcome. A stream that fails after 2xx headers is not a
// successful request even though its HTTP status remains 2xx.
func auditRequestSucceeded(statusCode int, errorCode string) bool {
	return statusCode >= 200 && statusCode < 300 && errorCode == ""
}

func (s *Service) cancelBillingReservation(eventID string) {
	ctx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
	defer cancel()
	if err := s.clientKeys.CancelBilling(ctx, eventID); err != nil {
		s.logger.Error("billing_reservation_cancel_failed", "event_id", eventID, "error", err)
	}
}

func newAuditEventID() string {
	value, err := security.NewOpaqueToken(18)
	if err != nil || value == "" {
		return fmt.Sprintf("evt_%d", time.Now().UnixNano())
	}
	return "evt_" + value
}

type finalizingBody struct {
	io.ReadCloser
	finalize func()
}

func (b *finalizingBody) Close() error {
	err := b.ReadCloser.Close()
	if b.finalize != nil {
		b.finalize()
	}
	return err
}
