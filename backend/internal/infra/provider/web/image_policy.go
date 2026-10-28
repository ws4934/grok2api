package web

import (
	"errors"
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func (*Adapter) NormalizeImageRequest(o provider.ImageRequestOptions) (provider.ImageRequestOptions, error) {
	if o.HasMask {
		return o, errors.New("当前模型不支持蒙版（mask），请选择支持蒙版的模型")
	}
	o.ResponseFormat = strings.ToLower(strings.TrimSpace(o.ResponseFormat))
	if o.ResponseFormat == "" {
		o.ResponseFormat = "url"
	}
	if o.ResponseFormat != "url" && o.ResponseFormat != "b64_json" {
		return o, errors.New("response_format 必须是 url 或 b64_json")
	}
	o.Count = min(max(o.Count, 1), maxGeneratedImages)
	if o.Edit {
		limit := 8
		if o.UpstreamModel == "grok-imagine-image-2.0" {
			limit = 5
		}
		if o.InputImages < 1 || o.InputImages > limit {
			return o, fmt.Errorf("image 数量必须在 1 到 %d 之间", limit)
		}
		o.Count = 1
	}
	spec, _ := Resolve(o.UpstreamModel)
	if spec.ProtocolModel == "imagine-lite" && o.Streaming {
		return o, errors.New("当前模型不支持 stream")
	}
	if o.UpstreamModel == "grok-imagine-image-2.0" {
		o.AspectRatio = resolveImage20AspectRatio(o.AspectRatio, o.Size)
	} else {
		o.AspectRatio = resolveImageAspectRatio(o.AspectRatio, o.Size)
	}
	o.Size, o.Resolution, o.Quality = "", "", ""
	return o, nil
}

func (*Adapter) ImageQuotaUnits(mode string, o provider.ImageRequestOptions) (int, bool) {
	if mode == "weekly" {
		return 0, false
	}
	return max(0, o.Count), true
}
