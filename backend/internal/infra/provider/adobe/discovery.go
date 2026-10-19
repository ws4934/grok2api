// Package adobe 把 Adobe Firefly 的图像与视频生成接成一个原生网关 Provider。
// 模型目录与请求体都由官方 discovery 接口的 schema 驱动（与 firefly.adobe.com
// 前端同源）；图像走共享的 AsyncImageAdapter 任务引擎，视频走 VideoAdapter。
package adobe

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

// discoveryURL 是官方模型发现接口，无需鉴权。以 var 声明便于测试覆盖。
var discoveryURL = "https://firefly-3p.ff.adobe.io/v2/models/discovery"

// discoverySnapshot 是 discovery 响应的快照：启动即有完整目录，线上拉取失败时兜底。
// 更新方法：
//
//	curl -sS -X POST https://firefly-3p.ff.adobe.io/v2/models/discovery \
//	  -H 'x-api-key: clio-playground-web' -H 'Content-Type: application/json' \
//	  -d '{"filters":{"resolveSchema":true}}' > discovery.json
//
//go:embed discovery.json
var discoverySnapshot []byte

// usageLimit 是 schema x-capabilities 给一种参考用途的数量限制；max 为 0 表示不限。
type usageLimit struct{ min, max int }

// modelVersion 是目录里一个可调用的上游模型版本。
type modelVersion struct {
	slug     string // 对外模型名，例如 "veo-3.1"
	modelID  string // 上游 modelId，例如 "veo"
	version  string // 上游 modelVersion，例如 "3.1-generate"
	video    bool
	editOnly bool                  // 只做局部重绘/扩图（inpainting/outpainting），没有纯生成路由
	props    map[string]any        // 已归一化的 requestSchema.properties
	refs     map[string]usageLimit // "image:frame" 这类 mediaType:usageType
	patch    patch
}

// upstream 是路由的 upstream_model（也是价格的键）："modelId/modelVersion"。它不随
// Adobe 改显示名而变，所以改名只会改对外名（旧名留作别名），路由、绑定与价格都保留。
func (v *modelVersion) upstream() string { return v.modelID + "/" + v.version }

// catalog 是一次 discovery 解析出的可用模型集合。
type catalog struct {
	versions   []*modelVersion
	byUpstream map[string]*modelVersion
}

func (c *catalog) lookup(upstream string) (*modelVersion, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.byUpstream[strings.TrimSpace(upstream)]
	return v, ok
}

// modelNames 给 displayName 不适合当模型名的版本指定名字；其余用补丁名或 displayName 生成。
var modelNames = map[string]string{
	"gemini-flash/nano-banana-2":       "nano-banana-pro", // Gemini 3 (Nano Banana Pro)
	"gemini-flash/nano-banana-3":       "nano-banana-2",   // Gemini 3.1 (Nano Banana 2)
	"gemini-flash/nano-banana-2026-09": "nano-banana-2.1",
	"gpt-4o-image/default":             "gpt-image-1",
}

// hiddenVersions 是官网模型选择器同样隐藏的版本前缀：Kling 的拆分版本由合并版本
// kling_v3 / kling_v3_omni 统一处理；aleph_2 只做视频编辑。
var hiddenVersions = []string{"kling/kling_v3_standard_", "kling/kling_v3_pro_", "kling/kling_o3_", "runway/aleph_2"}

type discoveryVersion struct {
	OutputModality    []string `json:"outputModality"`
	InputMediaUseCase []string `json:"inputMediaUseCase"`
	Enabled           bool     `json:"enabled"`
	IsDeprecated      bool     `json:"isDeprecated"`
	ModelDisplayName  string   `json:"modelDisplayName"`
	RequestSchema     any      `json:"requestSchema"`
}

// parseDiscovery 解析 discovery 响应并筛出网关能服务的图像/视频生成模型。
func parseDiscovery(data []byte) (*catalog, error) {
	var doc struct {
		Models []struct {
			ModelID       string                      `json:"modelId"`
			ModelVersions map[string]discoveryVersion `json:"modelVersions"`
		} `json:"models"`
		CommonRequestSchema map[string]any `json:"commonRequestSchema"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("adobe discovery: %w", err)
	}
	c := &catalog{byUpstream: map[string]*modelVersion{}}
	slugs := map[string]bool{}
	for _, model := range doc.Models {
		keys := make([]string, 0, len(model.ModelVersions))
		for key := range model.ModelVersions {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			raw := model.ModelVersions[key]
			v := newModelVersion(model.ModelID, key, raw, doc.CommonRequestSchema)
			if v == nil {
				continue
			}
			if slugs[v.slug] {
				continue // 同名重复条目（如 flux/1.1 与 fluxPro/1.1）只保留第一个
			}
			slugs[v.slug] = true
			c.byUpstream[v.upstream()] = v
			c.versions = append(c.versions, v)
		}
	}
	slices.SortFunc(c.versions, func(a, b *modelVersion) int { return strings.Compare(a.slug, b.slug) })
	return c, nil
}

func newModelVersion(modelID, version string, raw discoveryVersion, common map[string]any) *modelVersion {
	key := modelID + "/" + version
	modality := ""
	if len(raw.OutputModality) > 0 {
		modality = raw.OutputModality[0]
	}
	if !raw.Enabled || raw.IsDeprecated || (modality != "image" && modality != "video") || onlyEnhancement(raw.InputMediaUseCase) {
		return nil
	}
	for _, prefix := range hiddenVersions {
		if strings.HasPrefix(key, prefix) {
			return nil
		}
	}
	lower := strings.ToLower(version)
	if strings.Contains(lower, "v2v") || strings.Contains(lower, "editing") || strings.Contains(lower, "extension") {
		return nil // 视频编辑/延长版本：网关只支持生成
	}
	schema, _ := resolveSchema(raw.RequestSchema, common, 0).(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return nil
	}
	v := &modelVersion{modelID: modelID, version: version, video: modality == "video", props: props, patch: patches[key]}
	v.editOnly = len(raw.InputMediaUseCase) > 0 && !slices.ContainsFunc(raw.InputMediaUseCase, func(useCase string) bool {
		return useCase != "inpainting" && useCase != "outpainting"
	})
	v.refs = referenceLimits(props)
	if v.refs["video:source"].min > 0 || v.refs["video:general"].min > 0 {
		return nil // 必须给源视频的版本
	}
	v.slug = modelNames[key]
	if v.slug == "" {
		v.slug = slugify(cmpOr(v.patch.name, raw.ModelDisplayName, version))
	}
	if _, ok := modeldomain.NormalizePublicID(account.ProviderAdobe, v.slug); !ok || v.slug == "" {
		return nil
	}
	return v
}

func onlyEnhancement(useCases []string) bool {
	if len(useCases) == 0 {
		return false
	}
	for _, useCase := range useCases {
		switch useCase {
		case "upscaling", "sharpening", "denoising":
		default:
			return false
		}
	}
	return true
}

// slugify 把显示名变成模型名："Flux 1.1 Pro" → "flux-1.1-pro"。
func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func cmpOr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// resolveSchema 与官网前端的 discovery 归一化一致：$ref 对 commonRequestSchema 解析
// （同级键覆盖目标），单项 anyOf 换成那一项，allOf 展平成一个 object；解析不到的
// $ref 整个丢弃。与前端不同，嵌套 $ref 也会继续解析。
func resolveSchema(node any, common map[string]any, depth int) any {
	if depth > 64 {
		return node
	}
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			target, ok := lookupRef(common, ref)
			if !ok {
				return nil
			}
			merged := make(map[string]any, len(target)+len(v))
			for k, x := range target {
				merged[k] = x
			}
			for k, x := range v {
				if k != "$ref" {
					merged[k] = x
				}
			}
			return resolveSchema(merged, common, depth+1)
		}
		if anyOf, ok := v["anyOf"].([]any); ok && len(anyOf) == 1 {
			return resolveSchema(anyOf[0], common, depth+1)
		}
		if allOf, ok := v["allOf"].([]any); ok {
			props := map[string]any{}
			var required []any
			for _, part := range allOf {
				resolved, _ := resolveSchema(part, common, depth+1).(map[string]any)
				if p, ok := resolved["properties"].(map[string]any); ok {
					for k, x := range p {
						props[k] = x
					}
				}
				if r, ok := resolved["required"].([]any); ok {
					required = append(required, r...)
				}
			}
			return map[string]any{"type": "object", "properties": props, "required": required}
		}
		out := make(map[string]any, len(v))
		for k, x := range v {
			if resolved := resolveSchema(x, common, depth+1); resolved != nil {
				out[k] = resolved
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, x := range v {
			if resolved := resolveSchema(x, common, depth+1); resolved != nil {
				out = append(out, resolved)
			}
		}
		return out
	}
	return node
}

func lookupRef(common map[string]any, ref string) (map[string]any, bool) {
	var current any = common
	for _, part := range strings.Split(ref, "/") {
		if part == "#" || part == "" {
			continue
		}
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current = m[part]
	}
	target, ok := current.(map[string]any)
	return target, ok
}

// referenceLimits 读 referenceBlobs 的 x-capabilities：每种 mediaType:usageType 的数量
// 限制。标了 deprecated 的用途跳过，由 schema 给出的 replacement 用途接替（例如
// gpt-image 的 subject → source、Runway 的 general → frame）。
func referenceLimits(props map[string]any) map[string]usageLimit {
	limits := map[string]usageLimit{}
	blobs, ok := field(props, "referenceBlobs")
	if !ok {
		return limits
	}
	caps, _ := blobs["x-capabilities"].([]any)
	if caps == nil {
		if items, ok := blobs["items"].(map[string]any); ok {
			caps, _ = items["x-capabilities"].([]any)
		}
	}
	for _, c := range caps {
		capability, _ := c.(map[string]any)
		media, _ := capability["mediaType"].(string)
		constraints, _ := capability["usageConstraints"].([]any)
		for _, u := range constraints {
			constraint, _ := u.(map[string]any)
			usage, _ := constraint["usageType"].(string)
			if deprecated, _ := constraint["deprecated"].(bool); media == "" || usage == "" || deprecated {
				continue
			}
			limits[media+":"+usage] = usageLimit{min: intOf(constraint["minItems"]), max: intOf(constraint["maxItems"])}
		}
	}
	return limits
}

// --- schema 读取辅助 ---

// field 返回属性 schema；两项 anyOf（X | null）取非 null 那项，外层键作补充。
func field(props map[string]any, name string) (map[string]any, bool) {
	raw, ok := props[name].(map[string]any)
	if !ok {
		return nil, false
	}
	anyOf, ok := raw["anyOf"].([]any)
	if !ok {
		return raw, true
	}
	var picked map[string]any
	for _, x := range anyOf {
		m, _ := x.(map[string]any)
		if m == nil || m["type"] == "null" {
			continue
		}
		if picked != nil {
			return raw, true
		}
		picked = m
	}
	if picked == nil {
		return raw, true
	}
	merged := make(map[string]any, len(picked)+len(raw))
	for k, x := range raw {
		if k != "anyOf" {
			merged[k] = x
		}
	}
	for k, x := range picked {
		merged[k] = x
	}
	return merged, true
}

// nested 读对象属性下的子属性，例如 generationSettings.aspectRatio。
func nested(props map[string]any, object, name string) (map[string]any, bool) {
	parent, ok := field(props, object)
	if !ok {
		return nil, false
	}
	inner, _ := parent["properties"].(map[string]any)
	return field(inner, name)
}

func stringEnum(node map[string]any) []string {
	values, _ := node["enum"].([]any)
	out := make([]string, 0, len(values))
	for _, v := range values {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func intOf(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// --- 路由与刷新 ---

// routes 把目录映射成网关路由：图像模型能直接生成时登记 image，能接受输入图时
// 登记 image_edit；视频模型登记 video。
func (c *catalog) routes() []modeldomain.Route {
	if c == nil {
		return nil
	}
	values := make([]modeldomain.Route, 0, len(c.versions)*2)
	add := func(v *modelVersion, capability modeldomain.Capability) {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderAdobe, v.slug)
		values = append(values, modeldomain.Route{PublicID: publicID, Provider: account.ProviderAdobe, UpstreamModel: v.upstream(), Capability: capability, Origin: modeldomain.OriginCatalog, Enabled: true})
	}
	for _, v := range c.versions {
		switch {
		case v.video:
			add(v, modeldomain.CapabilityVideo)
		default:
			if !v.editOnly && !v.requiresImageInput() {
				add(v, modeldomain.CapabilityImage)
			}
			if usage, _ := v.referenceUsage(); usage != "" {
				add(v, modeldomain.CapabilityImageEdit)
			}
		}
	}
	return values
}

// RefreshCatalog 拉取线上 discovery 并替换内存目录，返回新路由。结果为空或比当前
// 目录少一半以上时视为异常，保留原目录。
func (a *Adapter) RefreshCatalog(ctx context.Context) ([]modeldomain.Route, error) {
	var data []byte
	err := a.withClient(ctx, account.Credential{Provider: account.ProviderAdobe, EgressIdentity: "adobe_catalog"}, func(ctx context.Context, c *client) error {
		headers := c.browserHeaders()
		headers["x-api-key"] = c.rc.apiKey
		headers["Content-Type"] = "application/json"
		headers["Accept"] = "application/json"
		headers["Origin"] = "https://firefly.adobe.com"
		headers["Referer"] = "https://firefly.adobe.com/"
		resp, raw, err := c.send(ctx, http.MethodPost, discoveryURL, strings.NewReader(`{"filters":{"resolveSchema":true}}`), headers, maxResponseBytes)
		if err != nil {
			return err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return classifyUpstream("discovery", resp.StatusCode, truncate(string(raw), 300))
		}
		data = raw
		return nil
	})
	if err != nil {
		return nil, err
	}
	next, err := parseDiscovery(data)
	if err != nil {
		return nil, err
	}
	// ponytail: 缩水过半即拒绝，防止接口抽风把路由（及其绑定、权限）整批删掉；阈值按需调整。
	if current := a.catalog.Load(); len(next.versions) == 0 || (current != nil && len(next.versions)*2 < len(current.versions)) {
		return nil, fmt.Errorf("adobe discovery 返回 %d 个模型，疑似异常，保留现有目录", len(next.versions))
	}
	a.catalog.Store(next)
	return next.routes(), nil
}

// SnapshotRoutes 是内嵌快照的路由，线上拉取失败时用于只增不删地补齐路由。
func SnapshotRoutes() []modeldomain.Route {
	c, err := parseDiscovery(discoverySnapshot)
	if err != nil {
		return nil
	}
	return c.routes()
}

func mustSnapshot() *catalog {
	c, err := parseDiscovery(discoverySnapshot)
	if err != nil || len(c.versions) == 0 {
		panic(errors.Join(errors.New("adobe: 内嵌 discovery 快照无效"), err))
	}
	return c
}
