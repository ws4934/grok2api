package channelhttp

import (
	"reflect"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/gin-gonic/gin"
)

func TestChatResponsesNormalizeIdentically(t *testing.T) {
	chat := `{"model":"m","messages":[{"role":"system","content":"规则"},{"role":"user","content":"问题"},{"role":"assistant","content":"answer"},{"role":"user","content":[{"type":"text","text":"继续"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	responses := `{"model":"m","store":false,"instructions":"规则","input":[{"role":"user","content":[{"type":"input_text","text":"问题"}]},{"role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"role":"user","content":[{"type":"input_text","text":"继续"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
	a, e := decodeChat([]byte(chat), false)
	if e != nil {
		t.Fatal(e)
	}
	b, e := decodeChat([]byte(responses), true)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(a.Request, b.Request) {
		t.Fatalf("semantic mismatch: %+v %+v", a.Request, b.Request)
	}
}

func TestDecodeChatParameterLeniency(t *testing.T) {
	accepted := []string{
		`"temperature":0.2`, `"top_p":1`, `"frequency_penalty":0`, `"presence_penalty":0`, `"seed":7`,
		`"user":"u"`, `"metadata":{"a":"b"}`, `"n":1`, `"logprobs":false`, `"top_logprobs":0`,
		`"parallel_tool_calls":false`, `"include":["reasoning.encrypted_content"]`, `"text":{"format":{"type":"text"},"verbosity":"low"}`,
		`"truncation":"auto"`, `"prompt_cache_key":"k"`, `"service_tier":"auto"`, `"safety_identifier":"s"`,
		`"modalities":["text"]`, `"stop":["x"]`, `"previous_response_id":null`, `"store":false`,
		`"response_format":{"type":"text"}`, `"stream_options":{"include_usage":true}`, `"temperature":null`,
		`"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"auto"`, `"tool_choice":"none"`,
		`"reasoning":{"effort":"high","summary":"auto"}`, `"max_tokens":5`,
	}
	rejected := []string{
		`"n":2`, `"logprobs":true`, `"top_logprobs":2`, `"previous_response_id":"resp_1"`, `"store":true`,
		`"background":true`, `"tool_choice":"required"`, `"tool_choice":{"type":"function","function":{"name":"f"}}`,
		`"tools":[{"type":"web_search"}]`, `"tools":[{"type":"image_generation"}]`, `"text":{"format":{"type":"json_schema"}}`,
		`"response_format":{"type":"json_object"}`, `"modalities":["audio"]`, `"max_tokens":0`, `"logit_bias":{}`,
		`"reasoning_effort":"turbo"`, `"stream_options":{"bogus":true}`,
	}
	for _, responses := range []bool{false, true} {
		body := func(extra string) []byte {
			if responses {
				return []byte(`{"model":"m","input":"hi",` + extra + `}`)
			}
			return []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` + extra + `}`)
		}
		for _, p := range accepted {
			if _, e := decodeChat(body(p), responses); e != nil {
				t.Errorf("responses=%v rejected %s: %v", responses, p, e)
			}
		}
		for _, p := range rejected {
			in, e := decodeChat(body(p), responses)
			if e == nil && in.N == 1 {
				t.Errorf("responses=%v accepted %s", responses, p)
			}
		}
	}
	in, e := decodeChat([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":300,"max_completion_tokens":200,"stream":true,"stream_options":{"include_usage":true}}`), false)
	if e != nil || in.Request.MaxOutputTokens != 200 || !in.Stream || !in.IncludeUsage {
		t.Fatalf("output cap and stream options: %+v %v", in, e)
	}
	in, e = decodeChat([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"modalities":["text","image"],"n":3}`), false)
	if e != nil || !in.Image || in.N != 3 {
		t.Fatalf("image modality: %+v %v", in, e)
	}
}

func TestReasoningEffortMapping(t *testing.T) {
	for body, want := range map[string]string{
		`"reasoning_effort":"low"`:                               "low",
		`"reasoning_effort":"MEDIUM"`:                            "medium",
		`"reasoning":{"effort":"high","summary":"detailed"}`:     "high",
		`"reasoning_effort":"xhigh"`:                             "extended",
		`"thinking_effort":"extended"`:                           "extended",
		`"reasoning_effort":"none"`:                              "",
		`"reasoning_effort":"minimal"`:                           "",
		`"thinking_effort":"low","reasoning_effort":"high"`:      "low",
		`"reasoning_effort":"high","reasoning":{"effort":"low"}`: "high",
		`"reasoning":{"summary":"auto"}`:                         "",
		`"reasoning":null`:                                       "",
	} {
		in, e := decodeChat([]byte(`{"model":"m","input":"hi",`+body+`}`), true)
		if e != nil || in.Request.ReasoningEffort != want {
			t.Errorf("%s: got %q %v", body, in.Request.ReasoningEffort, e)
		}
	}
	for _, body := range []string{`"reasoning_effort":"turbo"`, `"reasoning_effort":1`, `"reasoning":"high"`} {
		if _, e := decodeChat([]byte(`{"model":"m","input":"hi",`+body+`}`), true); e == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestToolHistoryIsFlattened(t *testing.T) {
	chat := `{"model":"m","tools":[{"type":"function","function":{"name":"weather"}}],"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"},
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"low"}},{"type":"text","text":"and here?"}]}]}`
	responses := `{"model":"m","tools":[{"type":"function","name":"weather","parameters":{}}],"input":[
		{"role":"user","content":"weather?"},
		{"type":"reasoning","id":"rs_1","summary":[]},
		{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"sunny"},
		{"type":"message","id":"msg_1","status":"completed","role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png","detail":"low"},{"type":"input_text","text":"and here?"}]}]}`
	want := []domain.Message{
		textMessage("system", toolsUnavailable),
		textMessage("user", "weather?"),
		textMessage("assistant", `Tool call weather (call_1): {"city":"Paris"}`),
		{Role: "user", Content: []domain.ContentPart{{Type: "text", Text: "Tool result (call_1):"}, {Type: "text", Text: "sunny"}}},
		{Role: "user", Content: []domain.ContentPart{{Type: "image_url", ImageURL: "https://example.com/a.png"}, {Type: "text", Text: "and here?"}}},
	}
	for i, body := range []string{chat, responses} {
		in, e := decodeChat([]byte(body), i == 1)
		if e != nil || !reflect.DeepEqual(in.Request.Messages, want) {
			t.Fatalf("case %d: %+v %v", i, in.Request.Messages, e)
		}
	}
	for _, input := range []string{
		`[{"type":"item_reference","id":"x"}]`,
		`[{"role":"user","content":[{"type":"input_file","file_id":"f"}]}]`,
		`[{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png","detail":"ultra"}]}]`,
		`[{"type":"web_search_call","id":"ws"}]`,
	} {
		if _, e := decodeChat([]byte(`{"model":"m","input":`+input+`}`), true); e == nil {
			t.Errorf("accepted %s", input)
		}
	}
}

func TestDecodeMessages(t *testing.T) {
	in, e := decodeMessages([]byte(`{"model":"m","max_tokens":64,"stream":true,"metadata":{"user_id":"u"},"thinking":{"type":"enabled","budget_tokens":1024},
		"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"read","input_schema":{}}],"tool_choice":{"type":"auto"},
		"messages":[
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"image","source":{"type":"url","url":"https://example.com/b.png"}}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"s"},{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"file body"}]}]}]}`))
	want := []domain.Message{
		textMessage("system", toolsUnavailable),
		textMessage("system", "be brief"),
		{Role: "user", Content: []domain.ContentPart{{Type: "text", Text: "look"}, {Type: "image_url", ImageURL: "data:image/png;base64,AAAA"}, {Type: "image_url", ImageURL: "https://example.com/b.png"}}},
		textMessage("assistant", `Tool call read (toolu_1): {"path":"a"}`),
		{Role: "user", Content: []domain.ContentPart{{Type: "text", Text: "Tool result (toolu_1):"}, {Type: "text", Text: "file body"}}},
	}
	if e != nil || !in.Stream || in.Request.MaxOutputTokens != 64 || !reflect.DeepEqual(in.Request.Messages, want) {
		t.Fatalf("decoded: %+v %v", in, e)
	}
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"any"}}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"mcp_servers":[]}`,
		`{"model":"m","max_tokens":8,"messages":[{"role":"system","content":"hi"}]}`,
	} {
		if _, e := decodeMessages([]byte(body)); e == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestErrorTypeFollowsStatus(t *testing.T) {
	for status, want := range map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "invalid_request_error", 429: "rate_limit_error", 502: "server_error", 503: "server_error"} {
		if got := openAIError(status, "x", "m")["type"]; got != want {
			t.Errorf("%d: %v", status, got)
		}
	}
	if got := openAIError(429, "insufficient_quota", "m")["type"]; got != "insufficient_quota" {
		t.Errorf("quota type: %v", got)
	}
	if got := anthropicErrorBody(503, "m")["error"].(gin.H)["type"]; got != "api_error" {
		t.Errorf("anthropic 5xx: %v", got)
	}
}
