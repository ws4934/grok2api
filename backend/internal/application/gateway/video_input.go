package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func (s *Service) acquireVideoInputSlot(ctx context.Context, references []string) (func(), error) {
	hasLocalInput := false
	for _, reference := range references {
		if strings.HasPrefix(reference, media.InputReferencePrefix) {
			hasLocalInput = true
			break
		}
	}
	if !hasLocalInput || s.mediaInputSlots == nil {
		return func() {}, nil
	}
	select {
	case s.mediaInputSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.mediaInputSlots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// validateVideoInputBudget 校验一组同类输入，并把它们物化后的体积计入共享预算：同一
// 任务的首尾帧、参考图、参考视频与源视频合计不超过 MaxInputJSONBytes。
func (s *Service) validateVideoInputBudget(ctx context.Context, references []string, expectedKind string, budget *int64) error {
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

// resolveVideoInputBudget 把 file_id 引用物化成 data URI（其余原样保留），体积计入共享预算。
func (s *Service) resolveVideoInputBudget(ctx context.Context, references []string, expectedKind string, budget *int64) ([]string, error) {
	resolved := make([]string, 0, len(references))
	for _, reference := range references {
		fileID, local := media.ParseInputReference(reference)
		if !strings.HasPrefix(reference, media.InputReferencePrefix) {
			if !addVideoReferenceBytes(budget, int64(len(reference))) {
				return nil, ErrVideoInputTooLarge
			}
			resolved = append(resolved, reference)
			continue
		}
		if s.mediaAssets == nil || !local {
			return nil, ErrVideoInputUnavailable
		}
		asset, body, err := s.mediaAssets.OpenInputAsset(ctx, fileID)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrVideoInputUnavailable, err)
		}
		if asset.Kind != expectedKind {
			_ = body.Close()
			return nil, fmt.Errorf("%w: file_id 必须引用%s", ErrVideoInputUnavailable, expectedKind)
		}
		if !addVideoReferenceBytes(budget, materializedVideoReferenceBytes(asset.MIMEType, asset.SizeBytes)) {
			_ = body.Close()
			return nil, ErrVideoInputTooLarge
		}
		// Encode straight from the file: only the data URI is held in memory.
		var uri strings.Builder
		uri.Grow(int(materializedVideoReferenceBytes(asset.MIMEType, asset.SizeBytes)))
		uri.WriteString("data:" + asset.MIMEType + ";base64,")
		encoder := base64.NewEncoder(base64.StdEncoding, &uri)
		n, readErr := io.Copy(encoder, io.LimitReader(body, asset.SizeBytes+1))
		closeErr := errors.Join(encoder.Close(), body.Close())
		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("读取视频临时输入: %w", errors.Join(readErr, closeErr))
		}
		if n != asset.SizeBytes {
			return nil, ErrVideoInputUnavailable
		}
		resolved = append(resolved, uri.String())
	}
	return resolved, nil
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

// imageRefs 按 首帧、尾帧、参考图 的顺序列出图像输入（物化后按此顺序回填）。
func (in videoInputs) imageRefs() []string {
	values := make([]string, 0, 2+len(in.referenceURLs))
	for _, raw := range append([]string{in.imageURL, in.lastFrameURL}, in.referenceURLs...) {
		if v := strings.TrimSpace(raw); v != "" {
			values = append(values, v)
		}
	}
	return values
}

// all 列出任务引用的全部输入，用于占用物化名额与任务结束后回收临时输入。
func (in videoInputs) all() []string {
	values := append(in.imageRefs(), in.referenceVideoURLs...)
	if v := strings.TrimSpace(in.videoURL); v != "" {
		values = append(values, v)
	}
	return values
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

// resolveVideoJobInputs 在任务执行时把 file_id 输入物化成 data URI，全部输入共用一个预算。
func (s *Service) resolveVideoJobInputs(ctx context.Context, operation provider.VideoOperation, inputJSON string) (videoInputs, error) {
	in := decodeVideoInputs(inputJSON)
	if in.videoURL != "" && operation == provider.VideoOperationGenerate {
		return videoInputs{}, ErrVideoInputUnavailable
	}
	budget := videoInputJSONBaseBytes
	images, err := s.resolveVideoInputBudget(ctx, in.imageRefs(), "image", &budget)
	if err != nil {
		return videoInputs{}, err
	}
	// 按 imageRefs 的顺序回填，空槽不占位。
	next := 0
	take := func(original string) string {
		if strings.TrimSpace(original) == "" || next >= len(images) {
			return ""
		}
		value := images[next]
		next++
		return value
	}
	out := videoInputs{imageURL: take(in.imageURL), lastFrameURL: take(in.lastFrameURL), referenceAudios: in.referenceAudios}
	for _, raw := range in.referenceURLs {
		if value := take(raw); value != "" {
			out.referenceURLs = append(out.referenceURLs, value)
		}
	}
	if out.referenceVideoURLs, err = s.resolveVideoInputBudget(ctx, in.referenceVideoURLs, "video", &budget); err != nil {
		return videoInputs{}, err
	}
	if in.videoURL != "" {
		videos, err := s.resolveVideoInputBudget(ctx, []string{in.videoURL}, "video", &budget)
		if err != nil {
			return videoInputs{}, err
		}
		if len(videos) == 0 {
			return videoInputs{}, ErrVideoInputUnavailable
		}
		out.videoURL = videos[0]
	}
	return out, nil
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

func (s *Service) releaseVideoInputs(job media.Job) {
	if s.mediaAssets == nil {
		return
	}
	references := decodeVideoInput(job.InputJSON)
	if len(references) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.mediaAssets.ReleaseInputAssets(ctx, references); err != nil {
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("video_input_release_failed", "job_id", job.ID, "error", err)
	}
}
