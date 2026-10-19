package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
	"golang.org/x/crypto/sha3"
)

func TestSentinelProofCancellationAndBudget(t *testing.T) {
	p := browserprofile.Default()
	zone := openAITimezoneSnapshot{at: time.Now(), location: time.UTC}
	if token := buildProofToken(context.Background(), "seed", "ff", p, nil, "build", zone); !strings.HasPrefix(token, "gAAAAAB") {
		t.Fatal("proof failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if token := buildProofToken(ctx, "seed", "00", p, nil, "build", zone); token != "" {
		t.Fatal("canceled proof returned token")
	}
	if token := buildProofToken(context.Background(), "seed", "invalid", p, nil, "build", zone); token != "" {
		t.Fatal("malformed difficulty accepted")
	}
}
// marshalProof is the loop solveProof replaced: it marshals config per attempt.
func marshalProof(ctx context.Context, seed string, target []byte, config []any) string {
	for i := 0; i < 500000; i++ {
		if i%256 == 0 && ctx.Err() != nil {
			return ""
		}
		config[3] = i
		config[9] = i >> 1
		raw, _ := json.Marshal(config)
		encoded := base64.StdEncoding.EncodeToString(raw)
		sum := sha3.Sum512(append([]byte(seed), []byte(encoded)...))
		if len(target) <= len(sum) && bytes.Compare(sum[:len(target)], target) <= 0 {
			return "gAAAAAB" + encoded
		}
	}
	return ""
}

func TestSentinelProofMatchesMarshalLoop(t *testing.T) {
	p := browserprofile.Default()
	// The same shape as buildProofToken's config; the script and build carry
	// characters json.Marshal escapes.
	config := func() []any {
		return []any{
			"1920x1080", "Mon Oct 19 2026 10:00:00 GMT+0000 (Coordinated Universal Time)", p.JSHeapSizeLimit, 0, p.UserAgent,
			"https://cdn.example/sdk.js?a=1&b=<2>", "prod-\"é\"", p.NavigatorLanguage, p.NavigatorLanguagesValue(), 0.5, "webdriver-false", "location", "navigator",
			0, "8f9a6c1e-2b3d-4e5f-8a7b-9c0d1e2f3a4b", "", p.HardwareConcurrency, int64(1760868000000), 0, 0, 0, 0, 0, 0, 0, 0,
		}
	}
	for _, difficulty := range []string{"ff", "0fffff", "03ffff", "00ffff", "003fff", "0003ff"} {
		target, err := hexBytes(difficulty)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 6 {
			seed := "0." + strconv.Itoa(7919*(i+1))
			want := marshalProof(context.Background(), seed, target, config())
			if got := solveProof(context.Background(), seed, target, config()); want == "" || got != want {
				t.Fatalf("difficulty %s seed %s: got %q, want %q", difficulty, seed, got, want)
			}
		}
	}
	if solveProof(context.Background(), "seed", make([]byte, 65), config()) != "" {
		t.Fatal("target longer than the sum was met")
	}
}

func TestSentinelUnknownAndGlobalStepBudget(t *testing.T) {
	encode := func(ops []any) string {
		raw, _ := json.Marshal(ops)
		return base64.StdEncoding.EncodeToString([]byte(xorSentinelString(string(raw), "key")))
	}
	if _, e := solveSentinelTurnstileToken(context.Background(), encode([]any{[]any{999}}), "key"); e == nil {
		t.Fatal("unknown opcode accepted")
	}
	vm := &sentinelVM{ctx: context.Background(), values: map[any]any{}}
	vm.install()
	vm.steps = 19999
	vm.values[float64(9)] = []any{[]any{float64(2), "x", "one"}}
	if e := vm.runQueue(20000); e != nil {
		t.Fatal(e)
	}
	vm.values[float64(9)] = []any{[]any{float64(2), "x", "two"}}
	if e := vm.runQueue(20000); e == nil {
		t.Fatal("nested execution reset global budget")
	}
}
func TestChatStreamPreservesHistoryAndRejectsPrematureEOF(t *testing.T) {
	stream := `data: {"message":{"author":{"role":"assistant"},"channel":"analysis","content":{"parts":["hidden"]}}}

data: {"message":{"author":{"role":"assistant"},"channel":"final","content":{"parts":["你好"]}}}

data: {"p":"/message/content/parts/0","o":"append","v":" world"}

data: [DONE]
`
	var text string
	e := scanOpenAIChat(strings.NewReader(stream), nil, func(ev OpenAIChatEvent) error { text += ev.Text; return nil })
	if e != nil || text != "你好 world" {
		t.Fatalf("stream: %q %v", text, e)
	}
	if e := scanOpenAIChat(strings.NewReader(strings.ReplaceAll(stream, "data: [DONE]", "")), nil, func(OpenAIChatEvent) error { return nil }); e == nil {
		t.Fatal("truncated stream treated as complete")
	}
}

func TestOnlyExplicitReasoningSummaryIsEmitted(t *testing.T) {
	var state openAIChatState
	event, e := state.event(map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}, "channel": "analysis", "content": map[string]any{"content_type": "thoughts", "thoughts": []any{map[string]any{"summary": "可见摘要", "content": "raw analysis must not be emitted"}}}}})
	if e != nil || event.Thinking != "可见摘要" || event.Text != "" {
		t.Fatalf("summary extraction: %+v %v", event, e)
	}
}

func chatSnapshot(id, text, status string, metadata string) string {
	return `data: {"message":{"id":"` + id + `","author":{"role":"assistant"},"channel":"final","status":"` + status + `","content":{"content_type":"text","parts":[` + strconv.Quote(text) + `]},"metadata":` + metadata + `}}` + "\n\n"
}

func TestChatStreamUsesMergedAnswerNotFragments(t *testing.T) {
	fragment := `{"is_message_fragment":true,"merged_message_id":"merged"}`
	merged := `{"is_merged_message":true,"source_message_ids":["a","b"]}`
	stream := chatSnapshot("a", "第一段。", "in_progress", fragment) +
		chatSnapshot("merged", "第一段。", "in_progress", merged) +
		chatSnapshot("a", "第一段。", "finished_successfully", fragment) +
		`data: {"message":{"id":"w","author":{"role":"assistant"},"channel":"final","status":"finished_successfully","content":{"content_type":"text","parts":["<box border radius=\"xl\">"]},"metadata":{"is_visually_hidden_from_conversation":true}}}` + "\n\n" +
		`data: {"message":{"id":"c","author":{"role":"assistant"},"channel":"final","status":"finished_successfully","content":{"content_type":"code","text":"widget()"},"metadata":{}}}` + "\n\n" +
		chatSnapshot("b", "你", "in_progress", fragment) +
		chatSnapshot("merged", "第一段。你", "in_progress", merged) +
		chatSnapshot("b", "你可以查看天气网。", "finished_successfully", fragment) +
		chatSnapshot("merged", "第一段。你可以查看天气网。", "finished_successfully", merged) +
		"data: [DONE]\n\n"
	var text strings.Builder
	if err := scanOpenAIChat(strings.NewReader(stream), nil, func(event OpenAIChatEvent) error {
		text.WriteString(event.Text)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if text.String() != "第一段。你可以查看天气网。" {
		t.Fatalf("text = %q", text.String())
	}
}

func TestChatStreamFallsBackToFragmentsWithoutMergedAnswer(t *testing.T) {
	fragment := `{"is_message_fragment":true}`
	stream := chatSnapshot("a", "前半", "finished_successfully", fragment) + chatSnapshot("b", "后半", "finished_successfully", fragment) + "data: [DONE]\n\n"
	var text strings.Builder
	if err := scanOpenAIChat(strings.NewReader(stream), nil, func(event OpenAIChatEvent) error {
		text.WriteString(event.Text)
		return nil
	}); err != nil || text.String() != "前半后半" {
		t.Fatalf("text = %q, err = %v", text.String(), err)
	}
}

func TestChatStreamSkipsEchoedInputMessages(t *testing.T) {
	echo := `data: {"message":{"id":"in-1","author":{"role":"assistant"},"status":"finished_successfully","content":{"content_type":"text","parts":["<tool_calls>…</tool_calls>"]},"metadata":{"message_source":null}}}` + "\n\n"
	stream := echo + chatSnapshot("a", "Here", "in_progress", "{}") + chatSnapshot("a", "Here it is.", "finished_successfully", "{}") + "data: [DONE]\n\n"
	var text strings.Builder
	if err := scanOpenAIChat(strings.NewReader(stream), map[string]bool{"in-1": true}, func(event OpenAIChatEvent) error {
		if event.Done && text.Len() == 0 {
			t.Fatal("the echoed input ended the answer")
		}
		text.WriteString(event.Text)
		return nil
	}); err != nil || text.String() != "Here it is." {
		t.Fatalf("text = %q, err = %v", text.String(), err)
	}
}
