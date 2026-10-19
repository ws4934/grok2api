package adobe

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCatalogMatchesDiscovery 是可选检查：拿官方 discovery 与 testdata/discovery-reviewed.json
// （写 models.go 时核对过的条目）对照，报告 Adobe 后来改了什么。平时跳过；在 backend/ 下运行：
//
//	ADOBE_DISCOVERY_CHECK=1 go test ./internal/infra/provider/adobe -run TestCatalogMatchesDiscovery -count=1
//	ADOBE_DISCOVERY_CHECK=/path/discovery.json go test ./internal/infra/provider/adobe -run TestCatalogMatchesDiscovery -count=1
//
// 值为 1 时在线拉取；其余值是存下来的 discovery 响应文件，可这样保存：
//
//	curl -sS -X POST https://firefly-3p.ff.adobe.io/v2/models/discovery \
//	  -H 'x-api-key: clio-playground-web' -H 'Content-Type: application/json' \
//	  -d '{"filters":{"resolveSchema":true}}' > discovery.json
//
// 报告：目录里的版本消失、停用或弃用；版本条目与 commonRequestSchema 里值变了的 JSON 路径；
// 目录涉及的 modelId 下既不在目录也不在 skipped 的新版本。有发现时：先到 firefly.adobe.com
// 核对官网实际用法，再改 models.go，最后用线上条目刷新 testdata：对应的 versions 条目与
// commonRequestSchema 换成线上内容，有意不收的新版本记进 skipped，再用 jq -S . 统一格式。
func TestCatalogMatchesDiscovery(t *testing.T) {
	src := os.Getenv("ADOBE_DISCOVERY_CHECK")
	if src == "" {
		t.Skip("可选检查：ADOBE_DISCOVERY_CHECK=1 在线拉取，或设为 discovery JSON 文件路径")
	}
	var reviewed struct {
		CommonRequestSchema any            `json:"commonRequestSchema"`
		Versions            map[string]any `json:"versions"`
		Skipped             []string       `json:"skipped"`
	}
	data, err := os.ReadFile("testdata/discovery-reviewed.json")
	if err == nil {
		err = json.Unmarshal(data, &reviewed)
	}
	if err != nil {
		t.Fatalf("读取核对记录失败：%v", err)
	}
	var doc struct {
		Models []struct {
			ModelID       string                    `json:"modelId"`
			ModelVersions map[string]map[string]any `json:"modelVersions"`
		} `json:"models"`
		CommonRequestSchema any `json:"commonRequestSchema"`
	}
	if err := json.Unmarshal(loadDiscovery(t, src), &doc); err != nil {
		t.Fatalf("discovery 不是预期的 JSON：%v", err)
	}

	families := map[string]bool{}
	for _, m := range models {
		families[m.modelID] = true
	}
	live := map[string]map[string]any{} // "modelId/modelVersion" → 版本条目
	for _, m := range doc.Models {
		for _, ver := range slices.Sorted(maps.Keys(m.ModelVersions)) {
			key, v := m.ModelID+"/"+ver, m.ModelVersions[ver]
			live[key] = v
			if families[m.ModelID] && v["enabled"] == true && v["isDeprecated"] != true && byUpstream[key] == nil && !slices.Contains(reviewed.Skipped, key) {
				t.Errorf("新版本 %s（%v）：核对后加进 models.go，或记进 skipped", key, v["modelDisplayName"])
			}
		}
	}

	report := func(name string, old, cur any) {
		var diffs []string
		diffJSON("$", old, cur, &diffs)
		for _, d := range diffs[:min(len(diffs), 20)] {
			t.Errorf("%s 改了 %s", name, d)
		}
		if len(diffs) > 20 {
			t.Errorf("%s 另有 %d 处改动未列出", name, len(diffs)-20)
		}
	}
	for _, m := range models {
		key := m.upstream()
		v, ok := live[key]
		switch {
		case !ok:
			t.Errorf("%s 不在 discovery 里了", key)
		case v["enabled"] != true:
			t.Errorf("%s 已停用（enabled 不是 true）", key)
		case v["isDeprecated"] == true:
			t.Errorf("%s 已弃用（isDeprecated=true）", key)
		case reviewed.Versions[key] == nil:
			t.Errorf("%s 没有核对记录（testdata 缺这个版本）", key)
		default:
			report(key, reviewed.Versions[key], v)
		}
	}
	report("commonRequestSchema", reviewed.CommonRequestSchema, doc.CommonRequestSchema)
}

// loadDiscovery 读 src 指向的 discovery 响应文件；src 为 "1" 时在线拉取。
func loadDiscovery(t *testing.T, src string) []byte {
	if src != "1" {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	req, _ := http.NewRequest(http.MethodPost, "https://firefly-3p.ff.adobe.io/v2/models/discovery",
		strings.NewReader(`{"filters":{"resolveSchema":true}}`))
	req.Header.Set("x-api-key", defaultAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json") // 不带 Accept 会 406（curl 默认带 */*）
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("拉取 discovery 失败：%v", err)
	}
	defer resp.Body.Close()
	const limit = 16 << 20 // 目前约 300 KB
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || resp.StatusCode != http.StatusOK || len(data) > limit {
		t.Fatalf("discovery 响应异常（%s，%d 字节，上限 %d，%v）：%s", resp.Status, len(data), limit, err, brief(string(data)))
	}
	return data
}

// absent 标记对象一侧没有这个键，与 null 区分。
type absent struct{}

// diffJSON 逐层比较两份 JSON（不解析 $ref），把值不同的路径记成 "路径：旧 → 新"；
// 长度不同的数组整个列出。
func diffJSON(path string, old, cur any, out *[]string) {
	switch o := old.(type) {
	case map[string]any:
		if c, ok := cur.(map[string]any); ok {
			keys := append(slices.Collect(maps.Keys(o)), slices.Collect(maps.Keys(c))...)
			slices.Sort(keys)
			for _, k := range slices.Compact(keys) {
				var ov, cv any = absent{}, absent{}
				if v, ok := o[k]; ok {
					ov = v
				}
				if v, ok := c[k]; ok {
					cv = v
				}
				diffJSON(path+"."+k, ov, cv, out)
			}
			return
		}
	case []any:
		if c, ok := cur.([]any); ok && len(o) == len(c) {
			for i := range o {
				diffJSON(fmt.Sprintf("%s[%d]", path, i), o[i], c[i], out)
			}
			return
		}
	}
	if !reflect.DeepEqual(old, cur) {
		*out = append(*out, fmt.Sprintf("%s：%s → %s", path, brief(old), brief(cur)))
	}
}

// brief 把值写成 JSON，超过 80 个字符的截断。
func brief(v any) string {
	if _, ok := v.(absent); ok {
		return "（无）"
	}
	b, _ := json.Marshal(v)
	if r := []rune(string(b)); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return string(b)
}
