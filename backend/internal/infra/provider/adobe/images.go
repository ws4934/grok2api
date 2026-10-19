package adobe

import (
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
)

var errNoToken = errors.New("adobe account has no usable access token")

// pollInterval 是阻塞轮询两次 GET 之间的间隔。
const pollInterval = 3 * time.Second

// buildImageInput 把网关的图像请求参数解析成上游 payload 的基础输入。
func (a *Adapter) buildImageInput(model, prompt, size, aspectRatio, resolution, outputFormat, background string) (imagePayloadInput, error) {
	id := strings.TrimSpace(model)
	m, ok := imageModelByID(id)
	if !ok {
		return imagePayloadInput{}, &requestError{msg: fmt.Sprintf("未知的 Adobe 图像模型：%s", id)}
	}
	ratioCandidate := strings.TrimSpace(aspectRatio)
	if ratioCandidate == "" {
		ratioCandidate = ratioFromSize(size)
	}
	ratio := normalizeRatio(ratioCandidate, "16:9", m.nanoRatios)
	res := strings.TrimSpace(resolution)
	if res == "" {
		res = tierFromSize(size)
	}
	if res == "" {
		res = "2K"
	}
	return imagePayloadInput{
		model: m, prompt: prompt, aspectRatio: ratio, resolution: normalizeResolution(res, "2K"),
		outputFormat: outputFormat, background: background,
	}, nil
}

// ValidateImageRequest 拒绝 Adobe 不支持的请求参数。
func (a *Adapter) ValidateImageRequest(options provider.ImageRequestOptions) error {
	if options.ResponseFormat != "" && options.ResponseFormat != "url" && options.ResponseFormat != "b64_json" {
		return &requestError{msg: "response_format 只支持 url 或 b64_json"}
	}
	return nil
}

// --- 同步图像（ImageGenerationAdapter / ImageEditAdapter）---

func (a *Adapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	in, err := a.buildImageInput(request.Model, request.Prompt, request.Size, request.AspectRatio, request.Resolution, request.OutputFormat, request.Background)
	if err != nil {
		return nil, err
	}
	in.count = imageCount(request.Count)
	in.quality = request.Quality
	return a.runSyncImage(ctx, request.Credential, in, request.ResponseFormat)
}

func (a *Adapter) EditImage(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	in, err := a.buildImageInput(request.Model, request.Prompt, request.Size, request.AspectRatio, request.Resolution, request.OutputFormat, request.Background)
	if err != nil {
		return nil, err
	}
	in.count = imageCount(request.Count)
	in.quality = request.Quality
	return a.runSyncImageEdit(ctx, request.Credential, in, request.ImageURLs, request.MaskURL, request.ResponseFormat)
}

func (a *Adapter) runSyncImage(ctx context.Context, credential account.Credential, in imagePayloadInput, responseFormat string) (*provider.Response, error) {
	return a.runSyncImageEdit(ctx, credential, in, nil, "", responseFormat)
}

func (a *Adapter) runSyncImageEdit(ctx context.Context, credential account.Credential, in imagePayloadInput, imageURLs []string, maskURL, responseFormat string) (*provider.Response, error) {
	var body map[string]any
	err := a.withClient(ctx, credential, func(ctx context.Context, c *client) error {
		if c.rc.token == "" {
			return &transportError{err: errNoToken}
		}
		if err := a.resolveInputs(ctx, c, &in, imageURLs, maskURL); err != nil {
			return err
		}
		sub, err := a.submitImageRemint(ctx, c, endpoints.imageSubmit, buildImagePayloads(in), in.prompt)
		if err != nil {
			return err
		}
		urls, err := pollAllUntilDone(ctx, c, sub.pollURL, "image")
		if err != nil {
			return err
		}
		images := make([][]byte, 0, len(urls))
		for _, url := range urls {
			data, _, err := c.download(ctx, url)
			if err != nil {
				return err
			}
			images = append(images, data)
		}
		body, err = a.imageResponseBody(ctx, in, images, responseFormat)
		return err
	})
	if err != nil {
		return nil, err
	}
	return jsonResponse(body), nil
}

// imageResponseBody 渲染 OpenAI Images 风格的响应体（可含多张）。
func (a *Adapter) imageResponseBody(ctx context.Context, in imagePayloadInput, images [][]byte, responseFormat string) (map[string]any, error) {
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
		"usage":   a.ImageUsage(in.prompt, len(in.sourceImageID), "", nil),
	}, nil
}

// imageCount 规范化请求张数（至少 1）。
func imageCount(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// --- 异步图像任务（AsyncImageAdapter）---

func (a *Adapter) SubmitImage(ctx context.Context, request provider.ImageJobRequest, save func(provider.ImageCheckpoint) error) (provider.ImageCheckpoint, error) {
	in, err := a.buildImageInput(request.Model, request.Prompt, request.Size, "", "", request.OutputFormat, request.Background)
	if err != nil {
		return provider.ImageCheckpoint{}, err
	}
	in.count = 1
	in.quality = request.Quality
	var cp provider.ImageCheckpoint
	err = a.withClient(ctx, request.Credential, func(ctx context.Context, c *client) error {
		if c.rc.token == "" {
			return &transportError{err: errNoToken}
		}
		if err := a.resolveInputs(ctx, c, &in, request.ImageURLs, request.MaskURL); err != nil {
			return err
		}
		// 不可逆的提交发生前先落盘 Submitted 标记。
		if err := save(provider.ImageCheckpoint{Submitted: true}); err != nil {
			return err
		}
		sub, err := a.submitImageRemint(ctx, c, endpoints.imageSubmit, buildImagePayloads(in), in.prompt)
		if err != nil {
			return err
		}
		cp = provider.ImageCheckpoint{Submitted: true, UpstreamID: sub.pollURL}
		return save(cp)
	})
	if err != nil {
		return cp, err
	}
	return cp, nil
}

func (a *Adapter) PollImage(ctx context.Context, request provider.ImageJobRequest, cp provider.ImageCheckpoint) (provider.ImageCheckpoint, error) {
	if len(cp.Outputs) > 0 {
		return cp, nil
	}
	if cp.UpstreamID == "" {
		return cp, errors.New("image checkpoint has no polling URL")
	}
	err := a.withClient(ctx, request.Credential, func(ctx context.Context, c *client) error {
		outputURL, err := pollUntilDone(ctx, c, cp.UpstreamID, "image", nil)
		if err != nil {
			return err
		}
		cp.Outputs = []string{outputURL}
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
		raw, _, err := c.download(ctx, cp.Outputs[0])
		if err != nil {
			return err
		}
		data = raw
		return nil
	})
	return data, err
}

// ImageUsage 估算交付图像的 usage 对象。Adobe 媒体按模型计价，这里只给出占位值。
func (a *Adapter) ImageUsage(prompt string, inputImages int, quality string, outputs []image.Point) map[string]any {
	return map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
}

// resolveInputs 把编辑用的输入图与蒙版上传到 Adobe 存储，并记录其 id。
func (a *Adapter) resolveInputs(ctx context.Context, c *client, in *imagePayloadInput, imageURLs []string, maskURL string) error {
	for _, ref := range imageURLs {
		id, err := a.uploadRef(ctx, c, ref, endpoints.imageUpload, "image")
		if err != nil {
			return err
		}
		in.sourceImageID = append(in.sourceImageID, id)
	}
	if strings.TrimSpace(maskURL) != "" {
		id, err := a.uploadRef(ctx, c, maskURL, endpoints.imageUpload, "image")
		if err != nil {
			return err
		}
		in.maskID = id
	}
	return nil
}

// uploadRef 取回 data URI 或 https 引用的字节并上传到 Adobe 存储，返回 asset id。
func (a *Adapter) uploadRef(ctx context.Context, c *client, ref, uploadURL, kind string) (string, error) {
	data, mime, err := fetchRefBytes(ctx, c, ref)
	if err != nil {
		return "", err
	}
	return c.upload(ctx, uploadURL, data, mime, kind)
}

func fetchRefBytes(ctx context.Context, c *client, ref string) ([]byte, string, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "data:") {
		comma := strings.IndexByte(ref, ',')
		if comma < 0 {
			return nil, "", errors.New("invalid data URI")
		}
		meta, payload := ref[5:comma], ref[comma+1:]
		mime := "image/png"
		if i := strings.IndexByte(meta, ';'); i >= 0 {
			mime = meta[:i]
		} else if meta != "" {
			mime = meta
		}
		raw, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, "", err
		}
		return raw, mime, nil
	}
	return c.download(ctx, ref)
}

// pollAllUntilDone 阻塞轮询直到成功，返回全部输出 URL（多图）。
func pollAllUntilDone(ctx context.Context, c *client, pollURL, kind string) ([]string, error) {
	for {
		result, err := c.poll(ctx, pollURL, kind)
		if err != nil {
			return nil, err
		}
		switch result.status {
		case "SUCCEEDED":
			if len(result.outputURLs) == 0 {
				return nil, &upstreamError{stage: "poll", status: 502, body: "no output URL"}
			}
			return result.outputURLs, nil
		case "FAILED":
			return nil, &upstreamError{stage: "poll", status: 502, body: "generation failed"}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// pollUntilDone 阻塞轮询直到任务成功、失败或上下文结束。
func pollUntilDone(ctx context.Context, c *client, pollURL, kind string, progress func(int)) (string, error) {
	for {
		result, err := c.poll(ctx, pollURL, kind)
		if err != nil {
			return "", err
		}
		if progress != nil && result.progress > 0 {
			progress(result.progress)
		}
		switch result.status {
		case "SUCCEEDED":
			if result.outputURL == "" {
				return "", &upstreamError{stage: "poll", status: 502, body: "no output URL"}
			}
			return result.outputURL, nil
		case "FAILED":
			return "", &upstreamError{stage: "poll", status: 502, body: "generation failed"}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func jsonResponse(body map[string]any) *provider.Response {
	encoded, _ := json.Marshal(body)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: io.NopCloser(strings.NewReader(string(encoded)))}
}
