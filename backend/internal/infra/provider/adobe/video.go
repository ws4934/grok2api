package adobe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// videoCheckpoint 记录已提交视频任务的轮询 URL，用于重启后续跑而不重复创建。
type videoCheckpoint struct {
	PollURL    string `json:"poll_url,omitempty"`
	Submitting bool   `json:"submitting,omitempty"`
}

// ValidateVideoRequest 校验 Adobe 视频请求的模型与参数。
func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	if options.Operation != provider.VideoOperationGenerate {
		return provider.ErrVideoOperationUnsupported
	}
	m, ok := videoModelByID(a.resolveVideoModelID(options.UpstreamModel))
	if !ok {
		return fmt.Errorf("%w: 未知的 Adobe 视频模型 %s", provider.ErrVideoParameterInvalid, options.UpstreamModel)
	}
	if options.Duration > 0 && len(m.durations) > 0 && !slices.Contains(m.durations, options.Duration) {
		return fmt.Errorf("%w: %s 不支持 %d 秒", provider.ErrVideoParameterInvalid, m.id, options.Duration)
	}
	if r := strings.TrimSpace(options.Resolution); r != "" && len(m.resolutions) > 0 && !slices.Contains(m.resolutions, strings.ToLower(r)) {
		return fmt.Errorf("%w: %s 不支持分辨率 %s", provider.ErrVideoParameterInvalid, m.id, r)
	}
	return nil
}

func (a *Adapter) resolveVideoModelID(model string) string {
	return strings.TrimSpace(model)
}

// GenerateVideo 创建并等待一个 Adobe 视频任务，返回结果的预签名 URL。
func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	m, ok := videoModelByID(a.resolveVideoModelID(request.Model))
	if !ok {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, fmt.Errorf("未知的 Adobe 视频模型：%s", request.Model))
	}

	var cp videoCheckpoint
	_ = json.Unmarshal([]byte(request.Checkpoint), &cp)

	lease, err := a.egress.AcquireCredential(ctx, scope, request.Credential)
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	defer lease.Release()
	rc, err := a.requestContextFor(ctx, request.Credential)
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	if rc.token == "" {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, errNoToken)
	}
	c := &client{do: lease, rc: rc}

	// 已有轮询 URL：仅结果下载丢失，继续等待即可。
	if cp.PollURL != "" {
		outputURL, pollErr := pollUntilDone(ctx, c, cp.PollURL, "video", request.Progress)
		if pollErr != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePoll, 0, pollErr)
		}
		return provider.VideoResult{URL: outputURL, ContentType: "video/mp4"}, nil
	}
	if cp.Submitting {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageSubmitted, 0, fmt.Errorf("Adobe 视频已提交但未取得任务 ID，无法安全重建"))
	}

	in := videoPayloadInput{
		model: m, prompt: request.Prompt, aspectRatio: normalizeRatio(request.AspectRatio, m.defaultRatio, false),
		duration: request.Duration, resolution: strings.TrimSpace(request.Resolution), generateAudio: m.genAudio,
		sourceAudioID: request.ReferenceAudios,
	}
	if in.duration == 0 {
		in.duration = m.defaultDur
	}
	if in.resolution == "" {
		in.resolution = m.defaultRes
	}
	// 上传首帧图、参考图与参考视频。
	if err := a.uploadVideoRefs(ctx, c, &in, request); err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}

	if err := provider.SaveVideoCheckpoint(request, videoCheckpoint{Submitting: true}); err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	sub, err := a.submitVideoRemint(ctx, c, endpoints.videoSubmit, buildVideoPayload(in))
	a.report(ctx, lease, err)
	if err != nil {
		return provider.VideoResult{}, provider.VideoCreateFailed(request, err)
	}
	if err := provider.SaveVideoCheckpoint(request, videoCheckpoint{PollURL: sub.pollURL}); err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePoll, 0, err)
	}
	outputURL, err := pollUntilDone(ctx, c, sub.pollURL, "video", request.Progress)
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePoll, 0, err)
	}
	return provider.VideoResult{URL: outputURL, ContentType: "video/mp4"}, nil
}

func (a *Adapter) uploadVideoRefs(ctx context.Context, c *client, in *videoPayloadInput, request provider.VideoRequest) error {
	if ref := strings.TrimSpace(request.ImageURL); ref != "" {
		id, err := a.uploadRef(ctx, c, ref, endpoints.imageUpload, "image")
		if err != nil {
			return err
		}
		in.sourceImageID = append(in.sourceImageID, id)
	}
	for _, ref := range request.ReferenceURLs {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		id, err := a.uploadRef(ctx, c, ref, endpoints.imageUpload, "image")
		if err != nil {
			return err
		}
		in.sourceImageID = append(in.sourceImageID, id)
	}
	if ref := strings.TrimSpace(request.VideoURL); ref != "" {
		id, err := a.uploadRef(ctx, c, ref, endpoints.videoUpload, "video")
		if err != nil {
			return err
		}
		in.sourceVideoID = append(in.sourceVideoID, id)
	}
	return nil
}

// DownloadVideo 用创建任务的凭据读取完成的视频内容。调用方必须先校验任务归属。
func (a *Adapter) DownloadVideo(ctx context.Context, credential account.Credential, rawURL string) (io.ReadCloser, string, int64, error) {
	lease, err := a.egress.AcquireCredential(ctx, scope, credential)
	if err != nil {
		return nil, "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		lease.Release()
		return nil, "", 0, err
	}
	req.Header.Set("Accept", "*/*")
	resp, err := lease.Do(req)
	if err != nil {
		a.report(ctx, lease, &transportError{err: err})
		lease.Release()
		return nil, "", 0, &transportError{err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		lease.Release()
		return nil, "", 0, classifyUpstream("download", resp.StatusCode, "")
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "video/mp4"
	}
	// 包装 Body：关闭时释放出口租约。
	return &leasedBody{ReadCloser: resp.Body, lease: lease}, contentType, resp.ContentLength, nil
}

// leasedBody 在响应体关闭时释放出口租约。
type leasedBody struct {
	io.ReadCloser
	lease interface{ Release() }
	once  bool
}

func (b *leasedBody) Close() error {
	err := b.ReadCloser.Close()
	if !b.once {
		b.once = true
		b.lease.Release()
	}
	return err
}
