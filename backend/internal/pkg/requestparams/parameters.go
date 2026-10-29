// Package requestparams keeps bounded, redacted diagnostic parameters separate
// from the request used for inference. It never rewrites the caller's payload.
package requestparams

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/pkg/imagejson"
)

const MaxSnapshotBytes = 16 << 10
const MaxStoredBytes = 64 << 10

type Snapshot struct {
	Values    json.RawMessage `json:"values"`
	Truncated bool            `json:"truncated,omitempty"`
}

type Data struct {
	Client    *Snapshot `json:"client,omitempty"`
	Effective *Snapshot `json:"effective,omitempty"`
	Upstream  *Snapshot `json:"upstream,omitempty"`
}

// Parameters is scoped to one gateway execution (one batch for multi-image
// requests). Freeze detaches it before audit enqueue or durable job creation.
type Parameters struct {
	mu   sync.RWMutex
	data Data
}

type clientKey struct{}
type traceKey struct{}

// WithClient retains the earliest snapshot, before transport canonicalization.
func WithClient(ctx context.Context, value any) context.Context {
	if _, ok := ctx.Value(clientKey{}).(*Snapshot); ok {
		return ctx
	}
	return context.WithValue(ctx, clientKey{}, snapshot(value))
}

func Start(ctx context.Context, client any) (context.Context, *Parameters) {
	ctx = WithClient(ctx, client)
	original, _ := ctx.Value(clientKey{}).(*Snapshot)
	p := &Parameters{data: Data{Client: original}}
	return context.WithValue(ctx, traceKey{}, p), p
}

func (p *Parameters) SetEffective(value any) {
	if p == nil {
		return
	}
	s := snapshot(value)
	p.mu.Lock()
	p.data.Effective = s
	p.data.Upstream = nil
	p.mu.Unlock()
}

// SetEffectiveModel records the gateway's selected upstream model separately
// from the public model in the original request, without modifying inference.
func (p *Parameters) SetEffectiveModel(value any, model string) {
	s := snapshot(value)
	if p == nil || s == nil {
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(s.Values, &fields) != nil || fields == nil {
		return
	}
	fields["model"], _ = json.Marshal(model)
	clean := snapshot(fields)
	if clean == nil {
		return
	}
	clean.Truncated = clean.Truncated || s.Truncated
	p.mu.Lock()
	p.data.Effective = clean
	p.data.Upstream = nil
	p.mu.Unlock()
}

func RecordUpstream(ctx context.Context, value any) {
	p, _ := ctx.Value(traceKey{}).(*Parameters)
	if p == nil {
		return
	}
	s := snapshot(value)
	p.mu.Lock()
	p.data.Upstream = s
	p.mu.Unlock()
}

func (p *Parameters) Data() Data {
	if p == nil {
		return Data{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.data
}

func (p *Parameters) Freeze() *Parameters {
	if p == nil {
		return nil
	}
	d := p.Data()
	clean := func(s *Snapshot) *Snapshot {
		if s == nil {
			return nil
		}
		out := snapshot(s.Values)
		if out != nil {
			out.Truncated = out.Truncated || s.Truncated
		}
		return out
	}
	d = Data{Client: clean(d.Client), Effective: clean(d.Effective), Upstream: clean(d.Upstream)}
	if d.Client == nil && d.Effective == nil && d.Upstream == nil {
		return nil
	}
	return &Parameters{data: d}
}

func (p *Parameters) MarshalJSON() ([]byte, error) { return json.Marshal(p.Data()) }
func (p *Parameters) UnmarshalJSON(raw []byte) error {
	var data Data
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	p.data = data
	return nil
}

func Parse(raw string) *Parameters {
	if raw == "" || raw == "{}" {
		return nil
	}
	var p Parameters
	if json.Unmarshal([]byte(raw), &p) != nil {
		return nil
	}
	return &p
}

func snapshot(value any) *Snapshot {
	var raw []byte
	switch v := value.(type) {
	case []byte:
		raw = v
	case json.RawMessage:
		raw = v
	default:
		var err error
		raw, err = json.Marshal(v)
		if err != nil {
			return nil
		}
	}
	var decoded any
	projection := imagejson.Projection{MaxStringBytes: 4096}
	compact := projection.Append(make([]byte, 0, min(len(raw), 8192)), raw)
	if projection.Invalid() {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(compact))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		return nil
	}
	w := sanitizer{left: MaxSnapshotBytes - 128, nodes: 1024}
	clean, ok := w.value(decoded, "", 0)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		return nil
	}
	return &Snapshot{Values: encoded, Truncated: w.truncated || projection.Truncated}
}

type sanitizer struct {
	left, nodes int
	truncated   bool
}

var inlineMedia = regexp.MustCompile(`(?i)data:[^\s,;]+(?:;[^\s,]+)*;base64,[A-Za-z0-9+/=_-]+`)
var credentialText = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{12,}|xai-[A-Za-z0-9_-]{12,}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b`)
var embeddedURL = regexp.MustCompile(`(?i)https?://[^\s"<>]+`)
var keyNormalizer = strings.NewReplacer("_", "", "-", "", " ", "")

func sensitive(key string) bool {
	k := strings.ToLower(keyNormalizer.Replace(key))
	for _, ending := range []string{"authorization", "cookie", "apikey", "password", "secret", "credential", "credentials", "privatekey", "encryptionkey", "signingkey", "signature"} {
		if strings.HasSuffix(k, ending) {
			return true
		}
	}
	return strings.HasSuffix(k, "token") || k == "jwt" || strings.HasSuffix(k, "sessionid") || k == "secwebsocketprotocol"
}

func (w *sanitizer) scalar(v any) (any, bool) {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > w.left {
		w.truncated = true
		return nil, false
	}
	w.left -= len(raw)
	return v, true
}

// Controls precede potentially large prompts/history so critical dimensions
// remain available even when the diagnostic text budget is exhausted.
func priority(key string) int {
	switch key {
	case "model", "n", "stream", "size", "aspect_ratio", "resolution", "quality", "response_format", "output_format", "background", "partial_images", "duration", "seconds", "voice", "max_tokens", "max_output_tokens", "temperature", "enable_nsfw":
		return 0
	case "prompt", "input", "messages", "tools":
		return 2
	default:
		return 1
	}
}

func (w *sanitizer) value(v any, key string, depth int) (any, bool) {
	w.nodes--
	if w.nodes < 0 || depth > 10 || w.left < 8 {
		w.truncated = true
		return nil, false
	}
	if sensitive(key) {
		return w.scalar("[REDACTED]")
	}
	switch x := v.(type) {
	case map[string]any:
		w.left -= 2
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := priority(keys[i]), priority(keys[j])
			if a != b {
				return a < b
			}
			return keys[i] < keys[j]
		})
		out := make(map[string]any)
		for i, k := range keys {
			encodedKey, _ := json.Marshal(k)
			if i >= 64 || len(encodedKey) > 256 || len(encodedKey)+12 > w.left {
				w.truncated = true
				continue
			}
			w.left -= len(encodedKey) + 4
			childKey := k
			if k == "data" && (key == "input_audio" || x["type"] == "base64") {
				childKey = "b64_json"
			}
			item, ok := w.value(x[k], childKey, depth+1)
			if ok {
				out[k] = item
			}
		}
		return out, true
	case []any:
		w.left -= 2
		out := make([]any, 0, min(len(x), 16))
		for i, item := range x {
			if i >= 16 {
				w.truncated = true
				break
			}
			w.left--
			clean, ok := w.value(item, key, depth+1)
			if !ok {
				break
			}
			out = append(out, clean)
		}
		return out, true
	case string:
		lowerKey := strings.ToLower(key)
		mediaField := lowerKey == "image" || lowerKey == "images" || lowerKey == "image[]" || lowerKey == "images[]" || lowerKey == "mask" || lowerKey == "image_url" || lowerKey == "audio" || lowerKey == "video" || lowerKey == "file_data"
		if lowerKey == "b64_json" || lowerKey == "base64" || lowerKey == "blob" || lowerKey == "encrypted_content" || ((len(x) > 1024 || mediaField) && x != "" && looksBase64(x)) {
			return w.scalar("[MEDIA OMITTED]")
		}
		x = inlineMedia.ReplaceAllString(x, "[MEDIA OMITTED]")
		x = credentialText.ReplaceAllString(x, "[REDACTED]")
		x = embeddedURL.ReplaceAllStringFunc(x, func(value string) string {
			parsed, err := url.Parse(value)
			if err != nil {
				return "[URL REDACTED]"
			}
			parsed.RawQuery = ""
			parsed.Fragment = ""
			parsed.User = nil
			return parsed.String()
		})
		if utf8.RuneCountInString(x) > 2048 {
			x = string([]rune(x)[:2048])
			w.truncated = true
		}
		encoded, _ := json.Marshal(x)
		if len(encoded) > w.left {
			runes := []rune(x)
			low, high := 0, len(runes)
			for low < high {
				mid := (low + high + 1) / 2
				b, _ := json.Marshal(string(runes[:mid]))
				if len(b) <= w.left {
					low = mid
				} else {
					high = mid - 1
				}
			}
			x = string(runes[:low])
			w.truncated = true
		}
		return w.scalar(x)
	default:
		return w.scalar(v)
	}
}

func looksBase64(s string) bool {
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
