package console

import (
	"errors"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/grokimage"
)

func (a *Adapter) NormalizeImageRequest(o provider.ImageRequestOptions) (provider.ImageRequestOptions, error) {
	if o.Streaming || o.PartialImages != 0 {
		return o, errors.New("当前模型不支持 stream 或 partial_images")
	}
	if o.HasMask {
		return o, errors.New("当前模型不支持蒙版（mask），请选择支持蒙版的模型")
	}
	if o.Edit && o.InputImages == 0 {
		return o, errors.New("图片编辑需要至少一张图片")
	}
	o.Count = min(max(o.Count, 1), 10)
	o.InputImages = min(o.InputImages, consoleMaxEditImages)
	format, err := normalizeConsoleImageFormat(o.ResponseFormat)
	if err != nil {
		return o, err
	}
	o.ResponseFormat = format
	o.AspectRatio = resolveConsoleImageAspectRatio(o.AspectRatio, o.Size)
	o.Size = ""
	o.Resolution = grokimage.Resolution(o.Resolution)
	if o.Resolution == "" {
		o.Resolution = "1k"
	}
	o.Quality = consoleImageQuality(o.UpstreamModel, o.Quality)
	if o.UpstreamModel == "grok-imagine-image-2.0" && o.Quality == "" {
		// Official auto: low for generation, medium for editing. Send the
		// effective level explicitly so the served tier and price agree.
		o.Quality = "low"
		if o.Edit {
			o.Quality = "medium"
		}
	}
	return o, nil
}

func (*Adapter) ImageQuotaUnits(mode string, o provider.ImageRequestOptions) (int, bool) {
	if mode != QuotaModeImage {
		return 0, false
	}
	if o.Count <= 0 {
		return 0, true
	}
	var cost audit.PricingResult
	var ok bool
	if o.Edit {
		cost, ok = audit.EstimateOfficialImageEditCost(o.UpstreamModel, o.Resolution, o.Quality, o.Count, o.InputImages)
	} else {
		cost, ok = audit.EstimateOfficialImageCost(o.UpstreamModel, o.Resolution, o.Quality, o.Count)
	}
	// One USD tick is 1e-10 USD; the Console window is integer cents.
	const ticksPerCent int64 = 100_000_000
	return int((cost.CostInUSDTicks + ticksPerCent - 1) / ticksPerCent), ok
}

func (*Adapter) QuotaPoolMode(mode string) string {
	if isConsoleQuotaMode(strings.TrimSpace(mode)) {
		return QuotaMode
	}
	return mode
}
