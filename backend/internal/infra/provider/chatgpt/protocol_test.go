package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
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
	e := scanOpenAIChat(strings.NewReader(stream), func(ev OpenAIChatEvent) error { text += ev.Text; return nil })
	if e != nil || text != "你好 world" {
		t.Fatalf("stream: %q %v", text, e)
	}
	if e := scanOpenAIChat(strings.NewReader(strings.ReplaceAll(stream, "data: [DONE]", "")), func(OpenAIChatEvent) error { return nil }); e == nil {
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
