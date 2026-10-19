package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

func (s *Service) validateVideoInputReferences(ctx context.Context, references []string, expectedKind string) error {
	estimatedBytes := videoInputJSONBaseBytes
	for _, reference := range references {
		fileID, local := media.ParseInputReference(reference)
		if !strings.HasPrefix(reference, media.InputReferencePrefix) {
			if !addVideoReferenceBytes(&estimatedBytes, int64(len(reference))) {
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
		if !addVideoReferenceBytes(&estimatedBytes, materializedVideoReferenceBytes(asset.MIMEType, asset.SizeBytes)) {
			return ErrVideoInputTooLarge
		}
	}
	return nil
}

func (s *Service) resolveVideoInputParts(ctx context.Context, inputJSON string) (string, []string, error) {
	imageURL, referenceURLs := decodeVideoInputParts(inputJSON)
	all := videoInputReferences(imageURL, referenceURLs)
	resolved, err := s.resolveVideoInputReferences(ctx, all, "image")
	if err != nil {
		return "", nil, err
	}
	// Map resolved URLs back while preserving empty/non-empty slots by original order.
	next := 0
	take := func(original string) string {
		original = strings.TrimSpace(original)
		if original == "" {
			return ""
		}
		if next >= len(resolved) {
			return original
		}
		value := resolved[next]
		next++
		return value
	}
	outImage := take(imageURL)
	outRefs := make([]string, 0, len(referenceURLs))
	for _, raw := range referenceURLs {
		if value := take(raw); value != "" {
			outRefs = append(outRefs, value)
		}
	}
	return outImage, outRefs, nil
}

func (s *Service) resolveVideoInputReferences(ctx context.Context, references []string, expectedKind string) ([]string, error) {
	resolved := make([]string, 0, len(references))
	estimatedBytes := videoInputJSONBaseBytes
	for _, reference := range references {
		fileID, local := media.ParseInputReference(reference)
		if !strings.HasPrefix(reference, media.InputReferencePrefix) {
			if !addVideoReferenceBytes(&estimatedBytes, int64(len(reference))) {
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
		data, readErr := io.ReadAll(io.LimitReader(body, media.MaxInputJSONBytes+1))
		closeErr := body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("读取视频临时输入: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("关闭视频临时输入: %w", closeErr)
		}
		if len(data) == 0 || len(data) > media.MaxInputJSONBytes {
			return nil, ErrVideoInputTooLarge
		}
		if !addVideoReferenceBytes(&estimatedBytes, materializedVideoReferenceBytes(asset.MIMEType, int64(len(data)))) {
			return nil, ErrVideoInputTooLarge
		}
		resolved = append(resolved, "data:"+asset.MIMEType+";base64,"+base64.StdEncoding.EncodeToString(data))
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

func encodeVideoInput(imageURL string, referenceURLs []string) (string, error) {
	return encodeVideoInputFull(provider.VideoOperationGenerate, imageURL, referenceURLs, nil, "")
}

func encodeVideoInputFull(operation provider.VideoOperation, imageURL string, referenceURLs []string, referenceAudios []string, videoURL string) (string, error) {
	payload := map[string]any{}
	if operation == "" {
		operation = provider.VideoOperationGenerate
	}
	if operation != provider.VideoOperationGenerate {
		payload["operation"] = string(operation)
	}
	if value := strings.TrimSpace(imageURL); value != "" {
		payload["image_url"] = value
	}
	refs := make([]string, 0, len(referenceURLs))
	for _, raw := range referenceURLs {
		if value := strings.TrimSpace(raw); value != "" {
			refs = append(refs, value)
		}
	}
	if len(refs) > 0 {
		payload["reference_urls"] = refs
	}
	audios := normalizeVideoReferenceAudios(referenceAudios)
	if len(audios) > 0 {
		payload["reference_audios"] = audios
	}
	if value := strings.TrimSpace(videoURL); value != "" {
		payload["video_url"] = value
	}
	// Keep a combined image_urls field for older readers/tools that only
	// understand the pre-split shape. New workers prefer image_url/reference_urls.
	combined := videoInputReferences(imageURL, referenceURLs)
	if len(combined) > 0 {
		payload["image_urls"] = combined
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

func decodeVideoInput(value string) []string {
	imageURL, refs, videoURL := decodeVideoInputFull(value)
	values := videoInputReferences(imageURL, refs)
	if v := strings.TrimSpace(videoURL); v != "" {
		values = append(values, v)
	}
	return values
}

func decodeVideoInputParts(value string) (string, []string) {
	imageURL, refs, _ := decodeVideoInputFull(value)
	return imageURL, refs
}

func decodeVideoInputFull(value string) (string, []string, string) {
	imageURL, refs, _, videoURL := decodeVideoInputDetailed(value)
	return imageURL, refs, videoURL
}

func decodeVideoInputDetailed(value string) (string, []string, []string, string) {
	var input struct {
		Operation       string   `json:"operation"`
		ImageURL        string   `json:"image_url"`
		ReferenceURLs   []string `json:"reference_urls"`
		ReferenceAudios []string `json:"reference_audios"`
		ImageURLs       []string `json:"image_urls"`
		VideoURL        string   `json:"video_url"`
	}
	_ = json.Unmarshal([]byte(value), &input)
	videoURL := strings.TrimSpace(input.VideoURL)
	audios := normalizeVideoReferenceAudios(input.ReferenceAudios)
	if strings.TrimSpace(input.ImageURL) != "" || len(input.ReferenceURLs) > 0 || len(audios) > 0 || videoURL != "" || strings.TrimSpace(input.Operation) != "" {
		refs := make([]string, 0, len(input.ReferenceURLs))
		for _, raw := range input.ReferenceURLs {
			if v := strings.TrimSpace(raw); v != "" {
				refs = append(refs, v)
			}
		}
		return strings.TrimSpace(input.ImageURL), refs, audios, videoURL
	}
	// Legacy jobs stored a flat image_urls list. Preserve historical mapping:
	// one URL was treated as the first-frame image; multiple URLs were references.
	legacy := make([]string, 0, len(input.ImageURLs))
	for _, raw := range input.ImageURLs {
		if v := strings.TrimSpace(raw); v != "" {
			legacy = append(legacy, v)
		}
	}
	switch len(legacy) {
	case 0:
		return "", nil, nil, ""
	case 1:
		return legacy[0], nil, nil, ""
	default:
		return "", legacy, nil, ""
	}
}

func decodeVideoOperation(value string) provider.VideoOperation {
	var input struct {
		Operation string `json:"operation"`
	}
	_ = json.Unmarshal([]byte(value), &input)
	switch provider.VideoOperation(strings.TrimSpace(input.Operation)) {
	case provider.VideoOperationEdit:
		return provider.VideoOperationEdit
	case provider.VideoOperationExtend:
		return provider.VideoOperationExtend
	default:
		return provider.VideoOperationGenerate
	}
}

func (s *Service) resolveVideoJobInputs(ctx context.Context, operation provider.VideoOperation, inputJSON string) (string, []string, []string, string, error) {
	_, _, referenceAudios, videoURL := decodeVideoInputDetailed(inputJSON)
	resolvedImage, resolvedRefs, err := s.resolveVideoInputParts(ctx, inputJSON)
	if err != nil {
		return "", nil, nil, "", err
	}
	if strings.TrimSpace(videoURL) == "" {
		return resolvedImage, resolvedRefs, referenceAudios, "", nil
	}
	if operation == provider.VideoOperationGenerate {
		return "", nil, nil, "", ErrVideoInputUnavailable
	}
	resolvedVideos, err := s.resolveVideoInputReferences(ctx, []string{videoURL}, "video")
	if err != nil {
		return "", nil, nil, "", err
	}
	if len(resolvedVideos) == 0 {
		return "", nil, nil, "", ErrVideoInputUnavailable
	}
	return resolvedImage, resolvedRefs, referenceAudios, resolvedVideos[0], nil
}

func validateVideoReferenceAudios(values []string) error {
	if len(values) > 3 {
		return fmt.Errorf("reference_audios 最多 3 个")
	}
	for _, raw := range values {
		if strings.TrimSpace(raw) == "" {
			return fmt.Errorf("reference_audios.voice_id 不能为空")
		}
	}
	return nil
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

func videoInputReferences(imageURL string, referenceURLs []string) []string {
	values := make([]string, 0, 1+len(referenceURLs))
	if v := strings.TrimSpace(imageURL); v != "" {
		values = append(values, v)
	}
	for _, raw := range referenceURLs {
		if v := strings.TrimSpace(raw); v != "" {
			values = append(values, v)
		}
	}
	return values
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
