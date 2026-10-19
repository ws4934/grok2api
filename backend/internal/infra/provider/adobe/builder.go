package adobe

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// genInput 是一次生成的参数，比例/分辨率/时长已对齐到模型支持的值，参考图已上传。
type genInput struct {
	prompt      string
	count       int
	aspect      string // 具体比例、"auto" 或空
	resolution  string // 模型的档位写法（"2K"、"1080p"），空表示模型没有档位
	quality     string
	transparent bool
	duration    int
	frames      []string // 首帧、尾帧（视频）
	references  []string // 参考图 / 编辑输入图
	videos      []string // 参考视频
	mask        string
	sizeHint    pixels // 无比例选项、只给尺寸范围的模型直接用客户端尺寸
}

// aspectCodes 是官网判断尺寸比例时的候选（顺序即平局优先级）。
var aspectCodes = []string{"1:1", "8:1", "4:1", "21:9", "16:9", "3:2", "4:3", "5:4", "4:5", "3:4", "2:3", "9:16", "1:4", "1:8", "9:21"}

func isAuto(aspect string) bool { return strings.EqualFold(strings.TrimSpace(aspect), "auto") }

// --- 选项推导（schema + 补丁）---

// sizes 是模型可用的具体尺寸：补丁替换或 schema 枚举，加补丁追加的尺寸。
func (v *modelVersion) sizes() []pixels {
	out := slices.Clone(v.patch.sizes)
	if len(out) == 0 {
		if size, ok := field(v.props, "size"); ok {
			values, _ := size["enum"].([]any)
			for _, value := range values {
				m, _ := value.(map[string]any)
				if p := (pixels{intOf(m["width"]), intOf(m["height"])}); p.width > 0 && p.height > 0 {
					out = append(out, p)
				}
			}
		}
	}
	return append(out, v.patch.extraSizes...)
}

// aspectOptions 是模型接受的比例：generationSettings.aspectRatio 枚举加尺寸表推出的比例。
func (v *modelVersion) aspectOptions() []string {
	var out []string
	add := func(value string) {
		if value != "" && !slices.ContainsFunc(out, func(s string) bool { return strings.EqualFold(s, value) }) {
			out = append(out, value)
		}
	}
	if ar, ok := nested(v.props, "generationSettings", "aspectRatio"); ok {
		for _, value := range stringEnum(ar) {
			add(value)
		}
	}
	if v.patch.supportsAuto {
		add("auto")
	}
	for _, size := range v.sizes() {
		add(aspectCode(size))
	}
	for _, code := range aspectCodes {
		if _, ok := v.patch.defaultSizes[code]; ok {
			add(code)
		}
		if len(v.patch.sizeRange) > 0 {
			if w, h := parsePair(code); math.Max(float64(w)/float64(h), float64(h)/float64(w)) <= 3 {
				add(code)
			}
		}
	}
	return out
}

// resolutionOptions 是模型接受的档位：补丁或 schema 枚举，否则由尺寸推出。
func (v *modelVersion) resolutionOptions() []string {
	if len(v.patch.resolutions) > 0 {
		return v.patch.resolutions
	}
	if res, ok := field(v.props, "resolution"); ok {
		if values := stringEnum(res); len(values) > 0 {
			return values
		}
	}
	var out []string
	for _, side := range v.patch.sizeRange {
		out = append(out, side.label)
	}
	sizes := v.sizes()
	for _, code := range aspectCodes {
		if p, ok := v.patch.defaultSizes[code]; ok {
			sizes = append(sizes, p)
		}
	}
	for _, size := range sizes {
		if tier := tierOf(size); !slices.Contains(out, tier) {
			out = append(out, tier)
		}
	}
	return out
}

func (v *modelVersion) defaultResolution(options []string) string {
	if res, ok := field(v.props, "resolution"); ok {
		if def, _ := res["default"].(string); def != "" && slices.Contains(options, def) {
			return def
		}
	}
	for _, preferred := range []string{"1K", "720p"} {
		if slices.Contains(options, preferred) {
			return preferred
		}
	}
	if len(options) > 0 {
		return options[0]
	}
	return ""
}

func (v *modelVersion) defaultAspect(options []string) string {
	if ar, ok := nested(v.props, "generationSettings", "aspectRatio"); ok {
		if def, _ := ar["default"].(string); def != "" && !isAuto(def) && slices.Contains(options, def) {
			return def
		}
	}
	for _, preferred := range []string{"16:9", "1:1"} {
		if v.video == (preferred == "16:9") && slices.Contains(options, preferred) {
			return preferred
		}
	}
	for _, option := range options {
		if !isAuto(option) {
			return option
		}
	}
	return ""
}

// --- 参考图用途 ---

// referenceUsage 是参考图/编辑输入图的用途：subject 优先（官网同样如此），否则用补丁
// 的语义，再否则用 schema 里的 general / style / asset / source。
func (v *modelVersion) referenceUsage() (string, int) {
	if limit, ok := v.refs["image:subject"]; ok {
		return "subject", limit.max
	}
	if usage := v.patch.styleUsage; usage != "" {
		if limit, ok := v.refs["image:"+usage]; ok {
			return usage, limit.max
		}
	}
	for _, usage := range []string{"general", "style", "asset", "source"} {
		if usage == "general" && v.patch.keyframeGeneral {
			continue
		}
		if limit, ok := v.refs["image:"+usage]; ok {
			return usage, limit.max
		}
	}
	return "", 0
}

// referenceBlobLimit 是参考素材的总数上限：补丁优先，否则 schema 的 referenceBlobs.maxItems。
func (v *modelVersion) referenceBlobLimit() int {
	if v.patch.maxReferenceBlobs > 0 {
		return v.patch.maxReferenceBlobs
	}
	return v.maxBlobs
}

// requiresImageInput 报告模型是否必须带输入图（例如 Flux Fill）。
func (v *modelVersion) requiresImageInput() bool {
	for key, limit := range v.refs {
		if strings.HasPrefix(key, "image:") && key != "image:element" && limit.min > 0 {
			return true
		}
	}
	return false
}

// --- 尺寸与比例 ---

// pickSize 复刻官网按比例与档位选尺寸（模块 260859 ZQ）。auto 比例不选尺寸。
func (v *modelVersion) pickSize(aspect, tier string) (pixels, bool) {
	if aspect == "" || isAuto(aspect) {
		return pixels{}, false
	}
	if p, ok := v.patch.defaultSizes[aspect]; ok && (len(v.patch.sizeRange) == 0 || strings.EqualFold(tierOf(p), tier)) {
		return p, true
	}
	if sides := v.patch.sizeRange; len(sides) > 0 {
		short := sides[0].short
		for _, side := range sides {
			if strings.EqualFold(side.label, tier) {
				short = side.short
			}
		}
		w, h := parsePair(aspect)
		if w == 0 || h == 0 {
			return pixels{}, false
		}
		return rangeSize(float64(w)/float64(h), short), true
	}
	if v.patch.defaultSizes != nil {
		return pixels{}, false
	}
	sizes := v.sizes()
	for _, size := range sizes {
		if strings.EqualFold(tierOf(size), tier) && aspectCode(size) == aspect {
			return size, true
		}
	}
	// 视频档位里没有这个比例（如 Runway 的 4:3 只有 832 短边）：取同比例里最接近档位的
	// 尺寸。图像不这样做：官网在该档位没有尺寸时只发比例。
	if !v.video {
		return pixels{}, false
	}
	best, found := pixels{}, false
	for _, size := range sizes {
		if aspectCode(size) == aspect && (!found || abs(tierLines(size)-tierLinesOf(tier)) < abs(tierLines(best)-tierLinesOf(tier))) {
			best, found = size, true
		}
	}
	return best, found
}

// tierLines / tierLinesOf 把尺寸与档位换成短边像素，用于就近比较。
func tierLines(size pixels) int { return min(size.width, size.height) }

func tierLinesOf(tier string) int {
	switch strings.ToUpper(strings.TrimSpace(tier)) {
	case "512":
		return 512
	case "1K":
		return 1024
	case "2K":
		return 2048
	case "4K":
		return 2160
	}
	return videoLines(tier)
}

func round16(value float64) int { return int(math.Round(value/16)) * 16 }

// rangeSize 按官网规则由短边推导尺寸（16 的倍数）；short 为 0 或结果超出 gpt-image 的
// 上限（边长 3840、总像素 8,294,400）时，取该比例在上限内的最大尺寸。
func rangeSize(ratio float64, short int) pixels {
	if short > 0 {
		size := pixels{short, round16(float64(short) / ratio)}
		if ratio >= 1 {
			size = pixels{round16(float64(short) * ratio), short}
		}
		if max(size.width, size.height) <= sizeRangeMaxSide && size.width*size.height <= sizeRangeMaxPixels {
			return size
		}
	}
	w := math.Min(sizeRangeMaxSide, math.Sqrt(sizeRangeMaxPixels*ratio))
	h := w / ratio
	if h > sizeRangeMaxSide {
		h = sizeRangeMaxSide
		w = h * ratio
	}
	return pixels{int(w) / 16 * 16, int(h) / 16 * 16}
}

// tierOf 按短边给尺寸归档（官网 c(size)）。
func tierOf(size pixels) string {
	short := min(size.width, size.height)
	switch short {
	case 360, 480, 540, 720, 1080:
		return strconv.Itoa(short) + "p"
	case 512:
		return "512"
	}
	switch {
	case short <= 512:
		return "512"
	case short <= 1024:
		return "1K"
	case short <= 2048:
		return "2K"
	default:
		return "4K"
	}
}

// aspectCode 是与尺寸最接近的比例代码。
func aspectCode(size pixels) string { return closestRatio(size.width, size.height, aspectCodes) }

// closestRatio 在 choices 里挑与 w:h 最接近的比例（对数距离，平局取靠前者）。
func closestRatio(w, h int, choices []string) string {
	best, bestDiff := "", math.Inf(1)
	for _, choice := range choices {
		cw, ch := parsePair(choice)
		if cw == 0 || ch == 0 || w <= 0 || h <= 0 {
			continue
		}
		if diff := math.Abs(math.Log(float64(w*ch) / float64(h*cw))); diff < bestDiff {
			best, bestDiff = choice, diff
		}
	}
	return best
}

// parsePair 解析 "1024x768"、"16:9" 这类由两个正整数组成的值；无效时返回 0, 0。
func parsePair(value string) (int, int) {
	value = strings.TrimSpace(value)
	for _, sep := range []string{"x", "X", "*", "×", ":"} {
		if left, right, ok := strings.Cut(value, sep); ok {
			w, errW := strconv.Atoi(strings.TrimSpace(left))
			h, errH := strconv.Atoi(strings.TrimSpace(right))
			if errW != nil || errH != nil || w <= 0 || h <= 0 {
				return 0, 0
			}
			return w, h
		}
	}
	return 0, 0
}

// formatResolution 是官网在 payload 里写分辨率的格式（2160 → "4k"，其余 "<短边>p"）。
func formatResolution(size pixels) string {
	short := min(size.width, size.height)
	if short == 2160 {
		return "4k"
	}
	return strconv.Itoa(short) + "p"
}

// --- 时长 ---

// chooseDuration 把时长对齐到 schema 的枚举或范围；模型不收时长时返回 false。
func (v *modelVersion) chooseDuration(requested int) (int, bool) {
	schema, ok := field(v.props, "duration")
	if !ok {
		return 0, false
	}
	if requested <= 0 {
		requested = v.patch.defaultDuration
	}
	if requested <= 0 {
		requested = intOf(schema["default"])
	}
	values, _ := schema["enum"].([]any)
	if len(values) > 0 {
		best := intOf(values[0])
		for _, value := range values {
			if n := intOf(value); abs(n-requested) < abs(best-requested) {
				best = n
			}
		}
		return best, true
	}
	if lo := intOf(schema["minimum"]); requested < lo {
		requested = lo
	}
	if hi := intOf(schema["maximum"]); hi > 0 && requested > hi {
		requested = hi
	}
	return requested, requested > 0
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// --- 请求体 ---

// detailLevel 把 quality 映射成 generationSettings.detailLevel（官网：low 1 / medium 3 /
// high 5，默认 medium），并收进 schema 范围。
func (v *modelVersion) detailLevel(quality string) (int, bool) {
	schema, ok := nested(v.props, "generationSettings", "detailLevel")
	if !ok {
		return 0, false
	}
	level := 3
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "low":
		level = 1
	case "high":
		level = 5
	}
	if lo := intOf(schema["minimum"]); lo > 0 && level < lo {
		level = lo
	}
	if hi := intOf(schema["maximum"]); hi > 0 && level > hi {
		level = hi
	}
	return level, true
}

func (v *modelVersion) has(name string) bool {
	_, ok := v.props[name]
	return ok
}

// buildBody 按官网前端的规则（模块 461324 eW + 731058 构建器）拼请求体：先按 schema
// 填字段，再按补丁放置比例与分辨率，最后丢掉 schema 之外的键。
func (v *modelVersion) buildBody(in genInput) map[string]any {
	body := map[string]any{"prompt": in.prompt}
	count := max(1, in.count)
	if v.has("n") {
		body["n"] = count
	}
	if v.has("seeds") || v.patch.seeds {
		seedValues := make([]int64, count)
		for i := range seedValues {
			seedValues[i] = randomSeed()
		}
		body["seeds"] = seedValues
	}
	if v.has("groundSearch") {
		body["groundSearch"] = false
	}
	if duration, ok := v.chooseDuration(in.duration); ok {
		body["duration"] = duration
	}
	if audio, ok := field(v.props, "generateAudio"); ok {
		// schema 写了默认值就照办（Kling 为 false）；没写的保持开启，与 OpenAI 风格视频接口一致。
		on, hasDefault := audio["default"].(bool)
		body["generateAudio"] = on || !hasDefault
	}
	if v.has("resolution") && in.resolution != "" {
		body["resolution"] = in.resolution
	}

	auto := isAuto(in.aspect)
	explicitAuto := v.patch.explicitAuto && auto
	omitAuto := v.patch.omitAuto && auto && !explicitAuto
	size, hasSize := v.pickSize(in.aspect, in.resolution)
	if !hasSize && len(v.aspectOptions()) == 0 && in.sizeHint.width > 0 {
		size, hasSize = in.sizeHint, true
	}
	if hasSize {
		body["size"] = map[string]any{"width": size.width, "height": size.height}
	}

	settings, specific := map[string]any{}, map[string]any{}
	_, settingsAspect := nested(v.props, "generationSettings", "aspectRatio")
	switch {
	case explicitAuto:
		settings["aspectRatio"] = "auto"
	case in.aspect != "" && !auto && !v.patch.computedAspect && settingsAspect:
		settings["aspectRatio"] = in.aspect
	}
	if hasSize && !omitAuto {
		if v.patch.resolutionInPayload {
			res := in.resolution
			if res == "" {
				res = formatResolution(size)
			}
			specific["resolution"] = res
		}
		code := in.aspect
		if code == "" || auto {
			code = aspectCode(size)
		}
		if v.patch.aspectInPayload {
			specific["aspect_ratio"] = code
		}
		if v.patch.computedAspect {
			settings["aspectRatio"] = code
		}
	}
	if v.patch.aspectInSpecific && in.aspect != "" && !omitAuto {
		specific["aspectRatio"] = in.aspect
	}
	if omitAuto || v.patch.excludeSize {
		delete(body, "size")
	}
	if v.patch.negativePrompt != "" {
		specific["negative_prompt"] = v.patch.negativePrompt
	}
	if level, ok := v.detailLevel(in.quality); ok {
		settings["detailLevel"] = level
	}
	if in.transparent {
		settings["transparentBackground"] = true
	}
	if len(specific) > 0 {
		body["modelSpecificPayload"] = specific
	}
	if len(settings) > 0 {
		body["generationSettings"] = settings
	}
	if refs := v.referenceBlobs(in); len(refs) > 0 {
		body["referenceBlobs"] = refs
	}

	module, submodule := "text2image", "ff-image-generate"
	if v.video {
		module, submodule = "text2video", "ff-video-generate"
	}
	if v.patch.module != "" {
		module = v.patch.module
	}
	body["generationMetadata"] = map[string]any{"module": module, "submodule": submodule}
	for key := range body {
		if !v.has(key) && key != "prompt" && key != "generationMetadata" && !(key == "seeds" && v.patch.seeds) {
			delete(body, key)
		}
	}
	body["modelId"], body["modelVersion"] = v.modelID, v.version
	body["output"] = map[string]any{"storeInputs": true}
	return body
}

func (v *modelVersion) referenceBlobs(in genInput) []any {
	var refs []any
	for i, id := range in.frames {
		refs = append(refs, map[string]any{"id": id, "usage": "frame", "order": i + 1})
	}
	usage, _ := v.referenceUsage()
	for _, id := range in.references {
		refs = append(refs, map[string]any{"id": id, "usage": usage})
	}
	for _, id := range in.videos {
		refs = append(refs, map[string]any{"id": id, "usage": "source"})
	}
	if in.mask != "" {
		refs = append(refs, map[string]any{"id": in.mask, "usage": "mask"})
	}
	return refs
}

func randomSeed() int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return 0
	}
	return n.Int64()
}

// --- 请求规划：校验并对齐参数 ---

// imagePlan 校验图像请求并选定比例与档位。显式给出但模型不支持的值直接拒绝。
func (v *modelVersion) imagePlan(prompt, size, aspect, resolution, quality, background string, count, inputs int, mask bool) (genInput, error) {
	in := genInput{prompt: prompt, count: max(1, count), quality: quality}
	if nSchema, ok := field(v.props, "n"); ok {
		if hi := intOf(nSchema["maximum"]); hi > 0 && in.count > hi {
			return in, &requestError{msg: fmt.Sprintf("%s 每次最多生成 %d 张", v.slug, hi)}
		}
	} else if in.count > 1 {
		return in, &requestError{msg: v.slug + " 每次只能生成 1 张"}
	}
	if inputs == 0 && (v.editOnly || v.requiresImageInput()) {
		return in, &requestError{msg: v.slug + " 需要输入图片"}
	}
	if inputs > 0 {
		usage, limit := v.referenceUsage()
		if usage == "" {
			return in, &requestError{msg: v.slug + " 不支持输入图片"}
		}
		if limit > 0 && inputs > limit {
			return in, &requestError{msg: fmt.Sprintf("%s 最多支持 %d 张输入图片", v.slug, limit)}
		}
	}
	if _, ok := v.refs["image:mask"]; mask && !ok {
		return in, &requestError{msg: v.slug + " 不支持 mask"}
	}
	if limit := v.referenceBlobLimit(); limit > 0 && inputs+boolCount(mask) > limit {
		return in, &requestError{msg: fmt.Sprintf("%s 最多支持 %d 张输入图（含 mask）", v.slug, limit)}
	}
	if strings.EqualFold(strings.TrimSpace(background), "transparent") {
		if _, ok := nested(v.props, "generationSettings", "transparentBackground"); !ok {
			return in, &requestError{msg: v.slug + " 不支持透明背景"}
		}
		in.transparent = true
	}

	options := v.aspectOptions()
	w, h := parsePair(size)
	in.sizeHint = pixels{w, h}
	switch requested := strings.TrimSpace(aspect); {
	case requested != "" && !isAuto(requested):
		match, ok := findFold(options, requested)
		if !ok {
			return in, &requestError{msg: fmt.Sprintf("%s 不支持 aspect_ratio %s", v.slug, requested)}
		}
		in.aspect = match
	case w > 0 && h > 0 && len(options) > 0:
		in.aspect = closestRatio(w, h, withoutAuto(options))
	case slices.ContainsFunc(options, isAuto):
		in.aspect = "auto"
	default:
		in.aspect = v.defaultAspect(options)
	}

	tiers := v.resolutionOptions()
	switch requested := strings.TrimSpace(resolution); {
	case requested != "":
		match, ok := findFold(tiers, requested)
		if !ok {
			return in, &requestError{msg: fmt.Sprintf("%s 不支持 resolution %s（可选：%s）", v.slug, requested, strings.Join(tiers, "、"))}
		}
		in.resolution = match
	case w > 0 && h > 0:
		if match, ok := findFold(tiers, tierOf(pixels{w, h})); ok {
			in.resolution = match
		} else {
			in.resolution = v.defaultResolution(tiers)
		}
	default:
		in.resolution = v.defaultResolution(tiers)
	}
	return in, nil
}

// videoPlan 选定视频的比例、分辨率与时长：网关会给未填的参数填默认值，所以这里
// 就近对齐而不是拒绝；参考图的支持与数量已在 ValidateVideoRequest 校验。
func (v *modelVersion) videoPlan(prompt, aspect, resolution string, duration int, hasFrame, hasRefs bool) genInput {
	in := genInput{prompt: prompt, count: 1, duration: duration}
	if hasRefs && v.patch.referenceDuration > 0 {
		in.duration = v.patch.referenceDuration
	}
	switch {
	case hasRefs && v.patch.referenceAspect != "":
		aspect = v.patch.referenceAspect
	case hasFrame && v.patch.autoWithFrame:
		aspect = "auto"
	}
	options := withoutAuto(v.aspectOptions())
	if w, h := parsePair(aspect); w > 0 && h > 0 && len(options) > 0 {
		in.aspect = closestRatio(w, h, options)
	} else if isAuto(aspect) && slices.ContainsFunc(v.aspectOptions(), isAuto) {
		in.aspect = "auto"
	} else {
		in.aspect = v.defaultAspect(options)
	}
	tiers := v.resolutionOptions()
	if match, ok := findFold(tiers, resolution); ok {
		in.resolution = match
	} else if want := videoLines(resolution); want > 0 && len(tiers) > 0 {
		best := tiers[0]
		for _, tier := range tiers {
			if abs(videoLines(tier)-want) < abs(videoLines(best)-want) {
				best = tier
			}
		}
		in.resolution = best
	} else {
		in.resolution = v.defaultResolution(tiers)
	}
	return in
}

// videoLines 把 "720p"、"4k" 这类档位换成行数，便于就近对齐。
func videoLines(tier string) int {
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "4k" {
		return 2160
	}
	n, _ := strconv.Atoi(strings.TrimSuffix(tier, "p"))
	return n
}

func findFold(options []string, value string) (string, bool) {
	value = strings.TrimSpace(value)
	for _, option := range options {
		if strings.EqualFold(option, value) {
			return option, true
		}
	}
	return "", false
}

func withoutAuto(options []string) []string {
	return slices.DeleteFunc(slices.Clone(options), isAuto)
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}
