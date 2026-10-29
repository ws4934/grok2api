package requestparams

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestParametersRedactCredentialsMediaAndSignedURLs(t *testing.T) {
	raw := []byte(`{"model":"image","n":100,"seed":999999999999999999,"max_tokens":4096,"api_key":"secret-value","headers":{"Authorization":"bearer-value","X-Api-Key":"nested-value"},"image":"aW1hZ2U=","images":["data:image/png;base64,YWJjZA==","https://user:password@example.test/a.png?signature=hidden#fragment"],"prompt":"see https://example.test/b.png?token=hidden and xai-01234567890123456789","messages":[{"content":[{"type":"input_audio","input_audio":{"data":"c2VjcmV0","format":"wav"}},{"type":"image","source":{"type":"base64","data":"c2VjcmV0"}}]}]}`)
	_, p := Start(context.Background(), raw)
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{"secret-value", "bearer-value", "nested-value", "aW1hZ2U=", "YWJjZA==", "c2VjcmV0", "signature=", "token=hidden", "user:password", "xai-01234567890123456789"} {
		if strings.Contains(text, secret) {
			t.Fatalf("parameter secret or media leaked: %s", text)
		}
	}
	for _, kept := range []string{`"n":100`, `"seed":999999999999999999`, `"max_tokens":4096`, "https://example.test/a.png", "https://example.test/b.png", "[REDACTED]", "[MEDIA OMITTED]"} {
		if !strings.Contains(text, kept) {
			t.Fatalf("lost useful parameter %s: %s", kept, text)
		}
	}
	if !strings.Contains(string(raw), "secret-value") {
		t.Fatal("inference payload was modified")
	}
}

func TestParametersAreBoundedValidAndRetainControls(t *testing.T) {
	long := strings.Repeat("\u4e2d\u6587<\\\"", 10000)
	values := map[string]any{"model": "actual-model", "n": 5, "quality": "medium", "prompt": long, "messages": []any{long, long, long}, "metadata": map[string]any{"long": long}}
	ctx, p := Start(context.Background(), values)
	p.SetEffective(values)
	RecordUpstream(ctx, values)
	for _, s := range []*Snapshot{p.Data().Client, p.Data().Effective, p.Data().Upstream} {
		if !s.Truncated || !json.Valid(s.Values) || !utf8.Valid(s.Values) || len(s.Values) > MaxSnapshotBytes {
			t.Fatalf("unbounded/invalid snapshot: size=%d truncated=%v", len(s.Values), s.Truncated)
		}
		if !strings.Contains(string(s.Values), `"n":5`) || !strings.Contains(string(s.Values), `"quality":"medium"`) {
			t.Fatalf("controls lost: %s", s.Values)
		}
	}
	encoded, _ := json.Marshal(p.Freeze())
	if len(encoded) > MaxStoredBytes {
		t.Fatalf("stored parameters=%d", len(encoded))
	}
}

func TestParametersRedactCredentialKeyVariants(t *testing.T) {
	values := map[string]any{"max_tokens": 4096}
	for _, key := range []string{"auth_token", "private_key", "signing_key", "x-signature", "Sec-WebSocket-Protocol"} {
		values[key] = "do-not-save-" + key
	}
	_, p := Start(context.Background(), values)
	text := string(p.Data().Client.Values)
	if strings.Contains(text, "do-not-save") || !strings.Contains(text, `"max_tokens":4096`) {
		t.Fatalf("incorrect credential redaction: %s", text)
	}
}

func TestOriginalParametersSurviveBatchesAndFreeze(t *testing.T) {
	parent := WithClient(context.Background(), []byte(`{"n":5,"quality":"high"}`))
	ctx, a := Start(parent, map[string]any{"n": 2})
	a.SetEffective(map[string]any{"n": 2, "quality": "medium"})
	RecordUpstream(ctx, map[string]any{"enable_nsfw": false})
	frozen := a.Freeze()
	RecordUpstream(ctx, map[string]any{"enable_nsfw": true})
	_, b := Start(parent, map[string]any{"n": 3})
	b.SetEffective(map[string]any{"n": 3})
	if string(a.Data().Client.Values) != `{"n":5,"quality":"high"}` || string(b.Data().Client.Values) != string(a.Data().Client.Values) || string(frozen.Data().Upstream.Values) != `{"enable_nsfw":false}` || b.Data().Upstream != nil {
		t.Fatalf("snapshots crossed executions: a=%s b=%s frozen=%s", mustJSON(a), mustJSON(b), mustJSON(frozen))
	}
}

func TestParametersConcurrentSnapshots(t *testing.T) {
	ctx, p := Start(context.Background(), map[string]any{"n": 5})
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Go(func() {
			for j := 0; j < 20; j++ {
				p.SetEffective(map[string]any{"n": j})
				RecordUpstream(ctx, map[string]any{"enable_nsfw": true})
				_ = p.Freeze()
				_, _ = json.Marshal(p)
			}
		})
	}
	wait.Wait()
}

func mustJSON(p *Parameters) string { b, _ := json.Marshal(p); return string(b) }

func BenchmarkParameterCaptureImage(b *testing.B) {
	for _, payload := range []struct {
		name string
		body []byte
	}{
		{"text", []byte(`{"model":"image","prompt":"a lighthouse","n":5,"quality":"medium"}`)},
		{"base64-2MiB", []byte(`{"model":"image","prompt":"a lighthouse","n":5,"image":"data:image/png;base64,` + strings.Repeat("A", 2<<20) + `"}`)},
	} {
		b.Run(payload.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, p := Start(context.Background(), payload.body)
				if p == nil {
					b.Fatal("no parameters")
				}
			}
		})
	}
}
