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

// ValidateVideoRequest 校验 Adobe 视频请求的模型与参数。时长和比例不在这里拒绝：
// 网关对未填的 duration 默认填 8 秒，GenerateVideo 会就近对齐到模型支持的值。
func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	if options.Operation != provider.VideoOperationGenerate {
		return provider.ErrVideoOperationUnsupported
	}
	m, ok := videoModelByID(strings.TrimSpace(options.UpstreamModel))
	if !ok {
		return fmt.Errorf("%w: 未知的 Adobe 视频模型 %s", provider.ErrVideoParameterInvalid, options.UpstreamModel)
	}
	if r := strings.ToLower(strings.TrimSpace(options.Resolution)); r != "" && !slices.Contains(m.resolutions, r) {
		return fmt.Errorf("%w: %s 不支持分辨率 %s", provider.ErrVideoParameterInvalid, m.id, options.Resolution)
	}
	return nil
}

// GenerateVideo 创建并等待一个 Adobe 视频任务，返回结果的预签名 URL。
func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	m, ok := videoModelByID(strings.TrimSpace(request.Model))
	if !ok {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, &requestError{msg: "未知的 Adobe 视频模型：" + request.Model})
	}
	if len(request.ReferenceAudios) > 0 {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, &requestError{msg: "Adobe 视频不支持 reference_audios"})
	}
	var cp videoCheckpoint
	_ = json.Unmarshal([]byte(request.Checkpoint), &cp)
	if cp.PollURL == "" && cp.Submitting {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageSubmitted, 0, fmt.Errorf("Adobe 视频已提交但未取得任务 ID，无法安全重建"))
	}
	var result provider.VideoResult
	err := a.withClient(ctx, request.Credential, func(ctx context.Context, c *client) error {
		// 已有轮询 URL：上次只丢了结果，继续等待即可，不重复创建。
		if cp.PollURL == "" {
			pollURL, err := a.createVideo(ctx, c, m, request)
			if err != nil {
				return err
			}
			cp.PollURL = pollURL
		}
		urls, err := pollUntilDone(ctx, c, cp.PollURL, "video", request.Progress)
		if err != nil {
			return provider.WrapVideoStage(provider.VideoStagePoll, 0, err)
		}
		result = provider.VideoResult{URL: urls[0], ContentType: "video/mp4"}
		return nil
	})
	if err != nil {
		// 已带阶段的错误原样返回；出口租约或解密失败发生在提交之前。
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	return result, nil
}

// createVideo 准备输入并提交任务，返回轮询 URL。提交前先取 x-arp-session-id，
// 再落盘 Submitting 标记，保证标记只出现在真的发出了请求之后。
func (a *Adapter) createVideo(ctx context.Context, c *client, m videoModel, request provider.VideoRequest) (string, error) {
	if c.rc.token == "" {
		return "", preSubmitFailure(errNoToken)
	}
	in := videoPayloadInput{
		model: m, prompt: request.Prompt,
		aspectRatio:   nearestRatio(request.AspectRatio, m.ratios, m.defaultRatio),
		duration:      nearestDuration(request.Duration, m.durations, m.defaultDur),
		resolution:    strings.ToLower(strings.TrimSpace(request.Resolution)),
		generateAudio: m.genAudio,
	}
	if in.resolution == "" {
		in.resolution = m.defaultRes
	}
	// 首帧图与参考图都上传成 Adobe 存储里的图像。
	for _, ref := range append([]string{request.ImageURL}, request.ReferenceURLs...) {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		id, err := a.uploadImage(ctx, c, ref)
		if err != nil {
			return "", preSubmitFailure(err)
		}
		in.sourceImageID = append(in.sourceImageID, id)
	}
	if err := a.useSherlock(ctx, c); err != nil {
		return "", preSubmitFailure(err)
	}
	if err := provider.SaveVideoCheckpoint(request, videoCheckpoint{Submitting: true}); err != nil {
		return "", provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	sub, err := a.submit(ctx, c, func() (submission, error) {
		return c.submitVideo(ctx, endpoints.videoSubmit, buildVideoPayload(in))
	})
	if err != nil {
		return "", provider.VideoCreateFailed(request, err)
	}
	if err := provider.SaveVideoCheckpoint(request, videoCheckpoint{PollURL: sub.pollURL}); err != nil {
		return "", provider.WrapVideoStage(provider.VideoStagePoll, 0, err)
	}
	return sub.pollURL, nil
}

// preSubmitFailure 给提交之前的失败标阶段：账号问题（401 凭据、429 积分）可换账号
// 重试，其余（输入无效、mint 不可用、网络）结束任务。
func preSubmitFailure(err error) error {
	stage := provider.VideoStagePrepare
	if status := provider.ErrorHTTPStatusOrZero(err); status == http.StatusUnauthorized || status == http.StatusTooManyRequests {
		stage = provider.VideoStageCreate
	}
	return provider.WrapVideoStage(stage, 0, err)
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
		err = &transportError{err: err}
		a.report(ctx, lease, err)
		lease.Release()
		return nil, "", 0, err
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
	// 读完或关闭时反馈出口健康度并释放租约。
	body := provider.NewCompletionReadCloser(resp.Body, func(readErr error, _ bool) {
		if readErr != nil {
			readErr = &transportError{err: readErr}
		}
		a.report(ctx, lease, readErr)
		lease.Release()
	})
	return body, contentType, resp.ContentLength, nil
}
