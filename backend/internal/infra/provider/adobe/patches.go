package adobe

// patch 是官网前端对单个模型版本的补丁（firefly.adobe.com 的按家族配置，2026-10-19
// 抄录）。schema 说明能发哪些字段，补丁说明怎么填：比例与分辨率放哪里、参考图用途
// 语义、尺寸表。没有补丁的新模型直接按 schema 构建。
type patch struct {
	name string // 显示名，覆盖 discovery 的 modelDisplayName
	// seeds：官网对 schema 里没有 seeds 的版本也会注入随机 seed。
	seeds bool
	// keyframeGeneral：schema 的 general 用途在该模型上表示关键帧，不能当参考图。
	keyframeGeneral bool
	// styleUsage：参考图的用途（"style" / "asset"），优先于 schema 的 general。
	styleUsage string

	resolutionInPayload bool // 分辨率写进 modelSpecificPayload.resolution
	aspectInPayload     bool // 比例写进 modelSpecificPayload.aspect_ratio
	computedAspect      bool // generationSettings.aspectRatio 用由尺寸算出的比例
	aspectInSpecific    bool // 比例写进 modelSpecificPayload.aspectRatio
	supportsAuto        bool // 允许 auto 比例
	omitAuto            bool // auto 比例时不发尺寸与比例
	explicitAuto        bool // auto 比例时发 generationSettings.aspectRatio="auto"
	excludeSize         bool
	autoWithFrame       bool // 带首帧时比例强制 auto（Seedance 2.5）

	module         string // generationMetadata.module 覆盖
	negativePrompt string // modelSpecificPayload.negative_prompt

	sizes        []pixels // 替换 schema 的尺寸枚举
	extraSizes   []pixels // 追加到 schema 的尺寸枚举
	defaultSizes map[string]pixels
	sizeRange    []tierSide // 按短边推导尺寸（gpt-image-2.5），受 maxSide / maxPixels 限制
	resolutions  []string   // 替换 schema 的分辨率枚举

	defaultDuration   int
	referenceDuration int    // 带参考图时强制的时长（Veo 3.1）
	referenceAspect   string // 带参考图时强制的比例（Veo 3.1 只支持横屏参考生成）
	noReferences      bool   // 官网不给该版本开参考图（Veo 3.1 Fast）
	requireFrame      bool   // 只做图生视频，必须带首帧（schema 没标 minItems）
}

type pixels struct{ width, height int }

type tierSide struct {
	label string
	short int
}

// sizeRange 推导出的尺寸上限（官网 gpt-image-2.5 的 sizeRangeDerivation）。
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

var (
	klingSizes  = px(1280, 720, 720, 1280, 720, 720, 1920, 1080, 1080, 1920, 1080, 1080)
	seedance480 = px(1120, 480, 854, 480, 640, 480, 480, 480, 480, 640, 480, 854)
	seedance720 = px(1680, 720, 1280, 720, 960, 720, 720, 720, 720, 960, 720, 1280)
	lumaSizes   = px(1280, 720, 720, 1280, 720, 720, 960, 720, 720, 960, 1680, 720, 720, 1680,
		1920, 1080, 1080, 1920, 1080, 1080, 1440, 1080, 1080, 1440, 2520, 1080, 1080, 2520,
		3840, 2160, 2160, 3840, 2160, 2160, 2880, 2160, 2160, 2880, 5040, 2160, 2160, 5040)
	gptImage2Sizes = map[string]pixels{
		"21:9": {1584, 672}, "16:9": {1376, 768}, "5:4": {1152, 928}, "4:3": {1200, 896}, "3:2": {1264, 848},
		"1:1": {1024, 1024}, "4:5": {928, 1152}, "3:4": {896, 1200}, "2:3": {848, 1264}, "9:16": {768, 1376},
	}
	gptImage25 = patch{supportsAuto: true, omitAuto: true, sizeRange: []tierSide{{"1K", 1024}, {"2K", 2048}}}
	lumaRay    = patch{seeds: true, keyframeGeneral: true, resolutionInPayload: true, aspectInPayload: true}
)

func withPatch(base patch, apply func(*patch)) patch {
	apply(&base)
	return base
}

var patches = map[string]patch{
	"flux/1.1":            {seeds: true},
	"flux/fluxPro":        {seeds: true},
	"flux/fluxPro-2":      {seeds: true},
	"flux/fluxUltra":      {seeds: true},
	"flux/fluxKontextPro": {seeds: true},
	"flux/fluxKontextMax": {seeds: true},
	"flux/fluxFillPro":    {seeds: true},
	"fluxPro/1.1":         {seeds: true},
	"fluxUltra/1.1":       {seeds: true},
	"humain/humain_image": {seeds: true},

	"gemini-flash/nano-banana-2": {seeds: true, aspectInSpecific: true, omitAuto: true},
	"gemini-flash/nano-banana-3": {seeds: true, aspectInSpecific: true, omitAuto: true,
		extraSizes: px(600, 448, 512, 512, 384, 688, 688, 384, 448, 600, 784, 336, 632, 424, 576, 464, 464, 576, 424, 632, 1536, 192, 1024, 256, 256, 1024, 192, 1536)},
	"gemini-flash/nano-banana-2026-09": {seeds: true, omitAuto: true, excludeSize: true},

	"gpt-image/1.5":                 {defaultSizes: map[string]pixels{"3:2": {1536, 1024}, "1:1": {1024, 1024}, "2:3": {1024, 1536}}},
	"gpt-image/2":                   {supportsAuto: true, omitAuto: true, defaultSizes: gptImage2Sizes},
	"gpt-image/gpt-image-2.5-flare": gptImage25,
	"gpt-image/gpt-image-2.5-prism": gptImage25,

	"veo/3.1-generate":      {seeds: true, keyframeGeneral: true, styleUsage: "asset", referenceDuration: 8, referenceAspect: "16:9"},
	"veo/3.1-fast-generate": {seeds: true, keyframeGeneral: true, styleUsage: "asset", noReferences: true},
	"veo/3.1-lite-generate": {seeds: true, keyframeGeneral: true},

	"kling/kling_v2_5_turbo_pro_i2v": {seeds: true, keyframeGeneral: true, aspectInPayload: true, module: "image2video", requireFrame: true,
		negativePrompt: "blur, distort, and low quality", sizes: px(1920, 1080, 1080, 1920, 1080, 1080)},
	"kling/kling_v3": {name: "Kling 3.0", seeds: true, keyframeGeneral: true, aspectInPayload: true, resolutionInPayload: true,
		defaultDuration: 5, sizes: klingSizes},
	"kling/kling_v3_omni": {name: "Kling 3.0 Omni", seeds: true, keyframeGeneral: true, aspectInPayload: true, resolutionInPayload: true,
		computedAspect: true, styleUsage: "style", defaultDuration: 5, sizes: klingSizes},

	"seedance/seedance_2.0": {seeds: true, styleUsage: "style", computedAspect: true, resolutions: []string{"480p", "720p", "1080p"},
		extraSizes: append(append(append([]pixels{}, seedance480...), seedance720...), px(2520, 1080, 1920, 1080, 1440, 1080, 1080, 1080, 1080, 1440, 1080, 1920)...)},
	"seedance/seedance_2.0_fast": {seeds: true, styleUsage: "style", computedAspect: true, resolutions: []string{"480p", "720p"},
		extraSizes: append(append([]pixels{}, seedance480...), seedance720...)},
	"seedance/seedance_2.5": {seeds: true, styleUsage: "style", explicitAuto: true, resolutionInPayload: true, autoWithFrame: true},

	"gemini-omni/omni-flash":     {seeds: true, styleUsage: "style", defaultDuration: 8},
	"gemini-omni/omni-flash-1.1": {seeds: true, defaultDuration: 8},

	"luma/2.0-ray":       lumaRay,
	"luma/2.0-ray-flash": lumaRay,
	"luma/3.0-ray":       lumaRay,
	"luma/3.0-ray-hdr":   lumaRay,
	"luma/3.14-ray":      withPatch(lumaRay, func(p *patch) { p.name, p.sizes = "Ray3.14", lumaSizes }),
	"luma/3.14-ray-hdr":  withPatch(lumaRay, func(p *patch) { p.name, p.sizes = "Ray3.14 HDR", lumaSizes }),

	"runway/gen4.5": {name: "Runway Gen-4.5", seeds: true, keyframeGeneral: true},
}
