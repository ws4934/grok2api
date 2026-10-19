// Package adobe 把 Adobe Firefly 的图像与视频生成接成一个原生网关 Provider。
// 图像走共享的 AsyncImageAdapter 任务引擎，视频走 VideoAdapter；持久任务、账号
// 池、出口代理、定价与审计等公共设施全部直接复用。
package adobe

import (
	"math"
	"slices"
	"strings"
)

// imageModel 把一个对外基础模型名映射到 Firefly 上游的 modelId 与 modelVersion。
// 分辨率与比例仍是请求参数，所以目录只暴露少量基础名，而不是参考项目里几百个
// 维度化 ID。
type imageModel struct {
	id           string
	upstreamID   string // modelId
	upstreamVer  string // modelVersion
	groundSearch bool   // sends groundSearch:false（官网只对 Nano Banana Pro / 3.1 发送）
	nanoRatios   bool   // nano-banana-3 also allows 1:8/1:4/4:1/8:1
	// ratioSizes 非空表示模型只出 1K、尺寸随比例变化（Gemini 2.5 Nano Banana）。
	ratioSizes map[string]pixels
}

// 模型版本号核对自 firefly.adobe.com 前端（2026-10-19）：nano-banana = Gemini 2.5，
// nano-banana-2 = Gemini 3 (Nano Banana Pro)，nano-banana-3 = Gemini 3.1 (Nano Banana 2)。
var imageModels = []imageModel{
	{id: "nano-banana-pro", upstreamID: "gemini-flash", upstreamVer: "nano-banana-2", groundSearch: true},
	{id: "nano-banana", upstreamID: "gemini-flash", upstreamVer: "nano-banana", ratioSizes: nanoBanana25Sizes},
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
	defaultDur    int
	defaultRatio  string
	defaultRes    string // "" means the engine ignores a resolution tier
	genAudio      bool
	referenceMode string // "image" for veo31-ref
	durations     []int
	ratios        []string
	resolutions   []string // empty means resolution is not a tier for this engine
}

// 视频引擎核对自 firefly.adobe.com 前端（2026-10-19）：Sora 已不在官网模型列表中。
var videoModels = []videoModel{
	{id: "veo31", engine: "veo31-standard", defaultDur: 8, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{4, 6, 8}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p", "1080p"}},
	{id: "veo31-ref", engine: "veo31-standard", referenceMode: "image", defaultDur: 8, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{4, 6, 8}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p", "1080p"}},
	{id: "gemini-omni", engine: "gemini-omni", defaultDur: 8, defaultRatio: "16:9", defaultRes: "720p", durations: []int{4, 6, 8, 10}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p", "1080p"}},
	{id: "kling-o3", engine: "kling-o3", defaultDur: 5, defaultRatio: "16:9", defaultRes: "1080p", genAudio: true, durations: []int{5, 15}, ratios: []string{"16:9", "9:16"}, resolutions: []string{"720p", "1080p"}},
	{id: "kling3", engine: "kling3", defaultDur: 5, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{5, 10, 15}, ratios: []string{"16:9", "9:16", "1:1"}, resolutions: []string{"720p", "1080p"}},
	{id: "seedance20", engine: "seedance", defaultDur: 5, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, ratios: []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"}, resolutions: []string{"480p", "720p", "1080p"}},
	{id: "seedance20-fast", engine: "seedance-fast", defaultDur: 5, defaultRatio: "16:9", defaultRes: "720p", genAudio: true, durations: []int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, ratios: []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"}, resolutions: []string{"480p", "720p"}},
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

// imageRatios 列出 Firefly 图像接受的 aspectRatio（顺序即平局时的优先级）。
var imageRatios = []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "21:9"}

// nanoExtraRatios 是只有 nano-banana-3 系列接受的极端比例。
var nanoExtraRatios = []string{"4:1", "1:4", "8:1", "1:8"}

func imageRatioChoices(nano bool) []string {
	if nano {
		return append(slices.Clone(imageRatios), nanoExtraRatios...)
	}
	return imageRatios
}

func supportedImageRatio(ratio string, nano bool) bool {
	return slices.Contains(imageRatioChoices(nano), ratio)
}

// closestRatio 在 choices 里挑与 w:h 最接近的比例（对数距离，2:1 与 1:2 离 1:1 一样远）。
func closestRatio(w, h int, choices []string) string {
	best, bestDiff := "", math.Inf(1)
	for _, choice := range choices {
		cw, ch := parsePair(choice)
		if cw == 0 || ch == 0 {
			continue
		}
		if diff := math.Abs(math.Log(float64(w*ch) / float64(h*cw))); diff < bestDiff {
			best, bestDiff = choice, diff
		}
	}
	return best
}

// ratioFromSize 把 OpenAI 风格的 size（WIDTHxHEIGHT）映射到最接近的受支持比例；
// 缺省或 auto 为 1:1。
func ratioFromSize(size string, nano bool) string {
	w, h := parsePair(size)
	if w == 0 || h == 0 {
		return "1:1"
	}
	return closestRatio(w, h, imageRatioChoices(nano))
}

// nearestRatio 把客户端比例对齐到模型支持的最接近比例；无法解析时用 fallback。
func nearestRatio(value string, choices []string, fallback string) string {
	w, h := parsePair(value)
	if w == 0 || h == 0 || len(choices) == 0 {
		return fallback
	}
	return closestRatio(w, h, choices)
}

// nearestDuration 把时长对齐到模型支持的最接近秒数（等距取较短）；未指定时用 fallback。
func nearestDuration(value int, choices []int, fallback int) int {
	if value <= 0 || len(choices) == 0 {
		return fallback
	}
	best := choices[0]
	for _, choice := range choices {
		if abs(choice-value) < abs(best-value) {
			best = choice
		}
	}
	return best
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// tierFromSize 在未显式指定分辨率时，用 size 的长边推断 1K/2K/4K 档位，缺省 2K。
func tierFromSize(size string) string {
	w, h := parsePair(size)
	switch side := max(w, h); {
	case side == 0:
		return "2K"
	case side < 1500:
		return "1K"
	case side < 2700:
		return "2K"
	default:
		return "4K"
	}
}

// parsePair 解析 "1024x768"、"16:9" 这类由两个正整数组成的值；无效时返回 0, 0。
func parsePair(value string) (int, int) {
	value = strings.TrimSpace(value)
	for _, sep := range []string{"x", "X", "*", "×", ":"} {
		if i := strings.Index(value, sep); i > 0 {
			return atoi(value[:i]), atoi(value[i+len(sep):])
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
