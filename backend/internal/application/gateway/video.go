package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	videoJobTimeout          = 2 * time.Hour
	videoJobLease            = videoJobTimeout + 5*time.Minute
	videoJobRecoveryInterval = 30 * time.Second
	videoOutputAttempts      = 3
	// Base64 物化会同时持有原图和编码后字符串，单独限流避免高 mediaConcurrency 放大内存峰值。
	videoInputMaterializeConcurrency = 4
	videoInputJSONBaseBytes          = int64(len(`{}`))
)

// VideoInputFileReference 将本地临时 file_id 编码为只在 Gateway 内部解释的引用。
func VideoInputFileReference(fileID string) string {
	return media.InputReference(fileID)
}

type VideoInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	// Operation defaults to generate when empty.
	Operation provider.VideoOperation
	Prompt    string
	// Duration, AspectRatio and Resolution are left zero when the client omits
	// them; the selected route's provider fills its model's defaults.
	Duration    int
	AspectRatio string
	Resolution  string
	// ImageURL is the optional first-frame image (official "image").
	ImageURL string
	// LastFrameURL is the optional last-frame image; it requires ImageURL.
	LastFrameURL string
	// ReferenceVideoURLs are optional reference videos for generation.
	ReferenceVideoURLs []string
	// ReferenceURLs are style/content references (official "reference_images").
	ReferenceURLs []string
	// ReferenceAudios are preset voice_ids for reference-to-video.
	ReferenceAudios []string
	// VideoURL is required for edit/extend (official "video").
	VideoURL string
	// IdempotencyKey deduplicates repeated creation of the same job per client key.
	IdempotencyKey string
}

func (s *Service) CreateVideo(ctx context.Context, input VideoInput) (media.Job, error) {
	if s.mediaJobs == nil || s.mediaQueue == nil {
		return media.Job{}, fmt.Errorf("视频任务服务未配置")
	}
	operation := input.Operation
	if operation == "" {
		operation = provider.VideoOperationGenerate
	}
	switch operation {
	case provider.VideoOperationGenerate, provider.VideoOperationEdit, provider.VideoOperationExtend:
	default:
		return media.Job{}, fmt.Errorf("不支持的视频操作")
	}
	if len(input.Prompt) > 100000 {
		return media.Job{}, fmt.Errorf("prompt 过长")
	}
	hasLastFrame := strings.TrimSpace(input.LastFrameURL) != ""
	if hasLastFrame && (operation != provider.VideoOperationGenerate || strings.TrimSpace(input.ImageURL) == "") {
		return media.Job{}, fmt.Errorf("last_frame 只能用于视频生成，且必须同时提供 image（首帧）")
	}
	if len(input.ReferenceVideoURLs) > 0 {
		switch {
		case operation != provider.VideoOperationGenerate:
			return media.Job{}, fmt.Errorf("reference_videos 只能用于视频生成")
		case len(input.ReferenceVideoURLs) > media.MaxReferenceVideos:
			return media.Job{}, fmt.Errorf("reference_videos 最多 %d 个", media.MaxReferenceVideos)
		case strings.TrimSpace(input.Prompt) == "":
			return media.Job{}, fmt.Errorf("参考视频生成必须提供 prompt")
		}
	}
	if operation == provider.VideoOperationGenerate {
		hasImage := strings.TrimSpace(input.ImageURL) != ""
		hasRefs := len(input.ReferenceURLs) > 0
		hasRefAudio := len(input.ReferenceAudios) > 0
		if hasImage && (hasRefs || hasRefAudio) {
			return media.Job{}, fmt.Errorf("image 不能与 reference_images/reference_audios 同时使用")
		}
		if (hasRefs || hasRefAudio) && len(input.Prompt) == 0 {
			return media.Job{}, fmt.Errorf("参考图/参考音频视频必须提供 prompt")
		}
		if len(input.Prompt) == 0 && !hasImage && !hasRefs && !hasRefAudio {
			return media.Job{}, fmt.Errorf("文本生视频必须提供 prompt；图片生视频可以省略 prompt")
		}
		if strings.TrimSpace(input.VideoURL) != "" {
			return media.Job{}, fmt.Errorf("视频生成不支持 video 输入")
		}
		if err := validateVideoReferenceAudios(input.ReferenceAudios); err != nil {
			return media.Job{}, err
		}
	} else {
		if strings.TrimSpace(input.Prompt) == "" {
			return media.Job{}, fmt.Errorf("视频编辑/延长必须提供 prompt")
		}
		if strings.TrimSpace(input.VideoURL) == "" {
			return media.Job{}, fmt.Errorf("视频编辑/延长必须提供 video")
		}
		if strings.TrimSpace(input.ImageURL) != "" || len(input.ReferenceURLs) > 0 || len(input.ReferenceAudios) > 0 {
			return media.Job{}, fmt.Errorf("视频编辑/延长不支持 image、reference_images 或 reference_audios")
		}
		if operation == provider.VideoOperationEdit && input.Duration != 0 {
			return media.Job{}, fmt.Errorf("视频编辑不支持 duration")
		}
		if operation == provider.VideoOperationExtend {
			if input.Duration == 0 {
				input.Duration = 6
			}
			if input.Duration < 2 || input.Duration > 10 {
				return media.Job{}, fmt.Errorf("视频延长 duration 必须在 2 到 10 秒之间")
			}
		}
	}
	routes, err := s.models.GetByPublicIDCandidates(ctx, input.PublicModel)
	if err != nil {
		return media.Job{}, ErrModelNotFound
	}
	routes, err = s.routesForVideoOperation(routes, operation)
	if err != nil {
		return media.Job{}, err
	}
	providerSupported := func(providerValue account.Provider) bool {
		_, ok := s.providers.Videos(providerValue)
		return ok
	}
	routes, _, err = s.eligibleMediaRoutes(routes, input.ClientKey, model.CapabilityVideo, providerSupported)
	if err != nil {
		return media.Job{}, err
	}
	options := provider.VideoRequestOptions{
		Operation: operation, AspectRatio: input.AspectRatio, Resolution: input.Resolution, HasImage: strings.TrimSpace(input.ImageURL) != "", HasLastFrame: hasLastFrame,
		ReferenceImages: len(input.ReferenceURLs), ReferenceVideos: len(input.ReferenceVideoURLs), ReferenceAudios: len(input.ReferenceAudios),
		Duration: input.Duration,
	}
	routes, err = s.routesForVideoParameters(routes, options)
	if err != nil {
		return media.Job{}, err
	}
	inputs := videoInputs{
		imageURL: input.ImageURL, lastFrameURL: input.LastFrameURL, referenceURLs: input.ReferenceURLs,
		referenceVideoURLs: input.ReferenceVideoURLs, referenceAudios: input.ReferenceAudios, videoURL: input.VideoURL,
	}
	// 图像与视频输入共用一个物化预算。
	budget := videoInputJSONBaseBytes
	if err := s.validateVideoInputBudget(ctx, inputs.imageRefs(), "image", &budget); err != nil {
		return media.Job{}, err
	}
	videos := trimmedValues(append(slices.Clone(input.ReferenceVideoURLs), input.VideoURL))
	if err := s.validateVideoInputBudget(ctx, videos, "video", &budget); err != nil {
		return media.Job{}, err
	}
	inputJSON, err := encodeVideoInputFull(operation, inputs)
	if err != nil {
		return media.Job{}, err
	}
	hash := payloadHash(struct {
		Model, Prompt, AspectRatio, Resolution, Input string
		Operation                                     provider.VideoOperation
		Duration                                      int
	}{input.PublicModel, input.Prompt, input.AspectRatio, input.Resolution, inputJSON, operation, input.Duration})
	if existing, found, err := s.idempotentJob(ctx, input.ClientKey.ID, input.IdempotencyKey, hash); err != nil || found {
		return existing, err
	}
	route, selection, err := s.selectSchedulableEligibleMediaRouteWithQuotaMode(ctx, routes, input.ClientKey, true, func(route model.Route) string {
		return s.providers.VideoQuotaMode(route.Provider, s.providers.QuotaMode(route.Provider, route.UpstreamModel), input.Resolution)
	})
	if err != nil {
		return media.Job{}, err
	}
	// 选中路由的 Provider 决定实际生成的时长、比例与分辨率；计费和任务记录都用它。
	options.UpstreamModel = route.UpstreamModel
	effective, err := s.providers.NormalizeVideoRequest(route.Provider, options)
	if err != nil {
		return media.Job{}, err
	}
	input.Duration, input.AspectRatio, input.Resolution = effective.Duration, effective.AspectRatio, effective.Resolution
	if err := s.checkLedgerReady(); err != nil {
		return media.Job{}, err
	}
	pricing, priced := s.resolveVideoPricing(operation, route.Provider, route.UpstreamModel, input.Resolution, input.Duration, len(inputs.imageRefs()))
	externalModel := model.ExternalPublicID(route.Provider, route.PublicID)
	lease, err := selection.Acquire(ctx, nil, false)
	if err != nil {
		return media.Job{}, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
	}
	accountID := lease.Credential.ID
	lease.Release()
	token, err := security.NewOpaqueToken(18)
	if err != nil {
		return media.Job{}, err
	}
	now := time.Now().UTC()
	job := media.Job{
		ID: "video_" + token, Kind: media.KindVideo, ItemCount: 1, RequestID: input.RequestID,
		ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		ClientIP:  requestmeta.ClientIP(ctx),
		AccountID: accountID, AccountName: lease.Credential.Name,
		Provider: string(route.Provider), Model: externalModel, ModelRouteID: route.ID, UpstreamModel: model.DisplayUpstreamModel(route.Provider, route.UpstreamModel), Operation: operation, Prompt: input.Prompt,
		Seconds: input.Duration, Size: input.AspectRatio, Quality: input.Resolution,
		Status: media.StatusQueued, Progress: 0, InputJSON: inputJSON, InputImageCount: len(inputs.imageRefs()), CreatedAt: now, UpdatedAt: now,
		Price: &pricing, IdempotencyKey: input.IdempotencyKey, PayloadHash: hash,
	}
	reserved := false
	if priced {
		reserved, err = s.clientKeys.ReserveBilling(ctx, input.ClientKey, "video_usage_"+job.ID, pricing.CostInUSDTicks, mediaBillingReservationTTL)
		if err != nil {
			return media.Job{}, err
		}
	}
	if err := s.mediaJobs.CreateMediaJob(ctx, job); err != nil {
		if reserved {
			s.cancelBillingReservation("video_usage_" + job.ID)
		}
		if errors.Is(err, repository.ErrConflict) {
			if existing, found, findErr := s.idempotentJob(ctx, input.ClientKey.ID, input.IdempotencyKey, hash); findErr != nil || found {
				return existing, findErr
			}
		}
		return media.Job{}, err
	}
	if !s.enqueueMediaJob(job.ID) {
		s.logger.Warn("video_job_queue_full", "job_id", job.ID)
	}
	return job, nil
}

// routesForVideoParameters removes only routes that cannot accept this request.
// Callers must first apply capability, client-key, and Provider eligibility:
// the same public model may aggregate Console, Build, and Web routes with
// different input contracts. options.UpstreamModel is filled per route.
func (s *Service) routesForVideoParameters(routes []model.Route, options provider.VideoRequestOptions) ([]model.Route, error) {
	if len(routes) == 0 {
		return routes, nil
	}
	compatible := make([]model.Route, 0, len(routes))
	var firstErr error
	for _, candidate := range routes {
		if err := s.validateVideoRouteParameters(candidate.Provider, candidate.UpstreamModel, options); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		compatible = append(compatible, candidate)
	}
	if len(compatible) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return compatible, nil
}

// validateVideoRouteParameters asks the route's provider whether it can serve
// the request. /v1/videos/generations is asynchronous, so a request the
// upstream must fail is rejected here instead of as a failed job; the adapter
// keeps the same checks at its outbound boundary.
func (s *Service) validateVideoRouteParameters(providerValue account.Provider, upstreamModel string, options provider.VideoRequestOptions) error {
	options.UpstreamModel = upstreamModel
	_, err := s.providers.NormalizeVideoRequest(providerValue, options)
	return err
}

// routesForVideoOperation keeps the routes whose provider serves an edit or extension.
func (s *Service) routesForVideoOperation(routes []model.Route, operation provider.VideoOperation) ([]model.Route, error) {
	if operation == provider.VideoOperationGenerate {
		return routes, nil
	}
	compatible := make([]model.Route, 0, len(routes))
	for _, candidate := range routes {
		if s.providers.ValidateVideoRequest(candidate.Provider, provider.VideoRequestOptions{Operation: operation, UpstreamModel: candidate.UpstreamModel}) == nil {
			compatible = append(compatible, candidate)
		}
	}
	if len(compatible) == 0 {
		return nil, ErrVideoOperationUnsupported
	}
	return compatible, nil
}

func (s *Service) resolveVideoPricing(operation provider.VideoOperation, providerValue account.Provider, upstreamModel, resolution string, duration, inputImages int) (audit.PricingResult, bool) {
	if operation == provider.VideoOperationGenerate {
		return s.pricing.QuoteVideo(providerValue, upstreamModel, resolution, duration, inputImages)
	}
	return audit.PricingResult{}, false
}

func (s *Service) getVideoJob(ctx context.Context, id string, key clientkey.Key) (media.Job, error) {
	if s.mediaJobs == nil {
		return media.Job{}, ErrResponseNotFound
	}
	job, err := s.mediaJobs.GetMediaJob(ctx, id, key.ID)
	if err != nil {
		return media.Job{}, ErrResponseNotFound
	}
	return job, nil
}

// GetVideo returns the client-facing job state. A result asset is exposed only
// while the local object can still be opened; completed assets are eligible for
// capacity cleanup, and a stale asset ID would otherwise produce a dead public URL.
func (s *Service) GetVideo(ctx context.Context, id string, key clientkey.Key) (media.Job, error) {
	job, err := s.getVideoJob(ctx, id, key)
	if err != nil {
		return media.Job{}, err
	}
	if job.Status != media.StatusCompleted || strings.TrimSpace(job.ResultAssetID) == "" {
		return job, nil
	}
	if s.mediaAssets == nil {
		job.ResultAssetID = ""
		return job, nil
	}
	_, body, openErr := s.mediaAssets.OpenVideo(ctx, job.ResultAssetID)
	if openErr != nil || body == nil {
		job.ResultAssetID = ""
		return job, nil
	}
	_ = body.Close()
	return job, nil
}

func (s *Service) OpenVideoContent(ctx context.Context, id string, key clientkey.Key) (io.ReadCloser, string, int64, error) {
	job, err := s.getVideoJob(ctx, id, key)
	if err != nil {
		return nil, "", 0, err
	}
	if job.Status != media.StatusCompleted {
		return nil, "", 0, fmt.Errorf("视频内容尚未可用")
	}
	// 本地资产优先：XAI ZDR 上传完成后不经公网回环下载。
	if job.ResultAssetID != "" && s.mediaAssets != nil {
		asset, body, openErr := s.mediaAssets.OpenVideo(ctx, job.ResultAssetID)
		if openErr == nil {
			return body, asset.MIMEType, asset.SizeBytes, nil
		}
	}
	if job.UpstreamURL == "" {
		return nil, "", 0, fmt.Errorf("视频内容尚未可用")
	}
	adapter, ok := s.providers.Videos(account.Provider(job.Provider))
	if !ok {
		return nil, "", 0, ErrResponseAccountUnavailable
	}
	downloader, ok := adapter.(provider.VideoContentDownloader)
	if !ok {
		return nil, "", 0, ErrResponseAccountUnavailable
	}
	credential, err := s.jobCredential(ctx, job)
	if err != nil {
		return nil, "", 0, err
	}
	return downloader.DownloadVideo(ctx, credential, job.UpstreamURL)
}

// jobCredential loads the account that created a job. Reading or resuming
// its upstream work needs that exact credential, not a scheduling slot.
func (s *Service) jobCredential(ctx context.Context, job media.Job) (account.Credential, error) {
	if job.AccountID == 0 || s.selector == nil || s.selector.accounts == nil || s.accounts == nil {
		return account.Credential{}, ErrResponseAccountUnavailable
	}
	credential, err := s.selector.accounts.Get(ctx, job.AccountID)
	if err != nil {
		return account.Credential{}, ErrResponseAccountUnavailable
	}
	credential, err = s.accounts.EnsureCredential(ctx, credential, false)
	if err != nil {
		return account.Credential{}, ErrResponseAccountUnavailable
	}
	return credential, nil
}
