package channelhttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	app "github.com/chenyme/grok2api/backend/internal/application/channel"
	keyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// chatScript is what the fixture upstream replays, and what it last received.
type chatScript struct {
	events []domain.ChatEvent
	err    error
	got    domain.ChatRequest
}

// chatModule extends the image fixture with a scripted chat adapter.
type chatModule struct {
	imageModule
	script *chatScript
}

func (chatModule) Descriptor() domain.ChannelDescriptor {
	return domain.ChannelDescriptor{ID: "fixture", Name: "Fixture", Version: 1, Capabilities: domain.Capabilities{Chat: true, Responses: true, ImageGeneration: true, ImageEdit: true}}
}
func (m chatModule) Chat(_ context.Context, _ domain.Credentials, r domain.ChatRequest, emit func(domain.ChatEvent) error) error {
	m.script.got = r
	for _, ev := range m.script.events {
		if e := emit(ev); e != nil {
			return e
		}
	}
	return m.script.err
}
func (chatModule) Classify(err error) domain.Failure {
	var f *domain.Failure
	if errors.As(err, &f) {
		return *f
	}
	return domain.Failure{Kind: domain.FailureNetwork}
}

type chatEnv struct {
	s       *app.Service
	r       *gin.Engine
	script  *chatScript
	account uint64
}

// chatFixture routes "gpt" (chat and responses) and "img" (image) to the
// scripted channel, in front of legacy routes that answer 418.
func chatFixture(t *testing.T) chatEnv {
	t.Helper()
	ctx := context.Background()
	db, e := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "chat.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	if e := db.InitializeSchema(ctx); e != nil {
		t.Fatal(e)
	}
	cipher, e := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if e != nil {
		t.Fatal(e)
	}
	script := &chatScript{}
	registry, e := domain.NewRegistry(chatModule{script: script})
	if e != nil {
		t.Fatal(e)
	}
	repo, keys := relational.NewChannelRepository(db), relational.NewClientKeyRepository(db)
	s := app.NewService(registry, repo, cipher, memory.NewLockStore(), relational.NewModelRepository(db))
	s.SetClientKeys(keys)
	encrypted, _ := cipher.Encrypt("secret")
	key, e := keys.Create(ctx, clientkey.Key{Name: "k", Prefix: "k", SecretHash: strings.Repeat("a", 64), EncryptedSecret: encrypted, Enabled: true, RPMLimit: 60, MaxConcurrent: 4, BillingLimitUSDTicks: 1000000})
	if e != nil {
		t.Fatal(e)
	}
	if e := s.SetExecution(keyapp.NewService(keys, memory.NewRateLimiter(), memory.NewConcurrencyLimiter(), 60, 4, cipher), auditWriter{relational.NewAuditRepository(db)}); e != nil {
		t.Fatal(e)
	}
	objects, e := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if e != nil {
		t.Fatal(e)
	}
	s.SetImages(repo, mediaapp.NewService(relational.NewMediaAssetRepository(db), relational.NewMediaJobRepository(db), objects, nil, mediaapp.Config{PublicBaseURL: "https://example.test", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30}))
	imported, e := s.Import(ctx, "fixture", []byte(`[{"name":"a","credentials":{"version":1,"access_token":"AT"}}]`), false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := s.AccountAction(ctx, "fixture", imported[0].AccountID, "validate"); e != nil {
		t.Fatal(e)
	}
	cfg := domain.DefaultSettings()
	cfg.Enabled, cfg.ImageConversationModel = true, "m"
	if e := s.SaveSettings(ctx, "fixture", cfg); e != nil {
		t.Fatal(e)
	}
	for _, p := range []domain.Price{{Model: "m", Operation: "chat", InputPerMillionTicks: 1, OutputPerMillionTicks: 1}, {Model: "m", Operation: "image", ImageTicks: 25}} {
		if _, e := s.SavePrice(ctx, "fixture", p); e != nil {
			t.Fatal(e)
		}
	}
	for _, m := range []app.ModelInput{{PublicID: "gpt", UpstreamModel: "m", Operation: "chat", Enabled: true}, {PublicID: "gpt", UpstreamModel: "m", Operation: "responses", Enabled: true}, {PublicID: "img", UpstreamModel: "m", Operation: "image", Enabled: true}} {
		if _, e := s.CreateModel(ctx, "fixture", m); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.SetGrant(ctx, "fixture", key.ID, true); e != nil {
		t.Fatal(e)
	}
	h := NewHandler(s)
	r := gin.New()
	v1 := r.Group("/v1", func(c *gin.Context) { c.Set(middleware.ClientKey, key) })
	v1.Use(h.Dispatch())
	legacy := func(c *gin.Context) { c.Status(418) }
	for _, path := range []string{"/chat/completions", "/responses", "/messages"} {
		v1.POST(path, legacy)
	}
	return chatEnv{s: s, r: r, script: script, account: imported[0].AccountID}
}

type parsedSSE struct {
	name string
	data map[string]any
	raw  string
}

func post(t *testing.T, r *gin.Engine, path, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return w.Code, w.Body.String()
}
func parseSSE(body string) []parsedSSE {
	var out []parsedSSE
	for _, frame := range strings.Split(body, "\n\n") {
		var ev parsedSSE
		for _, line := range strings.Split(frame, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev.name = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				ev.raw = v
				_ = json.Unmarshal([]byte(v), &ev.data)
			}
		}
		if ev.raw != "" {
			out = append(out, ev)
		}
	}
	return out
}
func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var v map[string]any
	if e := json.Unmarshal([]byte(body), &v); e != nil {
		t.Fatalf("not JSON: %s", body)
	}
	return v
}

var thinkThenAnswer = []domain.ChatEvent{{Reasoning: "think"}, {Reasoning: "ing"}, {Text: "Hello"}, {Text: " world", Done: true}}

func TestChatCompletionStreamFraming(t *testing.T) {
	env := chatFixture(t)
	env.script.events = thinkThenAnswer
	code, body := post(t, env.r, "/v1/chat/completions", `{"model":"gpt","stream":true,"stream_options":{"include_usage":true},"temperature":0.3,"max_tokens":50,"reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	events := parseSSE(body)
	if code != 200 || len(events) != 7 || events[6].raw != "[DONE]" {
		t.Fatalf("stream: %d %s", code, body)
	}
	if got := env.script.got; got.ReasoningEffort != "extended" || got.MaxOutputTokens != 50 {
		t.Fatalf("upstream request: %+v", got)
	}
	choice := func(i int) map[string]any { return events[i].data["choices"].([]any)[0].(map[string]any) }
	if delta := choice(0)["delta"].(map[string]any); delta["role"] != "assistant" || delta["reasoning_content"] != "think" {
		t.Fatalf("first chunk: %s", events[0].raw)
	}
	for i := 1; i < 5; i++ {
		if _, ok := choice(i)["delta"].(map[string]any)["role"]; ok || events[i].data["usage"] != nil {
			t.Fatalf("chunk %d: %s", i, events[i].raw)
		}
	}
	if choice(4)["finish_reason"] != "stop" {
		t.Fatalf("finish chunk: %s", events[4].raw)
	}
	usage, _ := events[5].data["usage"].(map[string]any)
	if len(events[5].data["choices"].([]any)) != 0 || usage == nil || usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"].(float64) < 1 || usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(0) {
		t.Fatalf("usage chunk: %s", events[5].raw)
	}

	// Without include_usage there is no usage chunk and no usage key.
	_, body = post(t, env.r, "/v1/chat/completions", `{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if strings.Contains(body, "usage") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream without usage: %s", body)
	}

	// An empty reply still opens with the role.
	env.script.events = nil
	_, body = post(t, env.r, "/v1/chat/completions", `{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if events := parseSSE(body); len(events) != 3 || !strings.Contains(events[0].raw, `"role":"assistant"`) {
		t.Fatalf("empty stream: %s", body)
	}
}

func TestChatCompletionNonStream(t *testing.T) {
	env := chatFixture(t)
	env.script.events = thinkThenAnswer
	code, body := post(t, env.r, "/v1/chat/completions", `{"model":"gpt","user":"u","n":1,"messages":[{"role":"user","content":"hi"}]}`)
	v := decode(t, body)
	message := v["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	usage := v["usage"].(map[string]any)
	if code != 200 || message["content"] != "Hello world" || message["reasoning_content"] != "thinking" || usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"].(float64) < 1 {
		t.Fatalf("completion: %d %s", code, body)
	}
	if code, body := post(t, env.r, "/v1/chat/completions", `{"model":"gpt","n":2,"messages":[{"role":"user","content":"hi"}]}`); code != 400 {
		t.Fatalf("n=2: %d %s", code, body)
	}
}

func TestResponsesStreamEventSequence(t *testing.T) {
	env := chatFixture(t)
	env.script.events = thinkThenAnswer
	code, body := post(t, env.r, "/v1/responses", `{"model":"gpt","stream":true,"input":"hi","reasoning":{"effort":"low","summary":"auto"},"tools":[{"type":"function","name":"f","parameters":{}}],"include":["reasoning.encrypted_content"],"store":false}`)
	events := parseSSE(body)
	var names []string
	for i, ev := range events {
		names = append(names, ev.name)
		if ev.data["type"] != ev.name || ev.data["sequence_number"] != float64(i) {
			t.Fatalf("event %d: %s %s", i, ev.name, ev.raw)
		}
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed",
	}
	if code != 200 || !reflect.DeepEqual(names, want) || strings.Contains(body, "[DONE]") {
		t.Fatalf("sequence: %d %v", code, names)
	}
	if events[2].data["output_index"] != float64(0) || events[9].data["output_index"] != float64(1) || events[6].data["text"] != "thinking" || events[13].data["text"] != "Hello world" {
		t.Fatalf("indexes: %s / %s", events[2].raw, events[9].raw)
	}
	final := events[16].data["response"].(map[string]any)
	output := final["output"].([]any)
	usage := final["usage"].(map[string]any)
	if final["status"] != "completed" || final["store"] != false || final["error"] != nil || len(output) != 2 || output[0].(map[string]any)["type"] != "reasoning" ||
		final["reasoning"].(map[string]any)["effort"] != "low" || len(final["tools"].([]any)) != 1 || final["text"].(map[string]any)["format"].(map[string]any)["type"] != "text" ||
		usage["output_tokens_details"].(map[string]any)["reasoning_tokens"].(float64) < 1 || usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(0) {
		t.Fatalf("final response: %s", events[16].raw)
	}
	if env.script.got.Messages[0].Content[0].Text != toolsUnavailable {
		t.Fatalf("tools notice missing: %+v", env.script.got.Messages)
	}
}

func TestResponsesLateReasoningFollowsMessage(t *testing.T) {
	env := chatFixture(t)
	env.script.events = []domain.ChatEvent{{Text: "Hi"}, {Reasoning: "late"}, {Text: "!"}}
	_, body := post(t, env.r, "/v1/responses", `{"model":"gpt","stream":true,"input":"hi"}`)
	events := parseSSE(body)
	var added []string
	for _, ev := range events {
		if ev.name == "response.output_item.added" {
			item := ev.data["item"].(map[string]any)
			added = append(added, item["type"].(string))
			if ev.data["output_index"] != float64(len(added)-1) {
				t.Fatalf("index: %s", ev.raw)
			}
		}
	}
	final := events[len(events)-1].data["response"].(map[string]any)["output"].([]any)
	if !reflect.DeepEqual(added, []string{"message", "reasoning"}) || len(final) != 2 || final[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "Hi!" {
		t.Fatalf("late reasoning: %v %v", added, final)
	}

	// Non-stream replies list the same items.
	_, body = post(t, env.r, "/v1/responses", `{"model":"gpt","input":"hi"}`)
	if v := decode(t, body); len(v["output"].([]any)) != 2 || v["status"] != "completed" {
		t.Fatalf("non-stream: %s", body)
	}
}

func TestResponsesTruncationAndFailure(t *testing.T) {
	env := chatFixture(t)
	env.script.events = []domain.ChatEvent{{Text: strings.Repeat("word ", 50)}}
	_, body := post(t, env.r, "/v1/responses", `{"model":"gpt","stream":true,"input":"hi","max_output_tokens":5}`)
	events := parseSSE(body)
	last := events[len(events)-1]
	if last.name != "response.incomplete" || last.data["response"].(map[string]any)["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("truncated: %s", last.raw)
	}

	// A failure after streaming started ends with response.failed.
	env.script.events, env.script.err = []domain.ChatEvent{{Text: "partial"}}, &domain.Failure{Kind: domain.FailureQuota}
	_, body = post(t, env.r, "/v1/responses", `{"model":"gpt","stream":true,"input":"hi"}`)
	events = parseSSE(body)
	last = events[len(events)-1]
	if last.name != "response.failed" || last.data["response"].(map[string]any)["error"].(map[string]any)["code"] != "rate_limit_exceeded" {
		t.Fatalf("failed: %s", body)
	}
}

func TestChatErrorMapping(t *testing.T) {
	env := chatFixture(t)
	request := `{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	for _, tc := range []struct {
		err          error
		status       int
		code, errTyp string
	}{
		{&domain.Failure{Kind: domain.FailureQuota}, 429, "rate_limit_exceeded", "rate_limit_error"},
		{&domain.Failure{Kind: domain.FailureAuth}, 503, "upstream_unavailable", "server_error"},
		{&domain.Failure{Kind: domain.FailureRequest}, 400, "upstream_rejected_request", "invalid_request_error"},
		{context.DeadlineExceeded, 504, "upstream_timeout", "server_error"},
		{errors.New("boom"), 502, "channel_request_failed", "server_error"},
	} {
		env.script.err = tc.err
		code, body := post(t, env.r, "/v1/chat/completions", request)
		problem, _ := decode(t, body)["error"].(map[string]any)
		if code != tc.status || problem["code"] != tc.code || problem["type"] != tc.errTyp {
			t.Errorf("%v: %d %s", tc.err, code, body)
		}
	}
	env.script.err = nil
	disabled := false
	if _, e := env.s.UpdateAccount(context.Background(), "fixture", env.account, app.AccountPatch{Enabled: &disabled}); e != nil {
		t.Fatal(e)
	}
	if code, body := post(t, env.r, "/v1/chat/completions", request); code != 503 || !strings.Contains(body, "no_available_account") {
		t.Fatalf("no account: %d %s", code, body)
	}
	if code, body := post(t, env.r, "/v1/messages", `{"model":"gpt","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`); code != 503 || decode(t, body)["type"] != "error" {
		t.Fatalf("messages no account: %d %s", code, body)
	}
}

func TestMessagesAPI(t *testing.T) {
	env := chatFixture(t)
	env.script.events = thinkThenAnswer
	code, body := post(t, env.r, "/v1/messages", `{"model":"gpt","max_tokens":64,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`)
	v := decode(t, body)
	content := v["content"].([]any)
	if code != 200 || v["type"] != "message" || v["stop_reason"] != "end_turn" || len(content) != 2 || content[0].(map[string]any)["thinking"] != "thinking" || content[1].(map[string]any)["text"] != "Hello world" || v["usage"].(map[string]any)["output_tokens"].(float64) < 1 {
		t.Fatalf("message: %d %s", code, body)
	}
	if env.script.got.MaxOutputTokens != 64 || env.script.got.Messages[0].Role != "system" {
		t.Fatalf("upstream request: %+v", env.script.got)
	}

	code, body = post(t, env.r, "/v1/messages", `{"model":"gpt","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	events := parseSSE(body)
	var names []string
	for _, ev := range events {
		names = append(names, ev.name)
		if ev.data["type"] != ev.name {
			t.Fatalf("type mismatch: %s", ev.raw)
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if code != 200 || !reflect.DeepEqual(names, want) {
		t.Fatalf("stream: %d %v", code, names)
	}
	start := events[0].data["message"].(map[string]any)
	if start["usage"].(map[string]any)["input_tokens"].(float64) < 1 || events[1].data["content_block"].(map[string]any)["type"] != "thinking" ||
		events[2].data["delta"].(map[string]any)["type"] != "thinking_delta" || events[5].data["index"] != float64(1) ||
		events[6].data["delta"].(map[string]any)["text"] != "Hello" || events[9].data["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("stream events: %s", body)
	}

	env.script.events = []domain.ChatEvent{{Text: strings.Repeat("word ", 50)}}
	if _, body := post(t, env.r, "/v1/messages", `{"model":"gpt","max_tokens":3,"messages":[{"role":"user","content":"hi"}]}`); decode(t, body)["stop_reason"] != "max_tokens" {
		t.Fatalf("max_tokens stop: %s", body)
	}
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"legacy model falls through": {`{"model":"grok","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, 418},
		"max_tokens is required":     {`{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`, 400},
		"forced tool is rejected":    {`{"model":"gpt","max_tokens":8,"tools":[{"name":"t","input_schema":{}}],"tool_choice":{"type":"tool","name":"t"},"messages":[{"role":"user","content":"hi"}]}`, 400},
	} {
		code, body := post(t, env.r, "/v1/messages", tc.body)
		if code != tc.code || (code == 400 && decode(t, body)["error"].(map[string]any)["type"] != "invalid_request_error") {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}

func TestChatCompletionImageGeneration(t *testing.T) {
	env := chatFixture(t)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { env.s.RunImages(ctx); close(done) }()
	t.Cleanup(func() { stop(); <-done })

	code, body := post(t, env.r, "/v1/chat/completions", `{"model":"img","messages":[{"role":"user","content":"draw a cat"}]}`)
	v := decode(t, body)
	content, _ := v["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if code != 200 || !strings.HasPrefix(content, "![image](https://example.test/") || v["usage"].(map[string]any)["prompt_tokens"].(float64) < 1 {
		t.Fatalf("image reply: %d %s", code, body)
	}

	code, body = post(t, env.r, "/v1/chat/completions", `{"model":"img","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"draw a dog"}]}`)
	events := parseSSE(body)
	if code != 200 || len(events) != 5 || !strings.Contains(events[0].raw, `"role":"assistant"`) || !strings.Contains(events[1].raw, "![image](https://example.test/") ||
		!strings.Contains(events[2].raw, `"finish_reason":"stop"`) || events[3].data["usage"] == nil || events[4].raw != "[DONE]" {
		t.Fatalf("image stream: %d %s", code, body)
	}

	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"image modality on a chat model": {`{"model":"gpt","modalities":["text","image"],"messages":[{"role":"user","content":"draw"}]}`, 400},
		"edit without an edit route":     {`{"model":"img","messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + onePixelPNG + `"}}]}]}`, 404},
		"legacy model falls through":     {`{"model":"grok","modalities":["image"],"messages":[{"role":"user","content":"draw"}]}`, 418},
	} {
		if code, body := post(t, env.r, "/v1/chat/completions", tc.body); code != tc.code {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}
