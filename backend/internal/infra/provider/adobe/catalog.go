// Package adobe 把 Adobe Firefly 的图像与视频生成接成一个原生网关 Provider。
// 图像走共享的 AsyncImageAdapter 任务引擎，视频走 VideoAdapter；持久任务、账号
// 池、出口代理、定价与审计等公共设施全部直接复用。
package adobe

import (
	"strings"
)

// imageModel 把一个对外基础模型名映射到 Firefly 上游的 modelId 与 modelVersion。
// 分辨率与比例仍是请求参数，所以目录只暴露少量基础名，而不是参考项目里几百个
// 维度化 ID。
type imageModel struct {
	id           string
	upstreamID   string // modelId
	upstreamVer  string // modelVersion
	groundSearch bool   // sends groundSearch:false (nano-banana family)
	nanoRatios   bool   // nano-banana-3 also allows 1:8/1:4/4:1/8:1
}

var imageModels = []imageModel{
	{id: "nano-banana-pro", upstreamID: "gemini-flash", upstreamVer: "nano-banana-2", groundSearch: true},
	{id: "nano-banana", upstreamID: "gemini-flash", upstreamVer: "nano-banana-2", groundSearch: true},
	{id: "nano-banana2", upstreamID: "gemini-flash", upstreamVer: "nano-banana-3", groundSearch: true, nanoRatios: true},
	{id: "gpt-image-2", upstreamID: "gpt-image", upstreamVer: "2"},
	// gpt-image-2.5 是当前线上的 2.5 引擎（官网默认用 flare 变体）。
	{id: "gpt-image-2.5", upstreamID: "gpt-image", upstreamVer: "gpt-image-2.5-flare"},
	{id: "gpt-image-2.5-flare", upstreamID: "gpt-image", upstreamVer: "gpt-image-2.5-flare"},
	{id: "gpt-image-2.5-sunburst", upstreamID: "gpt-image", upstreamVer: "gpt-image-2.5-prism"},
}

// videoModel 把一个对外基础模型名映射到 Firefly 引擎。时长、比例与分辨率仍是
// 请求参数，按下面每个引擎的允许集合校验。
type videoModel struct {
	id            string
	engine        string
	upstreamModel string // sora "model" field
	defaultDur    int
	defaultRatio  string
	defaultRes    string // "" means the engine ignores a resolution tier
	genAudio      bool
	referenceMode string // "image" for veo31-ref
	durations     []int
	ratios        []string
	resolutions   []string // empty means resolution is not a tier for this engine
}

var videoModels = []videoModel{
	{id: "sora2", engine: "sora2", upstreamModel: "openai:firefly:colligo:sora2", defaultDur: 8, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{4, 8, 12}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p"}},
	{id: "sora2-pro", engine: "sora2-pro", upstreamModel: "openai:firefly:colligo:sora2-pro", defaultDur: 8, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{4, 8, 12}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p"}},
	{id: "veo31", engine: "veo31-standard", defaultDur: 8, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{4, 6, 8}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p", "1080p"}},
	{id: "veo31-ref", engine: "veo31-standard", referenceMode: "image", defaultDur: 8, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{4, 6, 8}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p", "1080p"}},
	{id: "gemini-omni", engine: "gemini-omni", defaultDur: 8, defaultRatio: "16:9", defaultRes: "720p", durations: []int{4, 6, 8, 10}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p", "1080p"}},
	{id: "kling-o3", engine: "kling-o3", defaultDur: 5, defaultRatio: "16:9", defaultRes: "1080p", genAudio: true, durations: []int{5, 15}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p", "1080p"}},
	{id: "kling3", engine: "kling3", defaultDur: 5, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{5, 10, 15}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p", "1080p"}},
	{id: "seedance20", engine: "seedance", defaultDur: 5, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, ratios: []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"}, resolutions: []string{"480p", "720p", "1080p"}},
	{id: "seedance20-fast", engine: "seedance-fast", defaultDur: 5, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, ratios: []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"}, resolutions: []string{"480p", "720p", "1080p"}},
}

func imageModelByID(id string) (imageModel, bool) {
	for _, m := range imageModels {
		if m.id == id {
			return m, true
		}
	}
	return imageModel{}, false
}

func videoModelByID(id string) (videoModel, bool) {
	for _, m := range videoModels {
		if m.id == id {
			return m, true
		}
	}
	return videoModel{}, false
}

// imageRatios 列出映射到 Firefly aspectRatio 的客户端比例。
var imageRatios = map[string]bool{
	"1:1": true, "16:9": true, "9:16": true, "4:3": true, "3:4": true,
	"5:4": true, "4:5": true, "3:2": true, "2:3": true, "21:9": true,
}

// nanoExtraRatios 是只有 nano-banana-3 系列接受的极端比例。
var nanoExtraRatios = map[string]bool{"1:8": true, "1:4": true, "4:1": true, "8:1": true}

func supportedImageRatio(ratio string, nano bool) bool {
	if imageRatios[ratio] {
		return true
	}
	return nano && nanoExtraRatios[ratio]
}

// normalizeRatio 保留受支持的比例，"auto" 映射为 1:1，否则回退到模型默认值。
func normalizeRatio(value, fallback string, nano bool) string {
	candidate := strings.ToLower(strings.TrimSpace(value))
	if candidate == "auto" {
		return "1:1"
	}
	if supportedImageRatio(candidate, nano) {
		return candidate
	}
	return fallback
}

// normalizeResolution 保留 1K/2K/4K 档位，否则回退到模型默认值。
func normalizeResolution(value, fallback string) string {
	candidate := strings.ToUpper(strings.TrimSpace(value))
	switch candidate {
	case "1K", "2K", "4K":
		return candidate
	default:
		return fallback
	}
}

// ratioFromSize 从 OpenAI 风格的 size 字符串推断比例。
func ratioFromSize(size string) string {
	switch strings.TrimSpace(size) {
	case "1024x1792", "1536x2752", "1080x1920":
		return "9:16"
	case "1792x1024", "2752x1536", "1920x1080":
		return "16:9"
	case "2048x1536", "2400x1792":
		return "4:3"
	case "1536x2048", "1792x2400":
		return "3:4"
	default:
		return "1:1"
	}
}

// tierFromSize 在未显式指定分辨率时，用 size 的长边推断 1K/2K/4K 档位。
func tierFromSize(size string) string {
	w, h := parseSize(size)
	side := w
	if h > side {
		side = h
	}
	switch {
	case side == 0:
		return ""
	case side < 1500:
		return "1K"
	case side < 2700:
		return "2K"
	default:
		return "4K"
	}
}

func parseSize(size string) (int, int) {
	size = strings.TrimSpace(size)
	for _, sep := range []string{"x", "X", "*", "×"} {
		if i := strings.Index(size, sep); i > 0 {
			return atoi(size[:i]), atoi(size[i+len(sep):])
		}
	}
	return 0, 0
}

func atoi(value string) int {
	n := 0
	for _, c := range strings.TrimSpace(value) {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
