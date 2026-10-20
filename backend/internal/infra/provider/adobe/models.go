// Package adobe 把 Adobe Firefly 的图像与视频生成接成一个原生网关 Provider。
// 模型目录是本文件里的手写表；请求体按 firefly.adobe.com 前端的规则构建。图像走
// 共享的 AsyncImageAdapter 任务引擎，视频走 VideoAdapter。
package adobe

import (
	"slices"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

// modelVersion 是目录里的一个上游模型版本。取值抄自官方 discovery schema，填法抄自
// firefly.adobe.com 前端（2026-10-19 核对）。Adobe 后来改了什么，用可选测试
// TestCatalogMatchesDiscovery 对照（见 catalog_check_test.go）。
type modelVersion struct {
	slug    string // 对外模型名，例如 "veo-3.1"
	modelID string // 上游 modelId，例如 "veo"
	version string // 上游 modelVersion，例如 "3.1-generate"
	video   bool

	// --- 取值（schema）---
	fields          []string // 请求体顶层字段里模型接受的那些，其余不发
	aspects         []string // 可选比例（含 auto）；顺序即就近对齐的平局优先级
	settingsAspect  bool     // 有 generationSettings.aspectRatio
	tiers           []string // 可选分辨率档位
	sizes           []pixels // 可选的具体尺寸
	durations       []int    // 可选时长；为空时取 minDuration–maxDuration
	minDuration     int
	maxDuration     int
	defaultDuration int                   // 没传时长时用它
	audioOff        bool                  // generateAudio 默认关闭（Kling 3.0）
	detailRange     [2]int                // generationSettings.detailLevel 的范围；零值表示没有
	transparent     bool                  // 支持透明背景
	refs            map[string]usageLimit // 输入素材按 "媒体:用途" 的数量限制，例如 "image:frame"
	fileBytes       map[string]int64      // 每种媒体的单文件上限
	maxBlobs        int                   // 输入素材总数上限（官网 UI 比 schema 严时取官网）；0 表示不限

	// --- 填法（官网前端）---
	seeds bool // 官网对 schema 里没有 seeds 的版本也注入随机 seed

	resolutionInPayload bool // 分辨率写进 modelSpecificPayload.resolution
	aspectInPayload     bool // 比例写进 modelSpecificPayload.aspect_ratio
	computedAspect      bool // generationSettings.aspectRatio 用由尺寸算出的比例
	aspectInSpecific    bool // 比例写进 modelSpecificPayload.aspectRatio
	explicitAuto        bool // auto 比例时发 generationSettings.aspectRatio="auto"
	excludeSize         bool // 不发 size
	autoWithFrame       bool // 带首帧时比例强制 auto（Seedance 2.5）

	module         string // generationMetadata.module 覆盖
	submodule      string // generationMetadata.submodule 覆盖（编辑、延长版本是 ff-video-editor）
	negativePrompt string // modelSpecificPayload.negative_prompt

	defaultSizes map[string]pixels // 按比例固定的 1K 尺寸（gpt-image-2）
	sizeRange    []tierSide        // 按短边推导尺寸（gpt-image），超出上限时按比例缩进上限

	referenceDuration int    // 带参考图时强制的时长（Veo 3.1）
	referenceAspect   string // 带参考图时强制的比例（Veo 3.1 只支持横屏参考生成）
	noReferences      bool   // 官网不给该版本开参考图（Veo 3.1 Fast）
	// referenceVideos：schema 的 video:source 在该模型上是生成参考（Seedance）。
	referenceVideos bool
	// framesExclusive：首尾帧与任何参考素材（参考图、参考视频）互斥（官网
	// referenceFramesAndReferenceImagesAreMutuallyExclusive，Seedance、Veo 3.1）。
	framesExclusive bool
	requireFrame    bool // 只做图生视频，必须带首帧（schema 没标 minItems）

	// edit / extend：/v1/videos/edits 与 /v1/videos/extensions 实际提交的版本（官网带
	// 编辑视频时按 editReferenceVideoVersions 换版本）；nil 表示不支持该操作。
	edit, extend *modelVersion
}

// usageLimit 是一种输入用途的数量限制；max 为 0 表示不限。
type usageLimit struct{ min, max int }

type pixels struct{ width, height int }

// tierSide 是一个档位的短边；short 为 0 表示取上限内的最大尺寸（4K 档）。
type tierSide struct {
	label string
	short int
}

// gpt-image 的尺寸上限：schema 的宽高上限 3840，总像素上限取自官网 sizeRangeDerivation。
const (
	sizeRangeMaxSide   = 3840
	sizeRangeMaxPixels = 8_294_400
)

func px(values ...int) []pixels {
	out := make([]pixels, 0, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		out = append(out, pixels{values[i], values[i+1]})
	}
	return out
}

// upstream 是路由的 upstream_model（也是价格的键）："modelId/modelVersion"。
func (v *modelVersion) upstream() string { return v.modelID + "/" + v.version }

func (v *modelVersion) has(field string) bool { return slices.Contains(v.fields, field) }

// with 复制一个家族基底并改几项。
func with(base modelVersion, apply func(*modelVersion)) *modelVersion {
	apply(&base)
	return &base
}

// editVersion 由生成版本派生编辑、延长版本：输入只有一个源视频（video:source），
// generationMetadata.submodule 是官网编辑时的 ff-video-editor。
func editVersion(base modelVersion, version string) *modelVersion {
	base.version, base.submodule = version, "ff-video-editor"
	base.refs, base.maxBlobs = map[string]usageLimit{"video:source": {1, 1}}, 0
	base.edit, base.extend = nil, nil
	return &base
}

const mib = 1 << 20

var (
	videoFields = []string{"n", "seeds", "duration", "generateAudio", "resolution", "size", "modelSpecificPayload", "generationSettings", "referenceBlobs"}

	veo31 = modelVersion{modelID: "veo", video: true, fields: videoFields,
		aspects: []string{"16:9", "9:16"}, settingsAspect: true,
		tiers:     []string{"720p", "1080p"},
		sizes:     px(1280, 720, 720, 1280, 1920, 1080, 1080, 1920),
		durations: []int{4, 6, 8}, defaultDuration: 8,
		refs:      map[string]usageLimit{"image:frame": {0, 2}, "image:subject": {0, 3}},
		fileBytes: map[string]int64{"image": 100 * mib}, maxBlobs: 3,
		seeds: true, framesExclusive: true,
	}

	seedance480  = px(1120, 480, 854, 480, 640, 480, 480, 480, 480, 640, 480, 854)
	seedance720  = px(1680, 720, 1280, 720, 960, 720, 720, 720, 720, 960, 720, 1280)
	seedance1080 = px(2520, 1080, 1920, 1080, 1440, 1080, 1080, 1080, 1080, 1440, 1080, 1920)
	seedance20   = modelVersion{modelID: "seedance", video: true, fields: videoFields,
		aspects: []string{"auto", "21:9", "16:9", "4:3", "1:1", "3:4", "9:16"}, settingsAspect: true,
		minDuration: 4, maxDuration: 15,
		refs:      map[string]usageLimit{"image:frame": {0, 2}, "image:style": {0, 9}, "video:source": {0, 3}},
		fileBytes: map[string]int64{"image": 30 * mib, "video": 50 * mib}, maxBlobs: 12,
		seeds: true, computedAspect: true, referenceVideos: true, framesExclusive: true,
	}

	klingSizes = px(1280, 720, 720, 1280, 720, 720, 1920, 1080, 1080, 1920, 1080, 1080)
	kling30    = modelVersion{modelID: "kling", version: "kling_v3", video: true,
		fields:  []string{"duration", "generateAudio", "resolution", "size", "modelSpecificPayload", "generationSettings", "referenceBlobs"},
		aspects: []string{"16:9", "9:16", "1:1"}, settingsAspect: true,
		tiers: []string{"720p", "1080p"}, sizes: klingSizes,
		minDuration: 3, maxDuration: 15, defaultDuration: 5, audioOff: true,
		refs:      map[string]usageLimit{"image:frame": {0, 2}},
		fileBytes: map[string]int64{"image": 10 * mib}, maxBlobs: 5,
		seeds: true, aspectInPayload: true, resolutionInPayload: true,
	}

	// Nano Banana 的 1K/2K/4K 尺寸（schema 枚举）。
	nanoBananaSizes = px(1024, 1024, 1264, 848, 848, 1264, 1200, 896, 896, 1200, 1152, 928, 928, 1152, 768, 1376, 1376, 768, 1584, 672,
		2048, 2048, 2528, 1696, 1696, 2528, 2400, 1792, 1792, 2400, 2304, 1856, 1856, 2304, 1536, 2752, 2752, 1536, 3168, 1344,
		4096, 4096, 5056, 3392, 3392, 5056, 4800, 3584, 3584, 4800, 4608, 3712, 3712, 4608, 3072, 5504, 5504, 3072, 6336, 2688)
	nanoBananaAspects = []string{"1:1", "3:2", "2:3", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"}
	nanoBanana        = modelVersion{modelID: "gemini-flash",
		fields:  []string{"n", "seeds", "groundSearch", "resolution", "size", "modelSpecificPayload", "generationSettings", "referenceBlobs"},
		aspects: nanoBananaAspects, settingsAspect: true,
		tiers: []string{"1K", "2K", "4K"}, sizes: nanoBananaSizes,
		refs:      map[string]usageLimit{"image:general": {0, 14}},
		fileBytes: map[string]int64{"image": 20 * mib}, maxBlobs: 14,
		seeds: true,
	}

	gptImage = modelVersion{modelID: "gpt-image",
		fields:  []string{"n", "size", "modelSpecificPayload", "generationSettings", "referenceBlobs"},
		aspects: []string{"auto", "1:1", "21:9", "16:9", "3:2", "4:3", "5:4", "4:5", "3:4", "2:3", "9:16", "9:21"},
		tiers:   []string{"1K", "2K", "4K"},
		refs:    map[string]usageLimit{"image:mask": {0, 1}, "image:source": {0, 16}},
		// 官网只开放 1K/2K；schema 允许到 3840，所以加上 4K 档（上限内最大尺寸）。
		sizeRange: []tierSide{{"1K", 1024}, {"2K", 2048}, {"4K", 0}},
		fileBytes: map[string]int64{"image": 100 * mib}, maxBlobs: 17,
	}
)

// models 是 Adobe 的模型目录。
var models = []*modelVersion{
	with(veo31, func(m *modelVersion) {
		m.slug, m.version = "veo-3.1", "3.1-generate"
		m.referenceDuration, m.referenceAspect = 8, "16:9"
	}),
	with(veo31, func(m *modelVersion) {
		m.slug, m.version = "veo-3.1-fast", "3.1-fast-generate"
		m.noReferences = true
	}),
	with(veo31, func(m *modelVersion) {
		m.slug, m.version = "veo-3.1-lite", "3.1-lite-generate"
		m.refs, m.maxBlobs = map[string]usageLimit{"image:frame": {0, 2}}, 0
	}),

	with(seedance20, func(m *modelVersion) {
		m.slug, m.version = "seedance-2.0", "seedance_2.0"
		m.tiers = []string{"480p", "720p", "1080p"}
		m.sizes = slices.Concat(px(1920, 1080, 1280, 720, 640, 480), seedance480, seedance720, seedance1080)
	}),
	with(seedance20, func(m *modelVersion) {
		m.slug, m.version = "seedance-2.0-fast", "seedance_2.0_fast"
		m.tiers = []string{"480p", "720p"}
		m.sizes = slices.Concat(px(1280, 720, 640, 480), seedance480, seedance720)
	}),
	// 2.5 的 schema 写 30 张图 / 10 个视频 / 共 50 个，官网 UI（phase 1）只开放 9 / 3 / 共 12。
	with(seedance20, func(m *modelVersion) {
		m.slug, m.version = "seedance-2.5", "seedance_2.5"
		m.fields = slices.DeleteFunc(slices.Clone(videoFields), func(f string) bool { return f == "size" })
		m.tiers, m.sizes = []string{"480p", "720p"}, nil
		m.maxDuration = 30
		m.fileBytes = map[string]int64{"image": 30 * mib, "video": 200 * mib}
		m.computedAspect, m.explicitAuto, m.resolutionInPayload, m.autoWithFrame = false, true, true, true
		// 官网给 2.5 加编辑视频时换成 _editing；_extension 官网未开放，按 schema 接。两者比例
		// 只有 auto；编辑的时长跟随源视频，不发 duration。
		m.edit, m.extend = editVersion(*m, "seedance_2.5_editing"), editVersion(*m, "seedance_2.5_extension")
		m.edit.aspects, m.extend.aspects = []string{"auto"}, []string{"auto"}
		m.edit.fields = slices.DeleteFunc(slices.Clone(m.fields), func(f string) bool { return f == "duration" })
		m.edit.audioOff = true // 编辑按官网默认不另生成音轨；延长与生成一样开启
	}),

	with(kling30, func(m *modelVersion) { m.slug = "kling-3.0" }),
	with(kling30, func(m *modelVersion) {
		m.slug, m.version = "kling-3.0-omni", "kling_v3_omni"
		m.refs = map[string]usageLimit{"image:frame": {0, 2}, "image:style": {0, 3}}
		m.maxBlobs, m.computedAspect = 0, true
		// 官网给 Omni 加编辑视频（不带参考图）时换成 v2v_edit：schema 不收时长与
		// generateAudio，多一个 keepAudio。
		m.edit = editVersion(*m, "kling_v3_omni_v2v_edit")
		m.edit.fields = []string{"resolution", "size", "modelSpecificPayload", "referenceBlobs", "keepAudio"}
		m.edit.fileBytes = map[string]int64{"video": 200 * mib}
	}),
	{
		slug: "kling-video-2.5-turbo", modelID: "kling", version: "kling_v2_5_turbo_pro_i2v", video: true,
		fields:  []string{"seeds", "duration", "generateAudio", "size", "modelSpecificPayload", "generationSettings", "referenceBlobs"},
		aspects: []string{"16:9", "9:16", "1:1"}, tiers: []string{"1080p"}, sizes: px(1920, 1080, 1080, 1920, 1080, 1080),
		durations: []int{5, 10}, defaultDuration: 5,
		refs:      map[string]usageLimit{"image:frame": {0, 2}},
		fileBytes: map[string]int64{"image": 10 * mib},
		seeds:     true, aspectInPayload: true, requireFrame: true,
		module: "image2video", negativePrompt: "blur, distort, and low quality",
	},

	with(nanoBanana, func(m *modelVersion) { // Gemini 3
		m.slug, m.version, m.aspectInSpecific = "nano-banana-pro", "nano-banana-2", true
	}),
	with(nanoBanana, func(m *modelVersion) { // Gemini 3.1：多 512 档与 1:8、8:1、1:4、4:1
		m.slug, m.version, m.aspectInSpecific = "nano-banana-2", "nano-banana-3", true
		m.aspects = append(slices.Clone(nanoBananaAspects), "1:8", "8:1", "1:4", "4:1")
		m.tiers = []string{"512", "1K", "2K", "4K"}
		m.sizes = slices.Concat(px(512, 512), nanoBananaSizes, px(600, 448, 512, 512, 384, 688, 688, 384, 448, 600, 784, 336,
			632, 424, 576, 464, 464, 576, 424, 632, 1536, 192, 1024, 256, 256, 1024, 192, 1536))
	}),
	with(nanoBanana, func(m *modelVersion) {
		m.slug, m.version, m.excludeSize = "nano-banana-2.1", "nano-banana-2026-09", true
		m.aspects = append(slices.Clone(nanoBananaAspects), "1:8", "8:1", "1:4", "4:1")
	}),

	with(gptImage, func(m *modelVersion) {
		m.slug, m.version, m.detailRange = "gpt-image-2", "2", [2]int{1, 5}
		m.defaultSizes = map[string]pixels{
			"21:9": {1584, 672}, "16:9": {1376, 768}, "5:4": {1152, 928}, "4:3": {1200, 896}, "3:2": {1264, 848},
			"1:1": {1024, 1024}, "4:5": {928, 1152}, "3:4": {896, 1200}, "2:3": {848, 1264}, "9:16": {768, 1376},
		}
		m.sizeRange = []tierSide{{"2K", 2048}, {"4K", 0}} // 1K 用上面的尺寸表
	}),
	with(gptImage, func(m *modelVersion) {
		m.slug, m.version, m.detailRange, m.transparent = "gpt-image-2.5-flare", "gpt-image-2.5-flare", [2]int{1, 7}, true
	}),
	with(gptImage, func(m *modelVersion) {
		m.slug, m.version, m.detailRange, m.transparent = "gpt-image-2.5-sunburst", "gpt-image-2.5-prism", [2]int{1, 7}, true
	}),
}

var byUpstream = func() map[string]*modelVersion {
	out := make(map[string]*modelVersion, len(models))
	for _, v := range models {
		out[v.upstream()] = v
	}
	return out
}()

func lookup(upstream string) (*modelVersion, bool) {
	v, ok := byUpstream[strings.TrimSpace(upstream)]
	return v, ok
}

// Routes 是 Adobe 的路由目录：视频模型有 video；图像模型有 image，能收输入图的再有
// image_edit。upstream_model（也是价格的键）是 "modelId/modelVersion"。
func Routes() []modeldomain.Route {
	values := make([]modeldomain.Route, 0, len(models)*2)
	add := func(v *modelVersion, capability modeldomain.Capability) {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderAdobe, v.slug)
		values = append(values, modeldomain.Route{PublicID: publicID, Provider: account.ProviderAdobe, UpstreamModel: v.upstream(), Capability: capability, Origin: modeldomain.OriginCatalog, Enabled: true})
	}
	for _, v := range models {
		switch {
		case v.video:
			add(v, modeldomain.CapabilityVideo)
		default:
			if !v.requiresImageInput() {
				add(v, modeldomain.CapabilityImage)
			}
			if usage, _ := v.referenceUsage(); usage != "" {
				add(v, modeldomain.CapabilityImageEdit)
			}
		}
	}
	return values
}
