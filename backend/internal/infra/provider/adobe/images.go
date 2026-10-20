package adobe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/mediafile"
)

// imageModel 查找图像模型并规划请求参数。
func (a *Adapter) imageModel(model string) (*modelVersion, error) {
	v, ok := lookup(model)
	if !ok || v.video {
		return nil, &requestError{msg: fmt.Sprintf("未知的 Adobe 图像模型：%s", model)}
	}
	return v, nil
}

// ValidateImageRequest 拒绝所有 Adobe 模型都不支持的参数；与模型相关的比例、档位、
// 张数在规划请求时按模型表校验。
func (a *Adapter) ValidateImageRequest(options provider.ImageRequestOptions) error {
	switch {
	case options.Streaming || options.PartialImages != 0:
		return &requestError{msg: "Adobe 不支持 stream 或 partial_images"}
	case options.ResponseFormat != "" && options.ResponseFormat != "url" && options.ResponseFormat != "b64_json":
		return &requestError{msg: "response_format 只支持 url 或 b64_json"}
	}
	return nil
}

// --- 同步图像（ImageGenerationAdapter / ImageEditAdapter）---

func (a *Adapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	v, err := a.imageModel(request.Model)
	if err != nil {
		return syncFailure(err, false)
	}
	in, err := v.imagePlan(request.Prompt, request.Size, request.AspectRatio, request.Resolution, request.Quality, request.Background, request.Count, 0, false)
	if err != nil {
		return syncFailure(err, false)
	}
	return a.runSyncImage(ctx, request.Credential, v, in, nil, "", request.ResponseFormat)
}

func (a *Adapter) EditImage(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	v, err := a.imageModel(request.Model)
	if err != nil {
		return syncFailure(err, false)
	}
	in, err := v.imagePlan(request.Prompt, request.Size, request.AspectRatio, request.Resolution, request.Quality, request.Background, request.Count, len(request.ImageURLs), strings.TrimSpace(request.MaskURL) != "")
	if err != nil {
		return syncFailure(err, false)
	}
	return a.runSyncImage(ctx, request.Credential, v, in, request.ImageURLs, request.MaskURL, request.ResponseFormat)
}

// runSyncImage 在一次请求内 submit→poll→download。Adobe 接受任务前的明确拒绝转成
// 响应交给网关处理；接受之后的失败保持为错误，网关不会重新提交。
func (a *Adapter) runSyncImage(ctx context.Context, credential account.Credential, v *modelVersion, in genInput, imageURLs []string, maskURL, responseFormat string) (*provider.Response, error) {
	var body map[string]any
	var delivered int
	accepted := false
	err := a.withClient(ctx, credential, func(ctx context.Context, c *client) error {
		if err := a.prepareImage(ctx, c, v, &in, imageURLs, maskURL); err != nil {
			return err
		}
		sub, err := a.submit(ctx, c, func() (submission, error) {
			return c.submitJob(ctx, endpoints.imageSubmit, v.buildBody(in), in.prompt)
		})
		if err != nil {
			return err
		}
		accepted = true
		urls, err := pollUntilDone(ctx, c, sub.pollURL, "image", nil)
		if err != nil {
			return err
		}
		// 图已生成：之后的下载/存储失败不换账号重画，也不扣账号健康度。
		images := make([][]byte, 0, len(urls))
		for _, url := range urls {
			data, err := c.download(ctx, url)
			if err != nil {
				return provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err)
			}
			images = append(images, data)
		}
		if body, err = a.imageResponseBody(ctx, in, images, responseFormat); err != nil {
			return provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
		}
		delivered = len(images)
		return nil
	})
	if err != nil {
		return syncFailure(err, accepted)
	}
	response := jsonResponse(http.StatusOK, body)
	// Adobe 可能少出几张：只按实际交付的张数计费。
	response.QuotaUnits = delivered
	return response, nil
}

// imageResponseBody 渲染 OpenAI Images 风格的响应体（可含多张）。
func (a *Adapter) imageResponseBody(ctx context.Context, in genInput, images [][]byte, responseFormat string) (map[string]any, error) {
	data := make([]any, 0, len(images))
	for _, raw := range images {
		item := map[string]any{}
		if responseFormat == "url" && a.assets != nil {
			asset, err := a.assets.SaveImage(ctx, raw)
			if err != nil {
				return nil, err
			}
			item["url"] = a.assets.PublicImageURL(asset.ID)
		} else {
			item["b64_json"] = base64.StdEncoding.EncodeToString(raw)
		}
		data = append(data, item)
	}
	return map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
		"usage":   a.ImageUsage(in.prompt, len(in.references), "", nil),
	}, nil
}

// --- 异步图像任务（AsyncImageAdapter）---

func (a *Adapter) SubmitImage(ctx context.Context, request provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
	var cp provider.ImageCheckpoint
	v, err := a.imageModel(request.Model)
	if err != nil {
		return cp, err
	}
	in, err := v.imagePlan(request.Prompt, request.Size, "", "", request.Quality, request.Background, 1, len(request.ImageURLs), strings.TrimSpace(request.MaskURL) != "")
	if err != nil {
		return cp, err
	}
	err = a.withClient(ctx, request.Credential, func(ctx context.Context, c *client) error {
		if err := a.prepareImage(ctx, c, v, &in, request.ImageURLs, request.MaskURL); err != nil {
			return err
		}
		// 不可逆的提交发生前先落盘 Submitted 标记。
		next := provider.ImageCheckpoint{Submitted: true}
		if err := save(next); err != nil {
			return err
		}
		cp = next
		sub, err := a.submit(ctx, c, func() (submission, error) {
			return c.submitJob(ctx, endpoints.imageSubmit, v.buildBody(in), in.prompt)
		})
		if err != nil {
			// 明确的 4xx 拒绝没有生成任务，网关可清掉标记换账号；其余结果未知。
			cp.Submitted = !rejected(err)
			return err
		}
		cp.UpstreamID = sub.pollURL
		return save(cp)
	})
	return cp, err
}

func (a *Adapter) PollImage(ctx context.Context, request provider.ImageJobRequest, cp provider.ImageCheckpoint) (provider.ImageCheckpoint, error) {
	if len(cp.Outputs) > 0 {
		return cp, nil
	}
	if cp.UpstreamID == "" {
		return cp, errors.New("image checkpoint has no polling URL")
	}
	err := a.withClient(ctx, request.Credential, func(ctx context.Context, c *client) error {
		urls, err := pollUntilDone(ctx, c, cp.UpstreamID, "image", nil)
		if err != nil {
			return err
		}
		cp.Outputs = urls
		return nil
	})
	return cp, err
}

func (a *Adapter) FetchImage(ctx context.Context, request provider.ImageJobRequest, cp provider.ImageCheckpoint) ([]byte, error) {
	if len(cp.Outputs) == 0 {
		return nil, errors.New("image checkpoint has no output")
	}
	var data []byte
	err := a.withClient(ctx, request.Credential, func(ctx context.Context, c *client) error {
		var err error
		data, err = c.download(ctx, cp.Outputs[0])
		return err
	})
	return data, err
}

// ImageUsage 估算交付图像的 usage 对象。Adobe 媒体按模型计价，这里只给出占位值。
func (a *Adapter) ImageUsage(prompt string, inputImages int, quality string, outputs []image.Point) map[string]any {
	return map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
}

// prepareImage 完成不可逆提交之前的全部步骤：检查 token、上传输入图与蒙版、
// 取全局 x-arp-session-id。这里的失败都不会在 Adobe 生成任何东西。
func (a *Adapter) prepareImage(ctx context.Context, c *client, v *modelVersion, in *genInput, imageURLs []string, maskURL string) error {
	if c.rc.token == "" {
		return errNoToken
	}
	for _, ref := range imageURLs {
		id, err := a.uploadInput(ctx, c, v, ref, "image")
		if err != nil {
			return err
		}
		in.references = append(in.references, id)
	}
	if strings.TrimSpace(maskURL) != "" {
		id, err := a.uploadInput(ctx, c, v, maskURL, "image")
		if err != nil {
			return err
		}
		in.mask = id
	}
	return a.useSherlock(ctx, c)
}

// uploadInput 读取客户端给的输入（kind 为 "image" 或 "video"），按模型表的单文件
// 上限检查后上传到 Adobe 存储，返回 asset id。
func (a *Adapter) uploadInput(ctx context.Context, c *client, v *modelVersion, ref, kind string) (string, error) {
	data, mime, err := a.readInput(ctx, ref, kind)
	if err != nil {
		return "", err
	}
	if limit := v.fileBytes[kind]; limit > 0 && int64(len(data)) > limit {
		return "", &requestError{msg: fmt.Sprintf("%s 的%s不能超过 %d MB", v.slug, inputLabel(kind), limit>>20)}
	}
	uploadURL := endpoints.imageUpload
	if kind == "video" {
		uploadURL = endpoints.videoUpload
	}
	return c.upload(ctx, uploadURL, data, mime, kind)
}

func inputLabel(kind string) string {
	if kind == "video" {
		return "视频"
	}
	return "输入图片"
}

// readInput 读取 data URI 或 https 输入。https 只经带 SSRF 防护的 fetchRemote 下载，
// 绝不通过账号出口去访问客户端指定的地址。输入有问题是请求本身的错。
func (a *Adapter) readInput(ctx context.Context, ref, kind string) ([]byte, string, error) {
	label := inputLabel(kind)
	ref = strings.TrimSpace(ref)
	var data []byte
	switch meta, payload, _ := strings.Cut(ref, ","); {
	case strings.HasPrefix(meta, "data:") && strings.HasSuffix(meta, ";base64"):
		raw, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, "", &requestError{msg: label + "的 data URI 不是有效的 Base64"}
		}
		data = raw
	case strings.HasPrefix(ref, "https://") && a.fetchRemote != nil:
		raw, err := a.fetchRemote(ctx, ref)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			return nil, "", &requestError{msg: "无法下载" + label + "（需可公网访问的 https 地址，且不超过 20 MiB）"}
		}
		data = raw
	default:
		return nil, "", &requestError{msg: label + "必须是 Base64 data URI 或 https URL"}
	}
	if kind == "video" {
		// 官网只收 MP4 与 QuickTime。
		switch mime := mediafile.VideoType(data); mime {
		case "video/mp4", "video/quicktime":
			return data, mime, nil
		}
		return nil, "", &requestError{msg: "视频只支持 MP4 或 QuickTime（MOV）"}
	}
	switch mime := http.DetectContentType(data); mime {
	case "image/png", "image/jpeg", "image/webp":
		return data, mime, nil
	}
	return nil, "", &requestError{msg: "输入图片只支持 PNG、JPEG 或 WebP"}
}

func jsonResponse(status int, body any) *provider.Response {
	encoded, _ := json.Marshal(body)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &provider.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: header, Body: io.NopCloser(bytes.NewReader(encoded))}
}
