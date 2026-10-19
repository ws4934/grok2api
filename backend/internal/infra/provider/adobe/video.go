package adobe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// videoCheckpoint 记录已提交视频任务的轮询 URL，用于重启后续跑而不重复创建。
type videoCheckpoint struct {
	PollURL    string `json:"poll_url,omitempty"`
	Submitting bool   `json:"submitting,omitempty"`
}

// ValidateVideoRequest 按模型 schema 校验视频请求：首帧图、参考图的支持与数量。
// 时长、比例、分辨率不在这里拒绝：网关会给未填的参数填默认值，GenerateVideo 会就近
// 对齐到模型支持的值。
func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	if options.Operation != provider.VideoOperationGenerate {
		return provider.ErrVideoOperationUnsupported
	}
	v, ok := a.catalog.Load().lookup(options.UpstreamModel)
	if !ok || !v.video {
		return fmt.Errorf("%w: 未知的 Adobe 视频模型 %s", provider.ErrVideoParameterInvalid, options.UpstreamModel)
	}
	if err := v.checkVideoInputs(options.HasImage, options.ReferenceImages); err != nil {
		return fmt.Errorf("%w: %s", provider.ErrVideoParameterInvalid, err.Error())
	}
	return nil
}

// checkVideoInputs 按 schema 的 x-capabilities 检查首帧图与参考图。
func (v *modelVersion) checkVideoInputs(hasFrame bool, references int) error {
	frame, frameOK := v.refs["image:frame"]
	switch {
	case hasFrame && !frameOK:
		return &requestError{msg: v.slug + " 不支持首帧图 image"}
	case !hasFrame && (frame.min > 0 || v.patch.requireFrame):
		return &requestError{msg: v.slug + " 需要首帧图 image"}
	}
	if references > 0 {
		usage, limit := v.referenceUsage()
		if usage == "" || v.patch.noReferences {
			return &requestError{msg: v.slug + " 不支持 reference_images"}
		}
		if limit > 0 && references > limit {
			return &requestError{msg: fmt.Sprintf("%s 最多支持 %d 张参考图", v.slug, limit)}
		}
	}
	return nil
}

// GenerateVideo 创建并等待一个 Adobe 视频任务，返回结果的预签名 URL。
func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	if len(request.ReferenceAudios) > 0 {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, &requestError{msg: "Adobe 视频不支持 reference_audios"})
	}
	var cp videoCheckpoint
	_ = json.Unmarshal([]byte(request.Checkpoint), &cp)
	if cp.PollURL == "" && cp.Submitting {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageSubmitted, 0, fmt.Errorf("Adobe 视频已提交但未取得任务 ID，无法安全重建"))
	}
	var v *modelVersion
	if cp.PollURL == "" {
		// 续跑已提交的任务不查目录：模型即使刚下架，已在跑的任务也要取回结果。
		found, ok := a.catalog.Load().lookup(request.Model)
		if !ok || !found.video {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, &requestError{msg: "未知的 Adobe 视频模型：" + request.Model})
		}
		hasFrame := strings.TrimSpace(request.ImageURL) != ""
		if err := found.checkVideoInputs(hasFrame, len(request.ReferenceURLs)); err != nil {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
		}
		v = found
	}
	var result provider.VideoResult
	err := a.withClient(ctx, request.Credential, func(ctx context.Context, c *client) error {
		if cp.PollURL == "" {
			pollURL, err := a.createVideo(ctx, c, v, request)
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
func (a *Adapter) createVideo(ctx context.Context, c *client, v *modelVersion, request provider.VideoRequest) (string, error) {
	if c.rc.token == "" {
		return "", preSubmitFailure(errNoToken)
	}
	in := v.videoPlan(request.Prompt, request.AspectRatio, request.Resolution, request.Duration, strings.TrimSpace(request.ImageURL) != "", len(request.ReferenceURLs) > 0)
	if ref := strings.TrimSpace(request.ImageURL); ref != "" {
		id, err := a.uploadImage(ctx, c, ref)
		if err != nil {
			return "", preSubmitFailure(err)
		}
		in.frames = append(in.frames, id)
	}
	for _, ref := range request.ReferenceURLs {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		id, err := a.uploadImage(ctx, c, ref)
		if err != nil {
			return "", preSubmitFailure(err)
		}
		in.references = append(in.references, id)
	}
	if err := a.useSherlock(ctx, c); err != nil {
		return "", preSubmitFailure(err)
	}
	if err := provider.SaveVideoCheckpoint(request, videoCheckpoint{Submitting: true}); err != nil {
		return "", provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	sub, err := a.submit(ctx, c, func() (submission, error) {
		return c.submitJob(ctx, endpoints.videoSubmit, v.buildBody(in), "")
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
