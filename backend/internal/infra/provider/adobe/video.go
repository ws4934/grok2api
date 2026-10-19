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

// NormalizeVideoRequest 选定实际生成（也用于计费和任务记录）的比例、分辨率与时长：
// 没传的用模型默认值，传了但模型不支持的就近对齐；参考图模式下 Veo 固定 16:9、8 秒。
// 未知模型原样返回，由随后的 ValidateVideoRequest 拒绝。
func (a *Adapter) NormalizeVideoRequest(options provider.VideoRequestOptions) (provider.VideoRequestOptions, error) {
	v, ok := lookup(options.UpstreamModel)
	if !ok || !v.video || options.Operation != provider.VideoOperationGenerate {
		return options, nil
	}
	in := v.videoPlan("", options.AspectRatio, options.Resolution, options.Duration, options.HasImage, options.ReferenceImages > 0)
	options.AspectRatio, options.Resolution = in.aspect, in.resolution
	options.Duration, _ = v.chooseDuration(in.duration)
	return options, nil
}

// ValidateVideoRequest 按模型表校验视频请求：首帧图、参考图的支持与数量。
// 时长、比例、分辨率由 NormalizeVideoRequest 对齐，不在这里拒绝。
func (a *Adapter) ValidateVideoRequest(options provider.VideoRequestOptions) error {
	if options.Operation != provider.VideoOperationGenerate {
		return provider.ErrVideoOperationUnsupported
	}
	v, ok := lookup(options.UpstreamModel)
	if !ok || !v.video {
		return fmt.Errorf("%w: 未知的 Adobe 视频模型 %s", provider.ErrVideoParameterInvalid, options.UpstreamModel)
	}
	if options.ReferenceAudios > 0 {
		return fmt.Errorf("%w: Adobe 视频不支持 reference_audios", provider.ErrVideoParameterInvalid)
	}
	if err := v.checkVideoInputs(options.HasImage, options.HasLastFrame, options.ReferenceImages, options.ReferenceVideos); err != nil {
		return fmt.Errorf("%w: %s", provider.ErrVideoParameterInvalid, err.Error())
	}
	return nil
}

// checkVideoInputs 按表里的上限检查首帧、尾帧、参考图与参考视频。首尾帧共用 frame
// 用途，上限 2 的模型才收尾帧；所有素材另受总数上限约束。
func (v *modelVersion) checkVideoInputs(hasFrame, hasLastFrame bool, references, videos int) error {
	frame, frameOK := v.refs["image:frame"]
	frameLimit := frame.max
	if limit := v.maxBlobs; limit > 0 && (frameLimit == 0 || limit < frameLimit) {
		frameLimit = limit // 首尾帧也计入素材总数
	}
	switch {
	case hasFrame && !frameOK:
		return &requestError{msg: v.slug + " 不支持首帧图 image"}
	case !hasFrame && (frame.min > 0 || v.requireFrame):
		return &requestError{msg: v.slug + " 需要首帧图 image"}
	case hasLastFrame && (!frameOK || (frameLimit > 0 && frameLimit < 2)):
		return &requestError{msg: v.slug + " 不支持尾帧 last_frame"}
	case v.framesExclusive && (hasFrame || hasLastFrame) && references+videos > 0:
		return &requestError{msg: v.slug + " 的首尾帧不能与参考图、参考视频同时使用"}
	}
	if references > 0 {
		usage, limit := v.referenceUsage()
		if usage == "" || v.noReferences {
			return &requestError{msg: v.slug + " 不支持 reference_images"}
		}
		if limit > 0 && references > limit {
			return &requestError{msg: fmt.Sprintf("%s 最多支持 %d 张参考图", v.slug, limit)}
		}
	}
	if videos > 0 {
		limit, ok := v.refs["video:source"]
		if !ok || !v.referenceVideos {
			return &requestError{msg: v.slug + " 不支持 reference_videos"}
		}
		if limit.max > 0 && videos > limit.max {
			return &requestError{msg: fmt.Sprintf("%s 最多支持 %d 个参考视频", v.slug, limit.max)}
		}
	}
	if limit := v.maxBlobs; limit > 0 {
		total := references + videos + boolCount(hasFrame) + boolCount(hasLastFrame)
		if total > limit {
			return &requestError{msg: fmt.Sprintf("%s 最多支持 %d 个参考素材（参考图、参考视频与首尾帧合计）", v.slug, limit)}
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
		found, ok := lookup(request.Model)
		if !ok || !found.video {
			return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, &requestError{msg: "未知的 Adobe 视频模型：" + request.Model})
		}
		hasFrame := strings.TrimSpace(request.ImageURL) != ""
		if err := found.checkVideoInputs(hasFrame, strings.TrimSpace(request.LastFrameURL) != "", len(request.ReferenceURLs), len(request.ReferenceVideoURLs)); err != nil {
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
	// 首帧 order 1、尾帧 order 2（官网 keyFrames 的顺序）。
	uploads := []struct {
		refs []string
		kind string
		ids  *[]string
	}{
		{[]string{request.ImageURL, request.LastFrameURL}, "image", &in.frames},
		{request.ReferenceURLs, "image", &in.references},
		{request.ReferenceVideoURLs, "video", &in.videos},
	}
	for _, group := range uploads {
		for _, ref := range group.refs {
			if strings.TrimSpace(ref) == "" {
				continue
			}
			id, err := a.uploadInput(ctx, c, v, ref, group.kind)
			if err != nil {
				return "", preSubmitFailure(err)
			}
			*group.ids = append(*group.ids, id)
		}
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
