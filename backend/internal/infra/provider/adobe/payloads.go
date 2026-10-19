package adobe

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

type pixels struct {
	width  int
	height int
}

// imageSizes 是 Firefly 各 1K/2K/4K 档位的 aspectRatio→像素 表（官网 bundle 模块
// 790139/915772/769775）。gpt-image 与 nano-banana 用下面各自的表。
var imageSizes = map[string]map[string]pixels{
	"1K": {"1:1": {1024, 1024}, "4:3": {1200, 896}, "3:4": {896, 1200}, "16:9": {1376, 768}, "9:16": {768, 1376}, "21:9": {1584, 672}, "3:2": {1264, 848}, "5:4": {1152, 928}, "4:5": {928, 1152}, "2:3": {848, 1264}},
	"2K": {"1:1": {2048, 2048}, "4:3": {2400, 1792}, "3:4": {1792, 2400}, "16:9": {2752, 1536}, "9:16": {1536, 2752}, "21:9": {3168, 1344}, "3:2": {2528, 1696}, "5:4": {2304, 1856}, "4:5": {1856, 2304}, "2:3": {1696, 2528}},
	"4K": {"1:1": {4096, 4096}, "1:8": {1536, 12288}, "1:4": {2048, 8192}, "16:9": {5504, 3072}, "9:16": {3072, 5504}, "4:1": {8192, 2048}, "4:3": {4800, 3584}, "3:4": {3584, 4800}, "8:1": {12288, 1536}, "5:4": {4608, 3712}, "21:9": {6336, 2688}, "3:2": {5056, 3392}, "4:5": {3712, 4608}, "2:3": {3392, 5056}},
}

var nanoBananaSizes = map[string]pixels{"1K": {1024, 1024}, "2K": {2048, 2048}, "4K": {4096, 4096}}

// nanoBanana25Sizes 是 Gemini 2.5 (Nano Banana) 唯一的 1K 档：官网按比例发实际尺寸。
var nanoBanana25Sizes = map[string]pixels{
	"1:1": {1024, 1024}, "4:3": {1184, 864}, "3:4": {864, 1184}, "16:9": {1344, 768}, "9:16": {768, 1344},
	"21:9": {1536, 672}, "3:2": {1248, 832}, "2:3": {832, 1248}, "5:4": {1152, 896}, "4:5": {896, 1152},
}

var gptImageSizes = map[string]map[string]pixels{
	"1K": {"1:1": {1024, 1024}, "5:4": {1120, 896}, "9:16": {720, 1280}, "21:9": {1456, 624}, "16:9": {1280, 720}, "4:3": {1152, 864}, "3:2": {1248, 832}, "4:5": {896, 1120}, "3:4": {864, 1152}, "2:3": {832, 1248}},
	"2K": {"1:1": {2048, 2048}, "5:4": {2240, 1792}, "9:16": {1440, 2560}, "21:9": {3024, 1296}, "16:9": {2560, 1440}, "4:3": {2304, 1728}, "3:2": {2496, 1664}, "4:5": {1792, 2240}, "3:4": {1728, 2304}, "2:3": {1664, 2496}},
	"4K": {"1:1": {2880, 2880}, "5:4": {3200, 2560}, "9:16": {2160, 3840}, "21:9": {3696, 1584}, "16:9": {3840, 2160}, "4:3": {3264, 2448}, "3:2": {3504, 2336}, "4:5": {2560, 3200}, "3:4": {2448, 3264}, "2:3": {2336, 3504}},
}

var videoSizes = map[string]map[string]pixels{
	"480p":  {"21:9": {1120, 480}, "16:9": {854, 480}, "4:3": {640, 480}, "1:1": {480, 480}, "3:4": {480, 640}, "9:16": {480, 854}},
	"720p":  {"21:9": {1680, 720}, "16:9": {1280, 720}, "4:3": {960, 720}, "1:1": {720, 720}, "3:4": {720, 960}, "9:16": {720, 1280}},
	"1080p": {"21:9": {2520, 1080}, "16:9": {1920, 1080}, "4:3": {1440, 1080}, "1:1": {1080, 1080}, "3:4": {1080, 1440}, "9:16": {1080, 1920}},
}

func imageSizePixels(ratio, resolution string) pixels {
	table := imageSizes[resolution]
	if table == nil {
		table = imageSizes["2K"]
	}
	if p, ok := table[ratio]; ok {
		return p
	}
	return table["16:9"]
}

func videoSizePixels(ratio, resolution string) pixels {
	table := videoSizes[strings.ToLower(resolution)]
	if table == nil {
		table = videoSizes["720p"]
	}
	if p, ok := table[ratio]; ok {
		return p
	}
	return table["16:9"]
}

func randomSeed() int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return 0
	}
	return n.Int64()
}

func seeds(count int) []int64 {
	if count < 1 {
		count = 1
	}
	values := make([]int64, count)
	for i := range values {
		values[i] = randomSeed()
	}
	return values
}

// imagePayloadInput 保存解析后的图像请求，供构建 payload 使用。
type imagePayloadInput struct {
	model         imageModel
	prompt        string
	aspectRatio   string
	resolution    string
	quality       string
	sourceImageID []string
	maskID        string
	count         int
	outputFormat  string
	background    string
}

// buildImagePayloads 返回按顺序尝试的 Firefly 提交体。gpt-image 编辑路径提供
// referenceBlobs/referenceImages 两种回退形态，因为上游不同版本都接受过。
func buildImagePayloads(in imagePayloadInput) []map[string]any {
	count := in.count
	if count < 1 {
		count = 1
	}
	seedValues := seeds(count)
	if in.model.upstreamID == "gpt-image" {
		px := gptImageSizes[in.resolution]
		if px == nil {
			px = gptImageSizes["2K"]
		}
		p, ok := px[in.aspectRatio]
		if !ok {
			p = px["16:9"]
		}
		base := map[string]any{
			"modelId": in.model.upstreamID, "modelVersion": in.model.upstreamVer,
			"n": count, "prompt": in.prompt, "seeds": seedValues,
			"output":               map[string]any{"storeInputs": true},
			"referenceBlobs":       []any{},
			"generationMetadata":   map[string]any{"module": "text2image", "submodule": "ff-image-generate"},
			"modelSpecificPayload": map[string]any{"size": fmt.Sprintf("%dx%d", p.width, p.height)},
			"outputResolution":     in.resolution,
			"generationSettings":   map[string]any{"detailLevel": gptDetailLevel(in.quality)},
			"size":                 map[string]any{"width": p.width, "height": p.height},
		}
		if len(in.sourceImageID) == 0 && in.maskID == "" {
			return []map[string]any{applyImageOptions(base, in)}
		}
		// 空数组而非 nil：参考项目的 referenceBlobs 始终是数组，不能序列化成 null。
		maskRef := []any{}
		if in.maskID != "" {
			maskRef = []any{map[string]any{"id": in.maskID, "usage": "mask"}}
		}
		subjectBlobs := make([]any, 0, len(in.sourceImageID)+len(maskRef))
		for _, id := range in.sourceImageID {
			subjectBlobs = append(subjectBlobs, map[string]any{"id": id, "usage": "subject"})
		}
		subjectBlobs = append(subjectBlobs, maskRef...)
		refImagesID := make([]any, 0, len(in.sourceImageID))
		refImagesBlob := make([]any, 0, len(in.sourceImageID))
		for _, id := range in.sourceImageID {
			refImagesID = append(refImagesID, map[string]any{"id": id})
			refImagesBlob = append(refImagesBlob, map[string]any{"localBlobRef": id})
		}
		edit := func(refBlobs, refImages []any) map[string]any {
			m := cloneMap(base)
			m["generationMetadata"] = map[string]any{"module": "image2image", "submodule": "ff-image-generate"}
			m["referenceBlobs"] = refBlobs
			if refImages != nil {
				m["referenceImages"] = refImages
			}
			return applyImageOptions(m, in)
		}
		return []map[string]any{
			edit(subjectBlobs, nil),
			edit(maskRef, refImagesID),
			edit(maskRef, refImagesBlob),
		}
	}

	var size any
	if p, ok := in.model.ratioSizes[in.aspectRatio]; ok {
		size = map[string]any{"width": p.width, "height": p.height}
	} else if in.model.upstreamID == "gemini-flash" {
		p, ok := nanoBananaSizes[in.resolution]
		if !ok {
			p = nanoBananaSizes["2K"]
		}
		size = map[string]any{"width": p.width, "height": p.height}
	} else {
		p := imageSizePixels(in.aspectRatio, in.resolution)
		size = map[string]any{"width": p.width, "height": p.height}
	}
	modelSpecific := map[string]any{"parameters": map[string]any{"addWatermark": false}}
	if in.aspectRatio != "" && in.aspectRatio != "auto" {
		modelSpecific["aspectRatio"] = in.aspectRatio
	}
	refBlobs := make([]any, 0, len(in.sourceImageID)+1)
	for _, id := range in.sourceImageID {
		refBlobs = append(refBlobs, map[string]any{"id": id, "usage": "general"})
	}
	if in.maskID != "" {
		refBlobs = append(refBlobs, map[string]any{"id": in.maskID, "usage": "mask"})
	}
	base := map[string]any{
		"modelId": in.model.upstreamID, "modelVersion": in.model.upstreamVer,
		"n": count, "prompt": in.prompt, "size": size, "seeds": seedValues,
		"output":               map[string]any{"storeInputs": true},
		"generationMetadata":   map[string]any{"module": "text2image", "submodule": "ff-image-generate"},
		"modelSpecificPayload": modelSpecific,
		"referenceBlobs":       refBlobs,
	}
	if in.model.groundSearch {
		base["groundSearch"] = false
	}
	if len(in.sourceImageID) > 0 || in.maskID != "" {
		base["generationMetadata"] = map[string]any{"module": "image2image", "submodule": "ff-image-generate"}
	}
	return []map[string]any{applyImageOptions(base, in)}
}

// applyImageOptions 把 OpenAI 风格的输出选项合并进 payload。
func applyImageOptions(payload map[string]any, in imagePayloadInput) map[string]any {
	format := strings.ToLower(strings.TrimSpace(in.outputFormat))
	switch format {
	case "png", "jpeg", "webp":
		out, _ := payload["output"].(map[string]any)
		if out == nil {
			out = map[string]any{}
		}
		out["format"] = format
		payload["output"] = out
	}
	if bg := strings.TrimSpace(in.background); bg != "" {
		ms, _ := payload["modelSpecificPayload"].(map[string]any)
		if ms == nil {
			ms = map[string]any{}
		}
		params, _ := ms["parameters"].(map[string]any)
		if params == nil {
			params = map[string]any{}
		}
		params["background"] = bg
		ms["parameters"] = params
		payload["modelSpecificPayload"] = ms
	}
	return payload
}

// videoPayloadInput 保存解析后的视频请求，供构建 payload 使用。
type videoPayloadInput struct {
	model         videoModel
	prompt        string
	negative      string
	aspectRatio   string
	duration      int
	resolution    string
	sourceImageID []string
	generateAudio bool
}

// buildVideoPayload 按模型引擎构建 Firefly 视频提交体。
func buildVideoPayload(in videoPayloadInput) map[string]any {
	size := videoSizePixels(in.aspectRatio, in.resolution)
	sizeObj := map[string]any{"width": size.width, "height": size.height}
	seed := randomSeed()
	switch in.model.engine {
	case "gemini-omni":
		return map[string]any{
			"modelId": "gemini-omni", "modelVersion": "omni-flash", "n": 1, "seeds": []int64{seed},
			"prompt": in.prompt, "output": map[string]any{"storeInputs": true},
			"referenceBlobs":     imageRefBlobs(in.sourceImageID, 4, "style"),
			"generationMetadata": map[string]any{"module": "aura"},
			"size":               sizeObj, "duration": in.duration,
			"generationSettings": map[string]any{"aspectRatio": in.aspectRatio},
		}
	case "seedance", "seedance-fast":
		ver := "seedance_2.0"
		if in.model.engine == "seedance-fast" {
			ver = "seedance_2.0_fast"
		}
		neg := in.negative
		if neg == "" {
			neg = "cartoon, vector art, & bad aesthetics & poor aesthetic"
		}
		return map[string]any{
			"modelId": "seedance", "modelVersion": ver, "size": sizeObj, "seeds": []int64{seed}, "n": 1,
			"referenceBlobs": imageRefBlobs(in.sourceImageID, 9, "style"),
			"prompt":         in.prompt, "negativePrompt": neg, "duration": in.duration,
			"generateAudio":      in.generateAudio,
			"generationMetadata": map[string]any{"module": "text2video", "submodule": "ff-video-generate"},
			"generationSettings": map[string]any{"aspectRatio": in.aspectRatio},
			"output":             map[string]any{"storeInputs": true},
		}
	case "veo31-standard", "veo31-fast":
		ver := "3.1-generate"
		if in.model.engine == "veo31-fast" {
			ver = "3.1-fast-generate"
		}
		var refs []any
		if in.model.referenceMode == "image" && in.model.engine == "veo31-standard" {
			for _, id := range clip(in.sourceImageID, 3) {
				refs = append(refs, map[string]any{"id": id, "usage": "asset"})
			}
		} else {
			for i, id := range clip(in.sourceImageID, 2) {
				refs = append(refs, map[string]any{"id": id, "usage": "general", "promptReference": i + 1})
			}
		}
		return map[string]any{
			"n": 1, "seeds": []int64{seed}, "modelId": "veo", "modelVersion": ver,
			"output": map[string]any{"storeInputs": true}, "prompt": in.prompt, "size": sizeObj,
			"generateAudio": in.generateAudio, "referenceBlobs": refs,
			"generationMetadata":   map[string]any{"module": "text2video"},
			"modelSpecificPayload": map[string]any{"parameters": map[string]any{"durationSeconds": in.duration, "aspectRatio": in.aspectRatio, "addWaterMark": false}},
		}
	default: // kling-o3 / kling3
		hasRef := len(in.sourceImageID) > 0
		var refs []any
		for i, id := range clip(in.sourceImageID, 2) {
			refs = append(refs, map[string]any{"id": id, "usage": "frame", "order": i + 1})
		}
		// module 按"是否带参考图"决定，与引擎无关（对齐参考项目）。
		module := "text2video"
		if hasRef {
			module = "image2video"
		}
		ver := "kling_v3_standard_t2v"
		// 官网对 Kling 在 modelSpecificPayload 里带 aspect_ratio，Kling 3.0 另带 resolution。
		specific := map[string]any{"aspect_ratio": in.aspectRatio}
		if in.model.engine == "kling-o3" {
			ver = "kling_o3_pro_reference_to_video"
		} else {
			specific["resolution"] = in.resolution
			if hasRef {
				ver = "kling_v3_standard_i2v"
			}
		}
		return map[string]any{
			"n": 1, "seeds": []int64{seed}, "modelId": "kling", "modelVersion": ver,
			"output": map[string]any{"storeInputs": true}, "prompt": in.prompt, "size": sizeObj,
			"generateAudio":        in.generateAudio,
			"generationMetadata":   map[string]any{"module": module},
			"duration":             in.duration,
			"generationSettings":   map[string]any{"aspectRatio": in.aspectRatio},
			"modelSpecificPayload": specific,
			"referenceBlobs":       refs,
		}
	}
}

func imageRefBlobs(images []string, limit int, usage string) []any {
	refs := make([]any, 0)
	for _, id := range clip(images, limit) {
		refs = append(refs, map[string]any{"id": id, "usage": usage})
	}
	return refs
}

func clip(values []string, max int) []string {
	if len(values) > max {
		return values[:max]
	}
	return values
}

// gptDetailLevel 映射 quality→detailLevel（low:1 / medium:3 / high|默认:5）。
func gptDetailLevel(quality string) int {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "low":
		return 1
	case "medium":
		return 3
	default:
		return 5
	}
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
