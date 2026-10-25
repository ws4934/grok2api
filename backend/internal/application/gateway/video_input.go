package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// validateInputBudget 校验一组同类输入（暂存引用须存在且类型一致），并把它们按 data URI
// 计的体积计入共享预算：同一任务的全部输入合计不超过 MaxInputJSONBytes。
func (s *Service) validateInputBudget(ctx context.Context, references []string, expectedKind string, budget *int64) error {
	for _, reference := range references {
		fileID, local := media.ParseInputReference(reference)
		if !strings.HasPrefix(reference, media.InputReferencePrefix) {
			if !addVideoReferenceBytes(budget, int64(len(reference))) {
				return ErrVideoInputTooLarge
			}
			continue
		}
		if s.mediaAssets == nil || !local {
			return ErrVideoInputUnavailable
		}
		asset, body, err := s.mediaAssets.OpenInputAsset(ctx, fileID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVideoInputUnavailable, err)
		}
		_ = body.Close()
		if asset.Kind != expectedKind {
			return fmt.Errorf("%w: file_id 必须引用%s", ErrVideoInputUnavailable, expectedKind)
		}
		if !addVideoReferenceBytes(budget, materializedVideoReferenceBytes(asset.MIMEType, asset.SizeBytes)) {
			return ErrVideoInputTooLarge
		}
	}
	return nil
}

func materializedVideoReferenceBytes(mimeType string, sizeBytes int64) int64 {
	if sizeBytes <= 0 || sizeBytes > int64(media.MaxInputJSONBytes) {
		return -1
	}
	return int64(len("data:")+len(mimeType)+len(";base64,")) + ((sizeBytes+2)/3)*4
}

func addVideoReferenceBytes(total *int64, referenceBytes int64) bool {
	// 两个引号加一个逗号是保守的单元 JSON 开销（首项不需要逗号）。
	const jsonElementOverhead = 3
	addition := referenceBytes + jsonElementOverhead
	limit := int64(media.MaxInputJSONBytes)
	if referenceBytes < 0 || addition < 0 || *total > limit-addition {
		return false
	}
	*total += addition
	return true
}

// videoInputs 是视频任务的全部输入引用：url、data URI 或临时输入 file_id 引用。
type videoInputs struct {
	imageURL, lastFrameURL, videoURL  string
	referenceURLs, referenceVideoURLs []string
	referenceAudios                   []string
}

// fittedTo keeps the references the route's provider takes, as its
// normalized options count them: the first ones, in the client's order.
func (in videoInputs) fittedTo(options provider.VideoRequestOptions) videoInputs {
	in.referenceURLs = in.referenceURLs[:min(len(in.referenceURLs), max(options.ReferenceImages, 0))]
	in.referenceVideoURLs = in.referenceVideoURLs[:min(len(in.referenceVideoURLs), max(options.ReferenceVideos, 0))]
	in.referenceAudios = in.referenceAudios[:min(len(in.referenceAudios), max(options.ReferenceAudios, 0))]
	return in
}

// imageRefs 按 首帧、尾帧、参考图 的顺序列出图像输入。
func (in videoInputs) imageRefs() []string {
	values := make([]string, 0, 2+len(in.referenceURLs))
	for _, raw := range append([]string{in.imageURL, in.lastFrameURL}, in.referenceURLs...) {
		if v := strings.TrimSpace(raw); v != "" {
			values = append(values, v)
		}
	}
	return values
}

// videoRefs 列出参考视频与编辑、延长的源视频。
func (in videoInputs) videoRefs() []string {
	return trimmedValues(append(slices.Clone(in.referenceVideoURLs), in.videoURL))
}

// all 列出任务引用的全部输入，用于任务结束后回收临时输入。
func (in videoInputs) all() []string {
	return append(in.imageRefs(), in.videoRefs()...)
}

func encodeVideoInputs(in videoInputs) (string, error) {
	payload := map[string]any{}
	if value := strings.TrimSpace(in.imageURL); value != "" {
		payload["image_url"] = value
	}
	if value := strings.TrimSpace(in.lastFrameURL); value != "" {
		payload["last_frame_url"] = value
	}
	if refs := trimmedValues(in.referenceURLs); len(refs) > 0 {
		payload["reference_urls"] = refs
	}
	if videos := trimmedValues(in.referenceVideoURLs); len(videos) > 0 {
		payload["reference_video_urls"] = videos
	}
	if audios := normalizeVideoReferenceAudios(in.referenceAudios); len(audios) > 0 {
		payload["reference_audios"] = audios
	}
	if value := strings.TrimSpace(in.videoURL); value != "" {
		payload["video_url"] = value
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码视频输入: %w", err)
	}
	if len(data) > media.MaxInputJSONBytes {
		return "", ErrVideoInputTooLarge
	}
	return string(data), nil
}

func trimmedValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, raw := range values {
		if value := strings.TrimSpace(raw); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// decodeVideoInput 返回任务引用的全部输入（首帧、尾帧、参考图、参考视频、视频）。
func decodeVideoInput(value string) []string { return decodeVideoInputs(value).all() }

func decodeVideoInputs(value string) videoInputs {
	var input struct {
		ImageURL           string   `json:"image_url"`
		LastFrameURL       string   `json:"last_frame_url"`
		ReferenceURLs      []string `json:"reference_urls"`
		ReferenceVideoURLs []string `json:"reference_video_urls"`
		ReferenceAudios    []string `json:"reference_audios"`
		VideoURL           string   `json:"video_url"`
	}
	_ = json.Unmarshal([]byte(value), &input)
	return videoInputs{
		imageURL: strings.TrimSpace(input.ImageURL), lastFrameURL: strings.TrimSpace(input.LastFrameURL),
		videoURL: strings.TrimSpace(input.VideoURL), referenceURLs: trimmedValues(input.ReferenceURLs),
		referenceVideoURLs: trimmedValues(input.ReferenceVideoURLs), referenceAudios: normalizeVideoReferenceAudios(input.ReferenceAudios),
	}
}

// checkVideoJobInputs 在任务执行前确认暂存输入仍在且类型正确，不读入内容：适配器
// 只在上传那一刻经 provider.OpenInput 读盘。
func (s *Service) checkVideoJobInputs(ctx context.Context, operation provider.VideoOperation, inputJSON string) (videoInputs, error) {
	in := decodeVideoInputs(inputJSON)
	if in.videoURL != "" && operation == provider.VideoOperationGenerate {
		return videoInputs{}, ErrVideoInputUnavailable
	}
	budget := videoInputJSONBaseBytes
	if err := s.validateInputBudget(ctx, in.imageRefs(), "image", &budget); err != nil {
		return videoInputs{}, err
	}
	if err := s.validateInputBudget(ctx, in.videoRefs(), "video", &budget); err != nil {
		return videoInputs{}, err
	}
	return in, nil
}

func normalizeVideoReferenceAudios(values []string) []string {
	out := make([]string, 0, len(values))
	for _, raw := range values {
		if value := strings.TrimSpace(raw); value != "" {
			out = append(out, value)
		}
	}
	return out
}
