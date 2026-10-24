package muse

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// maxInputImages bounds the attachments of one prompt.
const maxInputImages = 4

func (a *Adapter) ValidateImageRequest(options provider.ImageRequestOptions) error {
	switch {
	case options.HasMask:
		return badRequest("Muse 不支持蒙版编辑")
	case options.Streaming || options.PartialImages > 0:
		return badRequest("Muse 不支持流式出图")
	case options.ResponseFormat != "" && options.ResponseFormat != "url" && options.ResponseFormat != "b64_json":
		return badRequest("response_format 只支持 url 或 b64_json")
	case options.Edit && options.InputImages == 0:
		return badRequest("图片编辑需要输入图片")
	case options.InputImages > maxInputImages:
		return badRequest(fmt.Sprintf("最多 %d 张输入图片", maxInputImages))
	case options.Size != "" && options.Size != "auto" && aspectLabel(options.Size) == "":
		return badRequest("size 必须是 auto 或 WIDTHxHEIGHT")
	}
	return nil
}

func (a *Adapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	prompt := "Create a brand-new image from scratch based only on the description below. Do not use or ask for earlier or reference images. Reply with the image."
	return a.drawImage(ctx, request.Credential, withShape(prompt, request.AspectRatio, request.Size)+"\n\n"+request.Prompt, nil, request.ResponseFormat)
}

func (a *Adapter) EditImage(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	inputs := make([]attachment, 0, len(request.ImageURLs))
	for _, ref := range request.ImageURLs {
		input, err := a.readInput(ctx, ref)
		if err != nil {
			return failureResponse("", err)
		}
		inputs = append(inputs, input)
	}
	prompt := "Edit the image attached to this message as described below. Use only the attached images, not earlier pictures. Reply with the edited image."
	return a.drawImage(ctx, request.Credential, withShape(prompt, request.AspectRatio, request.Size)+"\n\n"+request.Prompt, inputs, request.ResponseFormat)
}

// drawImage asks for one image. Once the prompt is sent the account spent
// usage, so later failures stay errors: the gateway does not draw again.
func (a *Adapter) drawImage(ctx context.Context, credential account.Credential, prompt string, inputs []attachment, responseFormat string) (*provider.Response, error) {
	sent := false
	var file mediaFile
	err := a.withSession(ctx, credential, func(ctx context.Context, s session) error {
		var err error
		file, err = a.engine.Generate(ctx, s, mediaJob{Kind: mediaImage, Prompt: prompt, Inputs: inputs, OnSent: func() { sent = true }})
		return err
	})
	if err != nil {
		return mediaFailure(err, sent)
	}
	item := map[string]any{}
	if responseFormat == "url" && a.assets != nil {
		asset, err := a.assets.SaveImage(ctx, file.Data)
		if err != nil {
			return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
		}
		item["url"] = a.assets.PublicImageURL(asset.ID)
	} else {
		item["b64_json"] = base64.StdEncoding.EncodeToString(file.Data)
	}
	response := jsonResponse(http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": []any{item}})
	response.QuotaUnits = 1
	return response, nil
}

// mediaFailure hands the gateway what it can act on. Before the prompt is
// sent every classified failure is a response (refresh, quota, failover);
// after it only request-scoped and account-state ones are.
func mediaFailure(err error, sent bool) (*provider.Response, error) {
	var known *museError
	if !errors.As(classify(err), &known) {
		return nil, err
	}
	if sent && !known.requestScoped && known.status != http.StatusUnauthorized && known.status != http.StatusTooManyRequests {
		return nil, known
	}
	return errorResponse("", known.status, known.code, known.message), nil
}

// withShape states the requested aspect ratio; the agent takes it from the prompt.
func withShape(prompt, aspectRatio, size string) string {
	label := strings.TrimSpace(aspectRatio)
	if label == "" {
		label = aspectLabel(size)
	}
	if label == "" {
		return prompt
	}
	return prompt + " Aspect ratio: " + label + "."
}

// aspectLabel names the shape of a WIDTHxHEIGHT size, "" when there is none.
func aspectLabel(size string) string {
	width, height, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	w, errW := strconv.Atoi(width)
	h, errH := strconv.Atoi(height)
	if !ok || errW != nil || errH != nil || w <= 0 || h <= 0 {
		return ""
	}
	divisor := gcd(w, h)
	shape := "square"
	if w > h {
		shape = "landscape"
	} else if w < h {
		shape = "portrait"
	}
	return fmt.Sprintf("%d:%d (%s)", w/divisor, h/divisor, shape)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// readInput loads an input image given as a data URI or https URL.
func (a *Adapter) readInput(ctx context.Context, ref string) (attachment, error) {
	ref = strings.TrimSpace(ref)
	var data []byte
	switch meta, payload, _ := strings.Cut(ref, ","); {
	case strings.HasPrefix(meta, "data:") && strings.HasSuffix(meta, ";base64"):
		raw, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return attachment{}, badRequest("输入图片的 data URI 不是有效的 Base64")
		}
		data = raw
	case strings.HasPrefix(ref, "https://") && a.fetchRemote != nil:
		raw, err := a.fetchRemote(ctx, ref)
		if err != nil {
			if ctx.Err() != nil {
				return attachment{}, ctx.Err()
			}
			return attachment{}, badRequest("无法下载输入图片（需可公网访问的 https 地址）")
		}
		data = raw
	default:
		return attachment{}, badRequest("输入图片必须是 Base64 data URI 或 https URL")
	}
	mime := http.DetectContentType(data)
	extension := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}[mime]
	if extension == "" {
		return attachment{}, badRequest("输入图片只支持 PNG、JPEG、WebP 或 GIF")
	}
	return attachment{Name: "image." + extension, MIME: mime, Data: data}, nil
}
