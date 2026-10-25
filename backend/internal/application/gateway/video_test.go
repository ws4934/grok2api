package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	pricingapp "github.com/chenyme/grok2api/backend/internal/application/pricing"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	pricingdomain "github.com/chenyme/grok2api/backend/internal/domain/pricing"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/adobe"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestVideoQuotaModeUsesWeb720pProduct(t *testing.T) {
	tests := []struct {
		provider   account.Provider
		resolution string
		want       string
	}{
		{account.ProviderWeb, "", account.QuotaModeWebVideo720p},
		{account.ProviderWeb, "720p", account.QuotaModeWebVideo720p},
		{account.ProviderWeb, "480p", account.QuotaModeWebVideo},
		{account.ProviderConsole, "720p", account.QuotaModeWebVideo},
	}
	for _, test := range tests {
		if got := grokTestRegistry.VideoQuotaMode(test.provider, account.QuotaModeWebVideo, test.resolution); got != test.want {
			t.Fatalf("videoQuotaMode(%s, %q) = %q, want %q", test.provider, test.resolution, got, test.want)
		}
	}
}

func TestVideoQuotaFinalizationKeepsEffectiveConsumptionFence(t *testing.T) {
	refreshMode, decrementMode, availabilityMode := quotaFinalizationModes(account.QuotaModeWebVideo720p, account.QuotaGroupWebImagine)
	if refreshMode != account.QuotaGroupWebImagine || decrementMode != account.QuotaModeWebVideo720p || availabilityMode != "" {
		t.Fatalf("refresh=%q decrement=%q availability=%q", refreshMode, decrementMode, availabilityMode)
	}
	refreshMode, decrementMode, availabilityMode = quotaFinalizationModes("weekly", account.QuotaGroupWebImagine)
	if refreshMode != "weekly" || decrementMode != "weekly" || availabilityMode != account.QuotaGroupWebImagine {
		t.Fatalf("shared weekly refresh=%q decrement=%q availability=%q", refreshMode, decrementMode, availabilityMode)
	}
}

func TestGetVideoExposesOnlyReadableResultAsset(t *testing.T) {
	completed := media.Job{
		ID: "video_status", ClientKeyID: 7, Status: media.StatusCompleted,
		ResultAssetID: "vid_local", UpstreamURL: "https://assets.grok.com/video.mp4",
	}
	tests := []struct {
		name  string
		store mediaAssetStore
		want  string
	}{
		{
			name: "available",
			store: &videoAssetStoreStub{
				openAsset: media.Asset{ID: "vid_local", Kind: "video", MIMEType: "video/mp4"},
				openData:  []byte("video"),
			},
			want: "vid_local",
		},
		{name: "missing", store: &videoAssetStoreStub{openErr: errors.New("asset missing")}},
		{name: "storage unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{mediaJobs: &videoUsageRepository{job: completed}, mediaAssets: test.store}
			job, err := service.GetVideo(context.Background(), completed.ID, clientkey.Key{ID: completed.ClientKeyID})
			if err != nil {
				t.Fatal(err)
			}
			if job.ResultAssetID != test.want {
				t.Fatalf("result asset ID = %q, want %q", job.ResultAssetID, test.want)
			}
		})
	}
}

func TestVideoEndpointsHideImageJobs(t *testing.T) {
	image := media.Job{ID: "img_job", ClientKeyID: 7, Kind: media.KindImage, Status: media.StatusCompleted, ResultAssetID: "img_asset"}
	service := &Service{mediaJobs: &videoUsageRepository{job: image}}
	key := clientkey.Key{ID: image.ClientKeyID}
	if _, err := service.GetVideo(context.Background(), image.ID, key); !errors.Is(err, ErrResponseNotFound) {
		t.Fatalf("GetVideo(image job) err = %v, want not found", err)
	}
	if _, _, _, err := service.OpenVideoContent(context.Background(), image.ID, key); !errors.Is(err, ErrResponseNotFound) {
		t.Fatalf("OpenVideoContent(image job) err = %v, want not found", err)
	}
}

func TestOpenVideoContentKeepsLocalAssetFastPath(t *testing.T) {
	completed := media.Job{
		ID: "video_content", ClientKeyID: 7, Status: media.StatusCompleted,
		ResultAssetID: "vid_local", UpstreamURL: "https://assets.grok.com/video.mp4",
	}
	service := &Service{
		mediaJobs: &videoUsageRepository{job: completed},
		mediaAssets: &videoAssetStoreStub{
			openAsset: media.Asset{ID: "vid_local", Kind: "video", MIMEType: "video/mp4", SizeBytes: 5},
			openData:  []byte("video"),
		},
	}
	body, contentType, size, err := service.OpenVideoContent(context.Background(), completed.ID, clientkey.Key{ID: completed.ClientKeyID})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "video" || contentType != "video/mp4" || size != 5 {
		t.Fatalf("content = %q, type = %q, size = %d", data, contentType, size)
	}
}

func TestRecoverVideoJobsRetriesUsageWithoutRegeneratingVideo(t *testing.T) {
	completedAt := time.Now().UTC()
	repository := &videoUsageRepository{job: media.Job{
		ID: "video_usage_recovery", RequestID: "request-usage-recovery",
		ClientKeyID: 1, ClientKeyName: "client", AccountID: 2, AccountName: "account",
		Provider: "grok_web", Model: "custom-video", ModelRouteID: 3, UpstreamModel: "Web/grok-imagine-video",
		Seconds: 8, Quality: "720p", Status: media.StatusCompleted, InputImageCount: 2, CreatedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
		Price: &audit.PricingResult{Model: "grok-imagine-video", CostInUSDTicks: 1234},
	}}
	recorder := &durableVideoAuditRecorder{failures: 1}
	service := &Service{mediaJobs: repository, audits: recorder}
	if err := service.RecoverMediaJobs(context.Background()); err == nil {
		t.Fatal("first durable audit failure was ignored")
	}
	if repository.job.UsageRecordedAt != nil {
		t.Fatal("usage was marked before durable audit commit")
	}
	if err := service.RecoverMediaJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.job.UsageRecordedAt == nil || recorder.calls != 2 {
		t.Fatalf("recordedAt = %v, audit calls = %d", repository.job.UsageRecordedAt, recorder.calls)
	}
	if recorder.last.EventID != "video_usage_video_usage_recovery" || recorder.last.EstimatedCostInUSDTicks <= 0 || recorder.last.MediaInputImages != 2 {
		t.Fatalf("audit = %#v", recorder.last)
	}
}

func TestVideoEditRouteUsesResolvedUpstreamInsteadOfPublicName(t *testing.T) {
	routes := []model.Route{
		{ID: 1, PublicID: "grok-imagine-video", Provider: account.ProviderBuild, UpstreamModel: "grok-imagine-video"},
		{ID: 2, PublicID: "company-video-editor", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video"},
		{ID: 3, PublicID: "company-video-editor", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video-1.5"},
	}
	compatible, err := grokPolicyService.routesForVideoOperation(routes, provider.VideoOperationEdit)
	if err != nil {
		t.Fatal(err)
	}
	if len(compatible) != 1 || compatible[0].ID != 2 {
		t.Fatalf("compatible routes = %#v", compatible)
	}
	if _, err := grokPolicyService.routesForVideoOperation(routes[:1], provider.VideoOperationExtend); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("unsupported route error = %v", err)
	}
}

func TestVideoPricingLeavesUnmeasurableOperationsUnpriced(t *testing.T) {
	pricing, priced := (&Service{}).resolveVideoPricing(provider.VideoOperationGenerate, account.ProviderConsole, "Console/grok-imagine-video", "720p", 6, 1)
	if !priced || pricing.CostInUSDTicks <= 0 {
		t.Fatalf("priced generation = %#v, priced=%v", pricing, priced)
	}
	if pricing, priced := (&Service{}).resolveVideoPricing(provider.VideoOperationGenerate, account.ProviderConsole, "Console/grok-imagine-video", "", 6, 0); priced || pricing.CostInUSDTicks != 0 {
		t.Fatalf("generation without resolution = %#v, priced=%v", pricing, priced)
	}
	if pricing, priced := (&Service{}).resolveVideoPricing(provider.VideoOperationExtend, account.ProviderConsole, "Console/grok-imagine-video", "", 6, 0); priced || pricing.CostInUSDTicks != 0 {
		t.Fatalf("extension without measurable input duration = %#v, priced=%v", pricing, priced)
	}
}

// 上游对 Console 视频的这两条限制原先只在 provider 层拦截，而生成接口是异步的，
// 客户端会先拿到 request_id 再从轮询里读到失败任务。入队前校验让错误立刻可见。
func TestVideoRouteParametersRejectConsoleReferenceLimits(t *testing.T) {
	// 实测：8 张 reference_images 上游回 400 "Too many reference images: 8. Maximum allowed is 7."
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderConsole, "grok-imagine-video-1.5", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", ReferenceImages: 8, Duration: 6}); !errors.Is(err, ErrVideoParameterInvalid) {
		t.Fatalf("8 references error = %v", err)
	}
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderConsole, "grok-imagine-video-1.5", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", ReferenceImages: 7, Duration: 6}); err != nil {
		t.Fatalf("7 references error = %v", err)
	}
	// 实测：grok-imagine-video 的 reference-to-video 回 400
	// "Duration 15s exceeds the maximum allowed for reference-to-video, which is 10s."
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderConsole, "grok-imagine-video", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", ReferenceImages: 1, Duration: 15}); !errors.Is(err, ErrVideoParameterInvalid) {
		t.Fatalf("base model reference duration error = %v", err)
	}
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderConsole, "grok-imagine-video", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", ReferenceImages: 1, Duration: 10}); err != nil {
		t.Fatalf("base model 10s reference error = %v", err)
	}
	// image-to-video（无 reference_images）与 1.5 都保持 15s。
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderConsole, "grok-imagine-video", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", HasImage: true, Duration: 15}); err != nil {
		t.Fatalf("base model text/first-frame 15s error = %v", err)
	}
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderConsole, "grok-imagine-video-1.5", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", ReferenceImages: 2, Duration: 15}); err != nil {
		t.Fatalf("1.5 multi-reference 15s error = %v", err)
	}
	// Build 使用相同的 1.5 上游模型名，但支持通用上限 8；不能因同名套用 Console 的 7 张限制。
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderBuild, "grok-imagine-video-1.5", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", ReferenceImages: 8, Duration: 15}); err != nil {
		t.Fatalf("Build 1.5 references error = %v", err)
	}
	// Web 新协议只有文本生视频抓包证据；不能退回已删除的 media-post 旧链路。
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderWeb, "grok-imagine-video", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", Duration: 15}); err != nil {
		t.Fatalf("Web text video error = %v", err)
	}
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderWeb, "grok-imagine-video", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", HasImage: true, Duration: 6}); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("Web image video error = %v", err)
	}
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderWeb, "grok-imagine-video", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", ReferenceImages: 1, Duration: 6}); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("Web reference video error = %v", err)
	}
}

func TestRoutesForVideoParametersKeepsCompatibleSameNameProviders(t *testing.T) {
	routes := []model.Route{
		{ID: 1, PublicID: "shared-video", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video-1.5"},
		{ID: 2, PublicID: "shared-video", Provider: account.ProviderBuild, UpstreamModel: "grok-imagine-video-1.5"},
	}
	compatible, err := grokPolicyService.routesForVideoParameters(routes, provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", ReferenceImages: 8, Duration: 15})
	if err != nil {
		t.Fatal(err)
	}
	if len(compatible) != 1 || compatible[0].ID != 2 {
		t.Fatalf("compatible routes = %#v", compatible)
	}
	if _, err := grokPolicyService.routesForVideoParameters(routes[:1], provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "720p", ReferenceImages: 8, Duration: 15}); !errors.Is(err, ErrVideoParameterInvalid) {
		t.Fatalf("Console-only invalid route error = %v", err)
	}
}

// 不传 duration 时用模型默认值，计费和任务记录也用它：Kling 3.0 默认 5 秒（以前网关先填
// 8 秒，就按 8 秒生成和计费）。
// An over-long prompt is a client error (400), not an upstream failure.
func TestCreateVideoRejectsAnOverlongPromptAsInvalid(t *testing.T) {
	service := NewService(nil, nil, nil, nil, nil, nil, nil, 3)
	service.ConfigureMedia(&videoUsageRepository{}, 1)
	_, err := service.CreateVideo(context.Background(), VideoInput{PublicModel: "grok-imagine-video", Prompt: strings.Repeat("a", 100001)})
	if !errors.Is(err, ErrVideoParameterInvalid) {
		t.Fatalf("err = %v, want ErrVideoParameterInvalid", err)
	}
}

func TestCreateVideoBillsTheParametersTheProviderPicks(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-params.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo, modelRepo, auditRepo, keyRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database), relational.NewClientKeyRepository(database)
	if _, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderAdobe, AuthType: account.AuthTypeOAuth, Name: "adobe", SourceKey: "adobe",
		EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertRoutes(ctx, adobe.Routes()); err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{Name: "video-params", Prefix: "vparams", SecretHash: strings.Repeat("e", 64), EncryptedSecret: "e",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4, ProviderScope: clientkey.AllProviders(), BillingLimitUSDTicks: 100_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(adobe.NewAdapter(adobe.Config{}, nil, nil, nil, nil))
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 1)
	prices, err := pricingapp.NewService(ctx, relational.NewModelPriceRepository(database), func(account.Provider) bool { return true }, registry.PricingModel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prices.Put(ctx, pricingapp.Input{Provider: string(account.ProviderAdobe), PricingModel: "kling/kling_v3", Operation: string(pricingdomain.OperationVideo),
		Rates: map[pricingdomain.Component]string{pricingdomain.OutputSecond: "0.1"}}); err != nil {
		t.Fatal(err)
	}
	service.SetPricing(prices)
	service.ConfigureMedia(relational.NewMediaJobRepository(database), 1)

	job, err := service.CreateVideo(ctx, VideoInput{RequestID: "video-params", ClientKey: key, PublicModel: "adobe/kling-3.0", Prompt: "waves"})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := prices.QuoteVideo(account.ProviderAdobe, "kling/kling_v3", "720p", 5, 0)
	if job.Seconds != 5 || job.Size != "16:9" || job.Quality != "720p" || job.Price == nil || want.CostInUSDTicks == 0 || job.Price.CostInUSDTicks != want.CostInUSDTicks {
		t.Fatalf("job = %ds %s %s price %+v, want 5s 16:9 720p at %d ticks", job.Seconds, job.Size, job.Quality, job.Price, want.CostInUSDTicks)
	}
}

// changeVideoAdapter stands in for a provider whose edit and extension rules
// are not xAI's: it takes any duration and aspect ratio, defaults to 1080p,
// and records what the job runner sends.
type changeVideoAdapter struct {
	requests []provider.VideoRequest
	// read holds each staged source video the provider read through its context.
	read []string
}

func (a *changeVideoAdapter) Provider() account.Provider { return account.ProviderWeb }

func (a *changeVideoAdapter) Definition() provider.Definition {
	definition := testConversationDefinition(account.ProviderWeb)
	definition.Media.VideoGeneration = true
	return definition
}

func (a *changeVideoAdapter) ValidateVideoRequest(provider.VideoRequestOptions) error { return nil }

func (a *changeVideoAdapter) NormalizeVideoRequest(options provider.VideoRequestOptions) (provider.VideoRequestOptions, error) {
	if options.Resolution == "" {
		options.Resolution = "1080p"
	}
	return options, nil
}

func (a *changeVideoAdapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	a.requests = append(a.requests, request)
	if provider.IsInputReference(request.VideoURL) {
		data, _, err := provider.ReadInput(ctx, request.VideoURL)
		a.read = append(a.read, fmt.Sprintf("%q %v", data, err))
	}
	return provider.VideoResult{AssetID: "video_asset_change", ContentType: "video/mp4"}, nil
}

// 编辑与延长的取值归选中路由的 Provider，网关不套 xAI 的规则：20 秒的延长照样入队，
// Provider 补的默认分辨率写进任务，执行时任务里的时长、比例和分辨率原样交给 Provider。
func TestVideoEditAndExtensionFollowTheProviderRules(t *testing.T) {
	for _, test := range []struct {
		input VideoInput
		want  provider.VideoRequest
	}{
		{VideoInput{Operation: provider.VideoOperationExtend, Duration: 20}, provider.VideoRequest{Duration: 20, Resolution: "1080p"}},
		{VideoInput{Operation: provider.VideoOperationEdit, Duration: 5, AspectRatio: "9:16"}, provider.VideoRequest{Duration: 5, AspectRatio: "9:16", Resolution: "1080p"}},
	} {
		t.Run(string(test.input.Operation), func(t *testing.T) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-change.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			accountRepo, modelRepo, auditRepo, keyRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database), relational.NewClientKeyRepository(database)
			credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
				Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper, Name: "change", SourceKey: "change",
				EncryptedAccessToken: "change-token", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
				t.Fatal(err)
			}
			if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
			if err != nil {
				t.Fatal(err)
			}
			key, err := keyRepo.Create(ctx, clientkey.Key{Name: "video-change", Prefix: "vchange", SecretHash: strings.Repeat("d", 64), EncryptedSecret: "d",
				Enabled: true, RPMLimit: 60, MaxConcurrent: 4, ProviderScope: clientkey.AllProviders()})
			if err != nil {
				t.Fatal(err)
			}
			adapter := &changeVideoAdapter{}
			registry := provider.NewRegistry(adapter)
			sticky := memory.NewStickyStore()
			accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
			selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
			service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 1)
			service.ConfigureMedia(relational.NewMediaJobRepository(database), 1)

			input := test.input
			input.RequestID, input.ClientKey, input.PublicModel = "video-change", key, route.PublicID
			input.Prompt, input.VideoURL = "change it", "https://example.com/source.mp4"
			job, err := service.CreateVideo(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if job.Seconds != test.want.Duration || job.Size != test.want.AspectRatio || job.Quality != test.want.Resolution {
				t.Fatalf("job = %ds %q %q", job.Seconds, job.Size, job.Quality)
			}
			service.processMediaJob(ctx, job.ID)
			if len(adapter.requests) != 1 {
				t.Fatalf("provider calls = %d", len(adapter.requests))
			}
			if got := adapter.requests[0]; got.Operation != input.Operation || got.Duration != test.want.Duration || got.AspectRatio != test.want.AspectRatio || got.Resolution != test.want.Resolution {
				t.Fatalf("request = %s %ds %q %q", got.Operation, got.Duration, got.AspectRatio, got.Resolution)
			}
		})
	}
}

// An inline source video is staged when the job is created, read from disk by
// the provider, and deleted once the job is done.
func TestVideoJobStagesInlineInputsAndReleasesThem(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-staged.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo, modelRepo, auditRepo, keyRepo := relational.NewAccountRepository(database), relational.NewModelRepository(database), relational.NewAuditRepository(database), relational.NewClientKeyRepository(database)
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper, Name: "staged", SourceKey: "staged",
		EncryptedAccessToken: "staged-token", ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
	if err != nil {
		t.Fatal(err)
	}
	key, err := keyRepo.Create(ctx, clientkey.Key{Name: "video-staged", Prefix: "vstaged", SecretHash: strings.Repeat("f", 64), EncryptedSecret: "f",
		Enabled: true, RPMLimit: 60, MaxConcurrent: 4, ProviderScope: clientkey.AllProviders()})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &changeVideoAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 1)
	jobs := relational.NewMediaJobRepository(database)
	service.ConfigureMedia(jobs, 1)
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	service.ConfigureMediaAssets(mediaapp.NewService(relational.NewMediaAssetRepository(database), jobs, objects, nil, mediaapp.Config{PublicBaseURL: "https://example.test", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 90, CleanupInterval: time.Hour}))

	clip := "\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2"
	job, err := service.CreateVideo(ctx, VideoInput{
		RequestID: "video-staged", ClientKey: key, PublicModel: route.PublicID, Operation: provider.VideoOperationEdit, Prompt: "change it",
		VideoURL: "data:video/mp4;base64," + base64.StdEncoding.EncodeToString([]byte(clip)),
	})
	if err != nil {
		t.Fatal(err)
	}
	input, err := jobs.GetMediaJobInput(ctx, job.ID)
	if err != nil || strings.Contains(input, "base64") {
		t.Fatalf("job input = %q err = %v, want only a staged reference", input, err)
	}
	fileID, ok := media.ParseInputReference(decodeVideoInputs(input).videoURL)
	if !ok {
		t.Fatalf("job input = %q names no staged file", input)
	}
	service.processMediaJob(ctx, job.ID)
	if want := fmt.Sprintf("%q %v", clip, error(nil)); len(adapter.read) != 1 || adapter.read[0] != want {
		t.Fatalf("provider read = %v, want [%s]", adapter.read, want)
	}
	if _, _, err := service.mediaAssets.OpenInputAsset(ctx, fileID); err == nil {
		t.Fatal("the finished job kept its staged input")
	}
}

func TestCreateVideoAppliesRouteConstraintsAfterKeyEligibilityAndBeforeInputIO(t *testing.T) {
	routes := []model.Route{
		{ID: 1, PublicID: "shared-video", Provider: account.ProviderBuild, UpstreamModel: "grok-imagine-video-1.5", Capability: model.CapabilityResponses},
		{ID: 2, PublicID: "shared-video", Provider: account.ProviderBuild, UpstreamModel: "grok-imagine-video-1.5", Capability: model.CapabilityVideo},
		{ID: 3, PublicID: "shared-video", Provider: account.ProviderConsole, UpstreamModel: "grok-imagine-video-1.5", Capability: model.CapabilityVideo},
	}
	jobs := &videoUsageRepository{}
	assets := &videoAssetStoreStub{inputID: "unused", inputData: []byte("unused")}
	service := &Service{
		models:     &aliasRouteResolver{byPublic: map[string][]model.Route{"shared-video": routes}},
		clientKeys: clientkeyapp.NewService(nil, nil, nil, 60, 4, nil),
		providers:  provider.NewRegistry(consoleVideoAdmissionAdapter{}),
		mediaJobs:  jobs, mediaAssets: assets, mediaQueue: make(chan string, 1), mediaQueued: make(map[string]struct{}),
		logger: slog.Default(),
	}
	references := make([]string, provider.ConsoleVideoMaxReferenceImages+1)
	for index := range references {
		references[index] = VideoInputFileReference(fmt.Sprintf("unused_%d", index))
	}
	_, err := service.CreateVideo(context.Background(), VideoInput{
		PublicModel: "shared-video", Prompt: "animate", Duration: 6, Resolution: "720p",
		ReferenceURLs: references,
		ClientKey: clientkey.Key{
			ProviderScope: clientkey.ProviderScopeOf(account.ProviderConsole),
			AllowedModels: []uint64{3},
		},
	})
	if !errors.Is(err, ErrVideoParameterInvalid) {
		t.Fatalf("CreateVideo error = %v", err)
	}
	if assets.openInputCalls != 0 || jobs.createCalls != 0 || len(service.mediaQueue) != 0 || len(service.mediaQueued) != 0 {
		t.Fatalf("side effects: input opens=%d job creates=%d queue=%d queued-set=%d", assets.openInputCalls, jobs.createCalls, len(service.mediaQueue), len(service.mediaQueued))
	}
}

func TestVideo1080pValidationUsesResolvedUpstreamModel(t *testing.T) {
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderConsole, "grok-imagine-video-1.5", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "1080P", Duration: 6}); err != nil {
		t.Fatalf("1.5 text/image 1080p rejected: %v", err)
	}
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderConsole, "grok-imagine-video", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "1080p", Duration: 6}); !errors.Is(err, ErrVideoOperationUnsupported) {
		t.Fatalf("legacy 1080p error = %v", err)
	}
	// 参考图模式的 1080p 先被 xAI 取值规则拦下（与原先接口层的 400 invalid_request 一致）。
	if err := grokPolicyService.validateVideoRouteParameters(account.ProviderConsole, "grok-imagine-video-1.5", provider.VideoRequestOptions{Operation: provider.VideoOperationGenerate, Resolution: "1080p", ReferenceImages: 1, Duration: 6}); !errors.Is(err, ErrVideoParameterInvalid) {
		t.Fatalf("reference 1080p error = %v", err)
	}
}

func TestEncodeVideoInputEnforcesPersistedLimit(t *testing.T) {
	atLimit := strings.Repeat("A", media.MaxInputJSONBytes-len(`{"image_url":""}`))
	encoded, err := encodeVideoInputs(videoInputs{imageURL: atLimit})
	if err != nil {
		t.Fatalf("encode at limit: %v", err)
	}
	if len(encoded) != media.MaxInputJSONBytes {
		t.Fatalf("encoded len=%d, want exactly the limit", len(encoded))
	}
	if _, err := encodeVideoInputs(videoInputs{imageURL: atLimit + "A"}); !errors.Is(err, ErrVideoInputTooLarge) {
		t.Fatalf("oversized input error = %v", err)
	}
}

func TestEncodeDecodeVideoInputPreservesImageAndReferences(t *testing.T) {
	encoded, err := encodeVideoInputs(videoInputs{imageURL: "https://example.com/first.png", referenceURLs: []string{"https://example.com/ref.png"}})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeVideoInputs(encoded)
	imageURL, refs := decoded.imageURL, decoded.referenceURLs
	if imageURL != "https://example.com/first.png" || len(refs) != 1 || refs[0] != "https://example.com/ref.png" {
		t.Fatalf("decoded split = %q %#v from %s", imageURL, refs, encoded)
	}

	encoded, err = encodeVideoInputs(videoInputs{referenceURLs: []string{"https://example.com/ref-only.png"}})
	if err != nil {
		t.Fatal(err)
	}
	decoded = decodeVideoInputs(encoded)
	imageURL, refs = decoded.imageURL, decoded.referenceURLs
	if imageURL != "" || len(refs) != 1 || refs[0] != "https://example.com/ref-only.png" {
		t.Fatalf("single reference decoded = %q %#v from %s", imageURL, refs, encoded)
	}
}

// 尾帧单独保存，参与临时输入的物化与回收。
func TestEncodeDecodeVideoInputPreservesLastFrame(t *testing.T) {
	encoded, err := encodeVideoInputs(videoInputs{imageURL: "https://example.com/first.png", lastFrameURL: media.InputReference("last")})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeVideoInputs(encoded)
	if decoded.imageURL != "https://example.com/first.png" || decoded.lastFrameURL != media.InputReference("last") || len(decoded.referenceURLs) != 0 {
		t.Fatalf("decoded = %+v from %s", decoded, encoded)
	}
	if all := decodeVideoInput(encoded); len(all) != 2 || all[1] != media.InputReference("last") {
		t.Fatalf("job inputs = %#v, want first and last frame", all)
	}
}

// 参考视频单独保存，参与物化与回收。
func TestEncodeDecodeVideoInputPreservesReferenceVideos(t *testing.T) {
	encoded, err := encodeVideoInputs(videoInputs{referenceVideoURLs: []string{media.InputReference("clip"), " https://example.com/b.mp4 "}})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeVideoInputs(encoded)
	if len(decoded.referenceVideoURLs) != 2 || decoded.referenceVideoURLs[1] != "https://example.com/b.mp4" || decoded.imageURL != "" {
		t.Fatalf("decoded = %+v from %s", decoded, encoded)
	}
	if all := decodeVideoInput(encoded); len(all) != 2 || all[0] != media.InputReference("clip") {
		t.Fatalf("job inputs = %#v", all)
	}
}

// 图像与视频输入共用一个物化预算：各自不超但合计超出时拒绝。
func TestVideoInputBudgetIsSharedAcrossKinds(t *testing.T) {
	store := &videoAssetStoreStub{inputID: "input_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", inputData: []byte("x"), inputSize: 14 << 20}
	service := &Service{mediaAssets: store}
	reference := VideoInputFileReference(store.inputID)
	budget := videoInputJSONBaseBytes
	if err := service.validateInputBudget(context.Background(), []string{reference}, "image", &budget); err != nil {
		t.Fatal(err)
	}
	fresh := videoInputJSONBaseBytes
	if err := service.validateInputBudget(context.Background(), []string{reference}, "image", &fresh); err != nil {
		t.Fatalf("a single 14 MiB input must fit its own budget: %v", err)
	}
	if err := service.validateInputBudget(context.Background(), []string{reference}, "image", &budget); !errors.Is(err, ErrVideoInputTooLarge) {
		t.Fatalf("two 14 MiB inputs on one budget = %v, want ErrVideoInputTooLarge", err)
	}
}

func TestEncodeDecodeVideoInputPreservesReferenceAudioAndVideo(t *testing.T) {
	encoded, err := encodeVideoInputs(videoInputs{
		referenceURLs: []string{"https://example.com/ref.png"}, referenceAudios: []string{"eve", " ara "},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeVideoInputs(encoded)
	imageURL, refs, audios, videoURL := decoded.imageURL, decoded.referenceURLs, decoded.referenceAudios, decoded.videoURL
	if imageURL != "" || videoURL != "" || len(refs) != 1 || refs[0] != "https://example.com/ref.png" || len(audios) != 2 || audios[0] != "eve" || audios[1] != "ara" {
		t.Fatalf("decoded reference input = image %q refs %#v audios %#v video %q from %s", imageURL, refs, audios, videoURL, encoded)
	}

	encoded, err = encodeVideoInputs(videoInputs{videoURL: media.InputReference("source-video")})
	if err != nil {
		t.Fatal(err)
	}
	videoURL = decodeVideoInputs(encoded).videoURL
	if videoURL != media.InputReference("source-video") {
		t.Fatalf("extension video = %q", videoURL)
	}
}

func TestRecoverVideoJobsRecordsFailedAuditWithEgress(t *testing.T) {
	completedAt := time.Now().UTC()
	nodeID := uint64(42)
	repository := &videoUsageRepository{job: media.Job{
		ID: "video_failed_recovery", RequestID: "request-failed-recovery",
		ClientKeyID: 1, ClientKeyName: "client", ClientIP: "203.0.113.9", AccountID: 2, AccountName: "account",
		Provider: "grok_web", Model: "grok-imagine-video", ModelRouteID: 3, UpstreamModel: "video",
		Seconds: 8, Quality: "720p", Status: media.StatusFailed, ErrorCode: "generation_failed", ErrorMessage: "upstream disconnected",
		EgressNodeID: &nodeID, EgressNodeName: "warp", EgressScope: "grok_web", EgressMode: "proxy",
		InputJSON: `{}`, CreatedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
	}}
	recorder := &durableVideoAuditRecorder{}
	service := &Service{mediaJobs: repository, audits: recorder}
	if err := service.RecoverMediaJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.job.UsageRecordedAt == nil || recorder.calls != 1 {
		t.Fatalf("recordedAt = %v, audit calls = %d", repository.job.UsageRecordedAt, recorder.calls)
	}
	if recorder.last.ClientIP != "203.0.113.9" || recorder.last.StatusCode != 502 || recorder.last.ErrorCode != "generation_failed" || recorder.last.EgressNodeID == nil || *recorder.last.EgressNodeID != nodeID || recorder.last.EgressNodeName != "warp" || recorder.last.EgressMode != audit.EgressModeProxy {
		t.Fatalf("audit = %#v", recorder.last)
	}
	if recorder.last.EstimatedCostInUSDTicks != 0 || recorder.last.MediaOutputSeconds != 0 {
		t.Fatalf("failed job was billed: %#v", recorder.last)
	}
}

func TestRecoverVideoJobsRecordsDetachedAccountSnapshot(t *testing.T) {
	completedAt := time.Now().UTC()
	repository := &videoUsageRepository{job: media.Job{
		ID: "video_detached_account", RequestID: "request-detached-account",
		ClientKeyID: 1, ClientKeyName: "client", AccountName: "deleted account",
		Provider: "grok_web", Model: "grok-imagine-video", ModelRouteID: 3, UpstreamModel: "video",
		Seconds: 8, Quality: "720p", Status: media.StatusFailed, ErrorCode: "generation_failed",
		InputJSON: `{}`, CreatedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
	}}
	recorder := &durableVideoAuditRecorder{}
	service := &Service{mediaJobs: repository, audits: recorder}
	if err := service.RecoverMediaJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorder.last.AccountID != nil || recorder.last.AccountName != "deleted account" {
		t.Fatalf("detached account audit = %#v", recorder.last)
	}
}

func TestLogVideoGenerationFailurePreservesUpstreamDiagnostic(t *testing.T) {
	var output bytes.Buffer
	service := &Service{logger: slog.New(slog.NewTextHandler(&output, nil))}
	nodeID := uint64(7)
	service.logVideoGenerationFailure(media.Job{
		ID: "video_failure", RequestID: "request-failure", UpstreamModel: "grok-imagine-video",
		EgressNodeID: &nodeID, EgressNodeName: "proxy-1", EgressScope: "grok_web", EgressMode: "proxy",
	}, account.Credential{ID: 42, Provider: account.ProviderWeb}, videoStatusError{
		status:  http.StatusForbidden,
		message: "Grok Web 媒体上游返回 403: upload denied access_token=secret https://assets.grok.com/video?token=secret",
	})
	logLine := output.String()
	for _, expected := range []string{
		"msg=video_generation_failed", "job_id=video_failure", "request_id=request-failure",
		"account_id=42", "provider=grok_web", "upstream_status=403", "upload denied",
		"egress_node_id=7", "egress_node_name=proxy-1",
	} {
		if !strings.Contains(logLine, expected) {
			t.Fatalf("log missing %q: %s", expected, logLine)
		}
	}
	for _, secret := range []string{"access_token=secret", "token=secret"} {
		if strings.Contains(logLine, secret) {
			t.Fatalf("log exposed %q: %s", secret, logLine)
		}
	}
}

type videoStatusError struct {
	status  int
	message string
}

func (e videoStatusError) Error() string       { return e.message }
func (e videoStatusError) HTTPStatusCode() int { return e.status }

func TestVideoQueueIsBoundedAndDeduplicated(t *testing.T) {
	service := &Service{}
	service.ConfigureMedia(&videoUsageRepository{}, 1)
	capacity := cap(service.mediaQueue)
	for index := range capacity {
		if !service.enqueueMediaJob(fmt.Sprintf("video_%d", index)) {
			t.Fatalf("enqueue %d failed before capacity", index)
		}
	}
	if !service.enqueueMediaJob("video_0") {
		t.Fatal("duplicate queued job should be treated as accepted")
	}
	if service.enqueueMediaJob("video_overflow") {
		t.Fatal("queue accepted a job beyond its capacity")
	}
}

func TestPersistRemoteVideoRetriesSameResultWithoutRegeneration(t *testing.T) {
	adapter := &videoPersistAdapter{failures: 1}
	store := &videoAssetStoreStub{}
	service := &Service{mediaAssets: store}
	credential := account.Credential{ID: 42, Provider: account.ProviderWeb}
	result, err := service.persistRemoteVideo(context.Background(), "video_job", adapter, credential, provider.VideoResult{URL: "https://assets.grok.com/video.mp4", ContentType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.generateCalls != 0 || adapter.downloadCalls != 2 || adapter.lastCredentialID != credential.ID {
		t.Fatalf("generate=%d download=%d credential=%d", adapter.generateCalls, adapter.downloadCalls, adapter.lastCredentialID)
	}
	if store.saveCalls != 1 || result.AssetID != "vid_local" || result.ContentType != "video/mp4" {
		t.Fatalf("store calls=%d result=%#v", store.saveCalls, result)
	}
}

// A staged input is checked before the job runs and read from disk by the
// adapter; the job never carries its bytes.
func TestStagedVideoInputIsCheckedAndOpenedFromDisk(t *testing.T) {
	raw := []byte("png-bytes")
	inputID := "input_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	store := &videoAssetStoreStub{inputID: inputID, inputData: raw}
	service := &Service{mediaAssets: store}
	reference := VideoInputFileReference(inputID)
	validate := func(references []string, kind string) error {
		budget := videoInputJSONBaseBytes
		return service.validateInputBudget(context.Background(), references, kind, &budget)
	}
	if err := validate([]string{"https://example.com/a.png", reference}, "image"); err != nil {
		t.Fatal(err)
	}
	if err := validate([]string{reference}, "video"); !errors.Is(err, ErrVideoInputUnavailable) {
		t.Fatalf("image accepted as video input: %v", err)
	}
	ctx := provider.WithInputOpener(context.Background(), service.openStagedInput)
	if data, mimeType, err := provider.ReadInput(ctx, reference); err != nil || string(data) != string(raw) || mimeType != "image/png" {
		t.Fatalf("read staged input = %q %q %v", data, mimeType, err)
	}
	if _, err := provider.OpenInput(ctx, VideoInputFileReference("missing")); !errors.Is(err, provider.ErrInputUnavailable) {
		t.Fatalf("missing staged input open error = %v", err)
	}
	if err := validate([]string{VideoInputFileReference("missing")}, "image"); !errors.Is(err, ErrVideoInputUnavailable) {
		t.Fatalf("missing input error=%v", err)
	}
	store.inputSize = 20 << 20
	if err := validate([]string{reference, reference}, "image"); !errors.Is(err, ErrVideoInputTooLarge) {
		t.Fatalf("aggregate local input error=%v", err)
	}
}

func TestInputStagerStagesDataURIsAndAbortReleasesThem(t *testing.T) {
	store := &videoAssetStoreStub{}
	stager := inputStager{service: &Service{mediaAssets: store}}
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\npixels"))
	in, err := stager.stageVideoInputs(context.Background(), videoInputs{
		imageURL: png, referenceURLs: []string{"https://example.com/ref.png"}, referenceAudios: []string{"eve"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !provider.IsInputReference(in.imageURL) || in.referenceURLs[0] != "https://example.com/ref.png" || in.referenceAudios[0] != "eve" {
		t.Fatalf("staged inputs = %#v", in)
	}
	if string(store.inputData) != "\x89PNG\r\n\x1a\npixels" || store.inputKind != "image" {
		t.Fatalf("saved %q as %q", store.inputData, store.inputKind)
	}
	stager.abort()
	if len(store.released) != 1 || !slices.Equal(store.released[0], []string{in.imageURL}) {
		t.Fatalf("released = %v, want the staged file", store.released)
	}
	for _, bad := range []string{"data:image/png,AAAA", "data:image/png;base64,%%%"} {
		if _, err := stager.stage(context.Background(), bad, "image"); !errors.Is(err, ErrMediaInputInvalid) {
			t.Fatalf("%q: error = %v, want ErrMediaInputInvalid", bad, err)
		}
	}
}

type videoPersistAdapter struct {
	failures         int
	generateCalls    int
	downloadCalls    int
	lastCredentialID uint64
}

type consoleVideoAdmissionAdapter struct{}

func (consoleVideoAdmissionAdapter) Provider() account.Provider { return account.ProviderConsole }

func (consoleVideoAdmissionAdapter) GenerateVideo(context.Context, provider.VideoRequest) (provider.VideoResult, error) {
	return provider.VideoResult{}, errors.New("unexpected Console video generation")
}

// ValidateVideoRequest mirrors the production Console adapter.
func (consoleVideoAdmissionAdapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	return grokTestRegistry.ValidateVideoRequest(account.ProviderConsole, options)
}

func (a *videoPersistAdapter) Provider() account.Provider { return account.ProviderWeb }

func (a *videoPersistAdapter) GenerateVideo(context.Context, provider.VideoRequest) (provider.VideoResult, error) {
	a.generateCalls++
	return provider.VideoResult{}, errors.New("must not regenerate")
}

func (a *videoPersistAdapter) DownloadVideo(_ context.Context, credential account.Credential, _ string) (io.ReadCloser, string, int64, error) {
	a.downloadCalls++
	a.lastCredentialID = credential.ID
	if a.downloadCalls <= a.failures {
		return nil, "", 0, errors.New("temporary download failure")
	}
	return io.NopCloser(strings.NewReader("video")), "video/mp4", 5, nil
}

type videoAssetStoreStub struct {
	saveCalls      int
	openInputCalls int
	openAsset      media.Asset
	openData       []byte
	openErr        error
	inputID        string
	inputData      []byte
	inputSize      int64
	inputKind      string
	inputMIME      string
	released       [][]string
}

func (s *videoAssetStoreStub) SaveVideo(_ context.Context, jobID, contentType string, body io.Reader) (media.Asset, error) {
	s.saveCalls++
	if jobID != "video_job" {
		return media.Asset{}, fmt.Errorf("job ID = %s", jobID)
	}
	if contentType != "video/mp4" {
		return media.Asset{}, fmt.Errorf("content type = %s", contentType)
	}
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "video" {
		return media.Asset{}, fmt.Errorf("video body = %q: %w", data, err)
	}
	return media.Asset{ID: "vid_local", Kind: "video", MIMEType: "video/mp4", SizeBytes: int64(len(data))}, nil
}

func (s *videoAssetStoreStub) OpenVideo(_ context.Context, id string) (media.Asset, io.ReadCloser, error) {
	if s.openErr != nil {
		return media.Asset{}, nil, s.openErr
	}
	if s.openAsset.ID == "" || id != s.openAsset.ID {
		return media.Asset{}, nil, errors.New("not implemented")
	}
	return s.openAsset, io.NopCloser(bytes.NewReader(s.openData)), nil
}

func (s *videoAssetStoreStub) OpenInputAsset(_ context.Context, id string) (media.Asset, io.ReadCloser, error) {
	s.openInputCalls++
	if id != s.inputID || len(s.inputData) == 0 {
		return media.Asset{}, nil, errors.New("not implemented")
	}
	size := s.inputSize
	if size <= 0 {
		size = int64(len(s.inputData))
	}
	kind := s.inputKind
	if kind == "" {
		kind = "image"
	}
	mimeType := s.inputMIME
	if mimeType == "" {
		mimeType = "image/png"
	}
	return media.Asset{ID: id, Kind: kind, MIMEType: mimeType, SizeBytes: size}, io.NopCloser(bytes.NewReader(s.inputData)), nil
}

func (s *videoAssetStoreStub) ReleaseInputAssets(_ context.Context, references []string) error {
	s.released = append(s.released, references)
	return nil
}

func (s *videoAssetStoreStub) SaveInputImage(_ context.Context, data []byte) (media.Asset, error) {
	return s.saveInput(data, "image", "image/png")
}

func (s *videoAssetStoreStub) SaveInputVideo(_ context.Context, contentType string, body io.Reader) (media.Asset, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return media.Asset{}, err
	}
	return s.saveInput(data, "video", contentType)
}

// saveInput keeps the one staged input the stub can open.
func (s *videoAssetStoreStub) saveInput(data []byte, kind, mimeType string) (media.Asset, error) {
	s.inputID, s.inputData, s.inputKind, s.inputMIME = "input_"+strings.Repeat("B", 32), data, kind, mimeType
	return media.Asset{ID: s.inputID, Kind: kind, MIMEType: mimeType, SizeBytes: int64(len(data))}, nil
}

func (*videoAssetStoreStub) SaveImage(context.Context, []byte) (media.Asset, error) {
	return media.Asset{}, errors.New("not implemented")
}

func (*videoAssetStoreStub) OpenImage(context.Context, string) (media.Asset, io.ReadCloser, error) {
	return media.Asset{}, nil, errors.New("not implemented")
}

type durableVideoAuditRecorder struct {
	failures int
	calls    int
	last     audit.Record
}

func (r *durableVideoAuditRecorder) Create(_ context.Context, value audit.Record) error {
	r.calls++
	r.last = value
	if r.calls <= r.failures {
		return errors.New("database unavailable")
	}
	return nil
}

type videoUsageRepository struct {
	job         media.Job
	createCalls int
}

func (r *videoUsageRepository) CreateMediaJob(_ context.Context, value media.Job) error {
	r.createCalls++
	r.job = value
	return nil
}

func (r *videoUsageRepository) GetMediaJob(context.Context, string, uint64) (media.Job, error) {
	return r.job, nil
}

func (r *videoUsageRepository) GetMediaJobsByIDs(context.Context, []string) ([]media.Job, error) {
	return []media.Job{r.job}, nil
}

func (r *videoUsageRepository) UpdateMediaJob(context.Context, media.Job) error { return nil }

func (r *videoUsageRepository) DeleteMediaJob(context.Context, string) error { return nil }

func (r *videoUsageRepository) ListMediaJobs(context.Context, repository.MediaJobListQuery) ([]media.Job, int64, error) {
	return nil, 0, nil
}

func (r *videoUsageRepository) SummarizeMediaJobs(context.Context, string) (repository.MediaJobStats, error) {
	return repository.MediaJobStats{}, nil
}

func (r *videoUsageRepository) ListRecoverableMediaJobs(context.Context, int) ([]media.Job, error) {
	return nil, nil
}

func (r *videoUsageRepository) ListUnrecordedTerminalMediaJobs(context.Context, int) ([]media.Job, error) {
	if r.job.UsageRecordedAt != nil || (r.job.Status != media.StatusCompleted && r.job.Status != media.StatusFailed) {
		return nil, nil
	}
	return []media.Job{r.job}, nil
}

func (r *videoUsageRepository) TryClaimMediaJob(context.Context, string, time.Time, time.Time, string) (media.Job, bool, error) {
	return media.Job{}, false, nil
}

func (r *videoUsageRepository) MarkMediaJobUsageRecorded(_ context.Context, _ string, recordedAt time.Time) error {
	r.job.UsageRecordedAt = &recordedAt
	return nil
}

func (r *videoUsageRepository) GetMediaJobByIdempotencyKey(context.Context, uint64, string) (media.Job, error) {
	return media.Job{}, repository.ErrNotFound
}

func (r *videoUsageRepository) GetMediaJobInput(context.Context, string) (string, error) {
	return r.job.InputJSON, nil
}

func (r *videoUsageRepository) RenewMediaJobLease(context.Context, media.Job, time.Time) error {
	return nil
}

func (r *videoUsageRepository) CancelMediaJob(context.Context, string, uint64, time.Time) (media.Job, error) {
	return r.job, repository.ErrConflict
}

func TestResolveVideoAuditStatusCodePrefersUpstream429(t *testing.T) {
	status429 := http.StatusTooManyRequests
	job := media.Job{Status: media.StatusFailed, ErrorCode: "generation_failed", ErrorMessage: "Console 媒体上游返回 429: Too many requests"}
	if got := resolveVideoAuditStatusCode(job, 0, nil); got != http.StatusTooManyRequests {
		t.Fatalf("message status = %d", got)
	}
	if got := resolveVideoAuditStatusCode(job, http.StatusTooManyRequests, nil); got != http.StatusTooManyRequests {
		t.Fatalf("explicit status = %d", got)
	}
	attempts := []audit.Attempt{{UpstreamStatusCode: &status429}}
	job.ErrorMessage = "wrapped"
	if got := resolveVideoAuditStatusCode(job, 0, attempts); got != http.StatusTooManyRequests {
		t.Fatalf("attempt status = %d", got)
	}
	job.ErrorCode = "rate_limited"
	job.ErrorMessage = ""
	if got := resolveVideoAuditStatusCode(job, 0, nil); got != http.StatusTooManyRequests {
		t.Fatalf("code status = %d", got)
	}
}

func TestVideoAttemptPolicyStandaloneAndUnlimited(t *testing.T) {
	service := &Service{}
	service.UpdateMaxAttempts(3)
	service.UpdateVideoMaxAttempts(-1)
	policy := service.videoAttemptPolicy()
	if !policy.unlimited {
		t.Fatalf("unlimited video policy = %#v", policy)
	}
	service.UpdateVideoMaxAttempts(5)
	policy = service.videoAttemptPolicy()
	if policy.unlimited || policy.limit != 5 {
		t.Fatalf("standalone policy = %#v", policy)
	}
}

type videoCreateFailoverAdapter struct {
	mu       sync.Mutex
	failures map[uint64]int
	status   int
	// scoped makes the failure a request-scoped rejection.
	scoped   bool
	attempts []uint64
}

func (a *videoCreateFailoverAdapter) Provider() account.Provider { return account.ProviderWeb }

func (a *videoCreateFailoverAdapter) Definition() provider.Definition {
	definition := testConversationDefinition(account.ProviderWeb)
	definition.Media.VideoGeneration = true
	return definition
}

func (a *videoCreateFailoverAdapter) GenerateVideo(_ context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	a.mu.Lock()
	a.attempts = append(a.attempts, request.Credential.ID)
	remaining := a.failures[request.Credential.ID]
	if remaining > 0 {
		a.failures[request.Credential.ID] = remaining - 1
	}
	a.mu.Unlock()
	if remaining > 0 {
		if a.status == 0 {
			return provider.VideoResult{}, errors.New("unclassified create failure")
		}
		if a.scoped {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageCreate, a.status, videoScopedError{videoHTTPStatusError{status: a.status}})
		}
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageCreate, a.status, videoHTTPStatusError{status: a.status})
	}
	return provider.VideoResult{AssetID: "video_asset_00001", ContentType: "video/mp4"}, nil
}

func (a *videoCreateFailoverAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}

type videoHTTPStatusError struct{ status int }

func (e videoHTTPStatusError) Error() string       { return http.StatusText(e.status) }
func (e videoHTTPStatusError) HTTPStatusCode() int { return e.status }

type videoScopedError struct{ videoHTTPStatusError }

func (videoScopedError) RequestScopedFailure() bool { return true }

func TestVideoWebForbiddenRetriesPinnedAccountOnceThenFailsOver(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	mediaRepo := relational.NewMediaJobRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "video-test", Prefix: "video-test", SecretHash: strings.Repeat("a", 64),
		EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	createAccount := func(name string, priority int) account.Credential {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
			Name: name, SourceKey: name, EncryptedAccessToken: name + "-token", ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: priority, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return credential
	}
	first := createAccount("first", 200)
	second := createAccount("second", 100)
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []uint64{first.ID, second.ID} {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, accountID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
	if err != nil {
		t.Fatal(err)
	}

	adapter := &videoCreateFailoverAdapter{
		failures: map[uint64]int{first.ID: 2},
		status:   http.StatusForbidden,
	}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	limiter := memory.NewConcurrencyLimiter()
	selector := NewSelector(accountRepo, limiter, sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 3)
	service.ConfigureMedia(mediaRepo, 1)
	service.UpdateVideoMaxAttempts(10)
	// A failed job must give the slot back: these accounts allow one request.
	assertSlotFree := func(credential account.Credential) {
		t.Helper()
		if held, _ := limiter.Current(ctx, repository.AccountConcurrencyKey(credential.ID)); held != 0 {
			t.Fatalf("account %s still holds %d concurrency slots", credential.Name, held)
		}
	}

	now := time.Now().UTC()
	job := media.Job{
		ID: "video_forbidden_retry", RequestID: "request-video-retry", ClientKeyID: key.ID, ClientKeyName: key.Name,
		AccountID: first.ID, AccountName: first.Name, Provider: string(account.ProviderWeb),
		Model: route.PublicID, ModelRouteID: route.ID, UpstreamModel: route.UpstreamModel,
		Operation: provider.VideoOperationGenerate, Prompt: "test", Seconds: 5, Quality: "720p",
		Status: media.StatusInProgress, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := mediaRepo.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	service.runVideoJob(ctx, job, route)

	if attempts := adapter.Attempts(); len(attempts) != 3 || attempts[0] != first.ID || attempts[1] != first.ID || attempts[2] != second.ID {
		t.Fatalf("video attempts = %#v, want first, first, second", attempts)
	}
	stored, err := mediaRepo.GetMediaJob(ctx, job.ID, job.ClientKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != media.StatusCompleted || stored.AccountID != second.ID || stored.ResultAssetID != "video_asset_00001" {
		t.Fatalf("completed job = %#v", stored)
	}

	adapter.mu.Lock()
	adapter.failures = map[uint64]int{first.ID: 1}
	adapter.status = 0
	adapter.attempts = nil
	adapter.mu.Unlock()
	unknownJob := job
	unknownJob.ID = "video_unclassified_failure"
	unknownJob.RequestID = "request-video-unclassified"
	unknownJob.AccountID = first.ID
	unknownJob.AccountName = first.Name
	unknownJob.Status = media.StatusInProgress
	unknownJob.Progress = 0
	unknownJob.ResultAssetID = ""
	unknownJob.ContentType = ""
	unknownJob.CompletedAt = nil
	unknownJob.CreatedAt = time.Now().UTC()
	unknownJob.UpdatedAt = unknownJob.CreatedAt
	if err := mediaRepo.CreateMediaJob(ctx, unknownJob); err != nil {
		t.Fatal(err)
	}
	service.runVideoJob(ctx, unknownJob, route)
	if attempts := adapter.Attempts(); len(attempts) != 1 || attempts[0] != first.ID {
		t.Fatalf("unclassified failure attempts = %#v, want pinned account once", attempts)
	}
	stored, err = mediaRepo.GetMediaJob(ctx, unknownJob.ID, unknownJob.ClientKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != media.StatusFailed || stored.AccountID != first.ID {
		t.Fatalf("unclassified failed job = %#v", stored)
	}
	assertSlotFree(first)

	// A request-scoped rejection (bad input, moderated prompt) fails the job
	// without failing over and without growing the account's failure count.
	third := createAccount("third", 50)
	if err := modelRepo.ReplaceAccountCapabilities(ctx, third.ID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	selector.MarkQuotaStateChanged(account.ProviderWeb) // drop the cached candidate snapshot
	before, err := accountRepo.Get(ctx, third.ID)
	if err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	adapter.failures = map[uint64]int{third.ID: 1}
	adapter.status, adapter.scoped, adapter.attempts = http.StatusBadRequest, true, nil
	adapter.mu.Unlock()
	scopedJob := unknownJob
	scopedJob.ID, scopedJob.RequestID = "video_request_scoped", "request-video-scoped"
	scopedJob.AccountID, scopedJob.AccountName = third.ID, third.Name
	scopedJob.CreatedAt = time.Now().UTC()
	scopedJob.UpdatedAt = scopedJob.CreatedAt
	if err := mediaRepo.CreateMediaJob(ctx, scopedJob); err != nil {
		t.Fatal(err)
	}
	service.runVideoJob(ctx, scopedJob, route)
	if attempts := adapter.Attempts(); len(attempts) != 1 || attempts[0] != third.ID {
		t.Fatalf("request-scoped attempts = %#v, want pinned account once", attempts)
	}
	after, err := accountRepo.Get(ctx, third.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.FailureCount != before.FailureCount {
		t.Fatalf("request-scoped failure changed account failure count %d -> %d", before.FailureCount, after.FailureCount)
	}
	assertSlotFree(third)
}

// The initial creation path (CreateVideoJob) always scopes account selection
// to the requesting client key's AccountScope. This confirms the retry path
// inside runVideoJob does not silently widen that scope when the originally
// pinned account fails: a key restricted to Super-tier accounts must never
// fail over to a Free-tier account, even though one is available in the pool.
func TestVideoRetryDoesNotEscapeClientKeyAccountScope(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-retry-scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	mediaRepo := relational.NewMediaJobRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "video-scope-test", Prefix: "video-scope-test", SecretHash: strings.Repeat("b", 64),
		EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
		// Restricted to Super-tier Web accounts only.
		ProviderScope: clientkey.ProviderScopeOf(account.ProviderWeb), TierScope: clientkey.TierScopeSuper,
	})
	if err != nil {
		t.Fatal(err)
	}
	createAccount := func(name string, tier account.WebTier, priority int) account.Credential {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: tier,
			Name: name, SourceKey: name, EncryptedAccessToken: name + "-token", ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: priority, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return credential
	}
	// pinned is the Super-tier account the job was originally created against
	// (in scope); outOfScope is a Free-tier account that must never be used
	// for this key, even as a failover target.
	pinned := createAccount("pinned-super", account.WebTierSuper, 200)
	outOfScope := createAccount("free-out-of-scope", account.WebTierBasic, 100)
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []uint64{pinned.ID, outOfScope.ID} {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, accountID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
	if err != nil {
		t.Fatal(err)
	}

	// The pinned account always fails (forbidden), forcing the retry path to
	// look for a replacement. Without the fix, that replacement search ignores
	// the client key's scope entirely and would pick the Free-tier account.
	adapter := &videoCreateFailoverAdapter{
		failures: map[uint64]int{pinned.ID: 999},
		status:   http.StatusForbidden,
	}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 3)
	service.ConfigureMedia(mediaRepo, 1)
	service.UpdateVideoMaxAttempts(3)

	now := time.Now().UTC()
	job := media.Job{
		ID: "video_scope_retry", RequestID: "request-video-scope-retry", ClientKeyID: key.ID, ClientKeyName: key.Name,
		AccountID: pinned.ID, AccountName: pinned.Name, Provider: string(account.ProviderWeb),
		Model: route.PublicID, ModelRouteID: route.ID, UpstreamModel: route.UpstreamModel,
		Operation: provider.VideoOperationGenerate, Prompt: "test", Seconds: 5, Quality: "720p",
		Status: media.StatusInProgress, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}
	t.Run("worker shutdown during key lookup preserves recovery", func(t *testing.T) {
		shutdownJob := job
		shutdownJob.ID = "video_scope_worker_shutdown"
		shutdownJob.RequestID = "request-video-scope-worker-shutdown"
		if err := mediaRepo.CreateMediaJob(ctx, shutdownJob); err != nil {
			t.Fatal(err)
		}
		workerCtx, stopWorker := context.WithCancel(ctx)
		stopWorker()
		service.runVideoJob(workerCtx, shutdownJob, route)

		stored, err := mediaRepo.GetMediaJob(ctx, shutdownJob.ID, key.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != media.StatusInProgress || stored.LeaseUntil == nil || !stored.LeaseUntil.After(time.Now().UTC()) {
			t.Fatalf("job must remain eligible for recovery after worker shutdown, got %#v", stored)
		}
		if stored.ErrorCode != "" || stored.ErrorMessage != "" || stored.CompletedAt != nil || stored.UsageRecordedAt != nil {
			t.Fatalf("worker shutdown must not finalize the job, got %#v", stored)
		}
		if attempts := adapter.Attempts(); len(attempts) != 0 {
			t.Fatalf("worker shutdown must not invoke the upstream, got attempts %#v", attempts)
		}
	})
	t.Run("a job pinned outside the scope does not run there", func(t *testing.T) {
		outsideJob := job
		outsideJob.ID, outsideJob.RequestID = "video_scope_pinned_outside", "request-video-scope-pinned-outside"
		outsideJob.AccountID, outsideJob.AccountName = outOfScope.ID, outOfScope.Name
		if err := mediaRepo.CreateMediaJob(ctx, outsideJob); err != nil {
			t.Fatal(err)
		}
		service.runVideoJob(ctx, outsideJob, route)
		for _, attempted := range adapter.Attempts() {
			if attempted == outOfScope.ID {
				t.Fatalf("job pinned to out-of-scope account %d ran there", outOfScope.ID)
			}
		}
	})

	if err := mediaRepo.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	service.runVideoJob(ctx, job, route)

	for _, attemptedAccountID := range adapter.Attempts() {
		if attemptedAccountID == outOfScope.ID {
			t.Fatalf("out-of-scope Free-tier account %d was used for a key scoped to Super tier only", outOfScope.ID)
		}
	}
	stored, err := mediaRepo.GetMediaJob(ctx, job.ID, job.ClientKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != media.StatusFailed {
		t.Fatalf("expected the job to fail rather than complete via an out-of-scope account, got %#v", stored)
	}
}

// A resumed video is no quota probe: its success must not clear the
// account's quota recovery and return the account to scheduling.
func TestResumedVideoKeepsTheAccountQuotaRecovery(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-resume-quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	mediaRepo := relational.NewMediaJobRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	key, err := keyRepo.Create(ctx, clientkey.Key{Name: "video-resume", Prefix: "video-resume", SecretHash: strings.Repeat("c", 64), EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 60, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "resumed", SourceKey: "resumed", EncryptedAccessToken: "resumed-token", ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
	if err != nil {
		t.Fatal(err)
	}
	nextProbe := time.Now().UTC().Add(time.Hour)
	if err := accountRepo.SaveQuotaRecovery(ctx, account.QuotaRecovery{AccountID: credential.ID, Kind: account.QuotaRecoveryKindPaid, Status: account.QuotaRecoveryStatusExhausted, NextProbeAt: &nextProbe, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(&videoCreateFailoverAdapter{})
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 3)
	service.ConfigureMedia(mediaRepo, 1)
	now := time.Now().UTC()
	job := media.Job{
		ID: "video_resumed_quota", RequestID: "request-video-resumed-quota", ClientKeyID: key.ID, ClientKeyName: key.Name, AccountID: credential.ID, AccountName: credential.Name,
		Provider: string(account.ProviderWeb), Model: route.PublicID, ModelRouteID: route.ID, UpstreamModel: route.UpstreamModel,
		Operation: provider.VideoOperationGenerate, Prompt: "test", Seconds: 5, Quality: "720p", Checkpoint: `{"id":"upstream"}`, Submitted: true,
		Status: media.StatusInProgress, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := mediaRepo.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	service.runVideoJob(ctx, job, route)
	if stored, err := mediaRepo.GetMediaJob(ctx, job.ID, key.ID); err != nil || stored.Status != media.StatusCompleted {
		t.Fatalf("resumed job = %#v, err = %v", stored, err)
	}
	if recovery, err := accountRepo.GetQuotaRecovery(ctx, credential.ID); err != nil || recovery.Status != account.QuotaRecoveryStatusExhausted {
		t.Fatalf("quota recovery after a resumed video = %#v, err = %v", recovery, err)
	}
}
