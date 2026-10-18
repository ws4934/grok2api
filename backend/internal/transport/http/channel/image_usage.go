package channelhttp

import (
	"context"
	"fmt"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/gin-gonic/gin"
)

// imageUsage estimates official usage for the succeeded items: the upstream
// reports none. Input is the prompt estimate plus the channel's per-image
// vision estimate; output follows the gpt-image patch formula on stored sizes.
func (h *Handler) imageUsage(ctx context.Context, job domain.ImageJob, items []domain.ImageItem) gin.H {
	r := job.Request
	text := h.service.EstimateTextTokens(r.Prompt)
	var images, output int64
	if cfg, e := h.service.Settings(ctx, job.Provider); e == nil {
		images = int64(len(r.Inputs)) * int64(cfg.VisionTokens)
	}
	for _, item := range items {
		if item.Status == "succeeded" {
			output += outputImageTokens(item, r)
		}
	}
	input := text + images
	return gin.H{
		"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
		"input_tokens_details":  gin.H{"text_tokens": text, "image_tokens": images, "cached_tokens": 0},
		"output_tokens_details": gin.H{"text_tokens": 0, "image_tokens": output, "reasoning_tokens": 0},
	}
}

// outputImageTokens: ceil(w/32)*ceil(h/32) patches, times 17/64 for low,
// 65/16 for high and 33/32 otherwise; unknown sizes count as 1024x1024.
func outputImageTokens(item domain.ImageItem, r domain.ImageRequest) int64 {
	w, h := item.Width, item.Height
	if w <= 0 || h <= 0 {
		w, h = 1024, 1024
		_, _ = fmt.Sscanf(r.Size, "%dx%d", &w, &h)
	}
	p := int64(max(w, 0)+31) / 32 * (int64(max(h, 0)+31) / 32)
	switch r.Quality {
	case "low":
		return (p*17 + 63) / 64
	case "high", "hd":
		return (p*65 + 15) / 16
	}
	return (p*33 + 31) / 32
}

// imageSize reports the stored output size, falling back to the requested one.
func imageSize(item domain.ImageItem, r domain.ImageRequest) string {
	if item.Width > 0 && item.Height > 0 {
		return fmt.Sprintf("%dx%d", item.Width, item.Height)
	}
	return r.Size
}
