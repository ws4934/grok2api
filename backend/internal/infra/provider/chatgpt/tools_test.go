package chatgpt

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/toolemu"
)

const lookupTool = `[{"type":"function","name":"lookup","description":"Look up","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]`

// callAnswer's argument holds <title>, which the Web UI filter would strip.
const callAnswer = `Sure.<tool_calls><tool_call><tool_name>lookup</tool_name><parameters>{"q":"<title>x</title>"}</parameters></tool_call></tool_calls>`

// answerSnapshots streams answer one rune per full-message snapshot, as the
// legacy conversation stream does, then ends it.
func answerSnapshots(answer, metadata string) string {
	var stream strings.Builder
	runes := []rune(answer)
	for i := 1; i <= len(runes); i++ {
		status := "in_progress"
		if i == len(runes) {
			status = "finished_successfully"
		}
		stream.WriteString(chatSnapshot("answer", string(runes[:i]), status, metadata))
	}
	return stream.String()
}

// respondTo runs one non-streaming request against answer and returns the
// conversation payload and the response body.
func respondTo(t *testing.T, operation, answer, body string) (map[string]any, map[string]any) {
	t.Helper()
	payloads := make(chan map[string]any, 1)
	a, _ := newTestAdapter(t, answerUpstream(t, payloads, answerSnapshots(answer, "{}")+"data: [DONE]\n\n"))
	object := decodeBody(t, forward(t, a, testCredential(t, a, credentials{AccessToken: "AT"}), operation, body, false))
	return <-payloads, object
}

func outputOf(object map[string]any, kind string) []map[string]any {
	var items []map[string]any
	for _, raw := range object["output"].([]any) {
		if item := raw.(map[string]any); item["type"] == kind {
			items = append(items, item)
		}
	}
	return items
}

func outputText(t *testing.T, object map[string]any) string {
	t.Helper()
	messages := outputOf(object, "message")
	if len(messages) != 1 {
		t.Fatalf("messages = %v", object["output"])
	}
	return messages[0]["content"].([]any)[0].(map[string]any)["text"].(string)
}

func payloadParts(payload map[string]any, index int) []any {
	return payload["messages"].([]any)[index].(map[string]any)["content"].(map[string]any)["parts"].([]any)
}

func TestEmulatedFunctionCallResponsesJSON(t *testing.T) {
	payload, object := respondTo(t, "responses", callAnswer, `{"model":"gpt-5","tools":`+lookupTool+`,"input":"find x"}`)
	if prompt := payloadParts(payload, 0)[0].(string); !strings.Contains(prompt, "Tool: lookup") || !strings.Contains(prompt, "<tool_calls>") || strings.Contains(prompt, toolsUnavailable) {
		t.Fatalf("prompt = %q", prompt)
	}
	calls := outputOf(object, "function_call")
	var arguments struct{ Q string }
	if object["status"] != "completed" || outputText(t, object) != "Sure." || len(calls) != 1 || calls[0]["status"] != "completed" || calls[0]["name"] != "lookup" ||
		!strings.HasPrefix(calls[0]["call_id"].(string), "call_") || json.Unmarshal([]byte(calls[0]["arguments"].(string)), &arguments) != nil || arguments.Q != "<title>x</title>" {
		t.Fatalf("object = %v", object)
	}
	if output := object["output"].([]any); output[len(output)-1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("calls must follow the text: %v", output)
	}
	want, _ := countTokens(callAnswer)
	if got := object["usage"].(map[string]any)["output_tokens"].(float64); int64(got) != want {
		t.Fatalf("output tokens = %v, want %d (tool markup included)", got, want)
	}
}

func TestEmulatedFunctionCallStreamNeverLeaksMarkup(t *testing.T) {
	a, _ := newTestAdapter(t, answerUpstream(t, nil, answerSnapshots(callAnswer, "{}")+"data: [DONE]\n\n"))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	stream := events(t, forward(t, a, credential, "responses", `{"model":"gpt-5","stream":true,"tools":`+lookupTool+`,"input":"find x"}`, true).Body)
	var types []string
	var text, callID string
	var completed map[string]any
	for index, event := range stream {
		if event["sequence_number"] != float64(index) {
			t.Fatalf("sequence_number %v at %d", event["sequence_number"], index)
		}
		types = append(types, event["type"].(string))
		switch event["type"] {
		case "response.output_text.delta":
			text += event["delta"].(string)
		case "response.output_item.added":
			if item := event["item"].(map[string]any); item["type"] == "function_call" {
				callID = item["call_id"].(string)
			}
		case "response.completed":
			completed = event["response"].(map[string]any)
		}
	}
	if text != "Sure." {
		t.Fatalf("visible text %q", text)
	}
	tail := []string{
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done",
		"response.completed",
	}
	if got := strings.Join(types[len(types)-len(tail):], ","); got != strings.Join(tail, ",") {
		t.Fatalf("events = %v", types)
	}
	if calls := outputOf(completed, "function_call"); len(calls) != 1 || calls[0]["call_id"] != callID || callID == "" {
		t.Fatalf("completed = %v", completed)
	}
}

func TestEmulatedFunctionCallThroughChatAndMessages(t *testing.T) {
	a, _ := newTestAdapter(t, answerUpstream(t, nil, answerSnapshots(callAnswer, "{}")+"data: [DONE]\n\n"))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	chatTools := `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`
	chatBody := `"tools":` + chatTools + `,"messages":[{"role":"user","content":"find x"}]}`

	chat := decodeBody(t, forward(t, a, credential, "chat", `{"model":"gpt-5",`+chatBody, false))
	choice := chat["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if choice["finish_reason"] != "tool_calls" || message["content"] != "Sure." || len(calls) != 1 || calls[0].(map[string]any)["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("chat = %v", chat)
	}

	name, arguments, finish, content := "", "", "", ""
	for _, chunk := range events(t, forward(t, a, credential, "chat", `{"model":"gpt-5","stream":true,`+chatBody, true).Body) {
		for _, raw := range chunk["choices"].([]any) {
			choice := raw.(map[string]any)
			if reason, ok := choice["finish_reason"].(string); ok {
				finish = reason
			}
			delta := choice["delta"].(map[string]any)
			text, _ := delta["content"].(string)
			content += text
			for _, raw := range toAnyList(delta["tool_calls"]) {
				function := raw.(map[string]any)["function"].(map[string]any)
				if value, _ := function["name"].(string); value != "" {
					name = value
				}
				value, _ := function["arguments"].(string)
				arguments += value
			}
		}
	}
	if finish != "tool_calls" || name != "lookup" || content != "Sure." || !json.Valid([]byte(arguments)) {
		t.Fatalf("chat stream: finish %q name %q content %q arguments %q", finish, name, content, arguments)
	}

	messagesBody := `"max_tokens":1000,"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],"messages":[{"role":"user","content":"find x"}]}`
	reply := decodeBody(t, forward(t, a, credential, "messages", `{"model":"gpt-5",`+messagesBody, false))
	blocks := reply["content"].([]any)
	last := blocks[len(blocks)-1].(map[string]any)
	if reply["stop_reason"] != "tool_use" || last["type"] != "tool_use" || last["name"] != "lookup" || last["input"].(map[string]any)["q"] != "<title>x</title>" {
		t.Fatalf("messages = %v", reply)
	}

	toolUse, stop, partial := false, "", ""
	for _, event := range events(t, forward(t, a, credential, "messages", `{"model":"gpt-5","stream":true,`+messagesBody, true).Body) {
		if block, ok := event["content_block"].(map[string]any); ok && block["type"] == "tool_use" && block["name"] == "lookup" {
			toolUse = true
		}
		delta, _ := event["delta"].(map[string]any)
		switch {
		case delta["type"] == "input_json_delta":
			partial += delta["partial_json"].(string)
		case delta["type"] == "text_delta" && strings.Contains(delta["text"].(string), "tool_"):
			t.Fatalf("markup leaked: %v", event)
		case delta["stop_reason"] != nil:
			stop = delta["stop_reason"].(string)
		}
	}
	if !toolUse || stop != "tool_use" || !json.Valid([]byte(partial)) {
		t.Fatalf("messages stream: tool_use %v stop %q input %q", toolUse, stop, partial)
	}
}

func TestEmulatedToolHistoryRendersCallsAndResults(t *testing.T) {
	payloads := make(chan map[string]any, 2)
	a, _ := newTestAdapter(t, chatUpstream(t, payloads))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	body := `{"model":"gpt-5","tools":` + lookupTool + `,"input":[
		{"role":"user","content":"find a and b"},
		{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"q\":\"a\"}"},
		{"type":"function_call","call_id":"c2","name":"lookup","arguments":"{\"q\":\"b\"}"},
		{"type":"function_call_output","call_id":"c1","output":"found a"},
		{"type":"function_call_output","call_id":"c2","output":"found b"},
		{"type":"reasoning","summary":[]}]}`
	_ = decodeBody(t, forward(t, a, credential, "responses", body, false))
	payload := <-payloads
	messages := payload["messages"].([]any)
	role := func(i int) any { return messages[i].(map[string]any)["author"].(map[string]any)["role"] }
	calls := toolemu.FormatCalls([]toolemu.Call{{Name: "lookup", Arguments: `{"q":"a"}`}, {Name: "lookup", Arguments: `{"q":"b"}`}})
	if len(messages) != 5 || role(2) != "assistant" || payloadParts(payload, 2)[0] != calls ||
		payloadParts(payload, 3)[0] != "Tool result (c1):" || payloadParts(payload, 3)[1] != "found a" || payloadParts(payload, 4)[1] != "found b" {
		t.Fatalf("history = %v", messages)
	}

	// Chat history reaches the same convention through the hub.
	chat := `{"model":"gpt-5","tools":[{"type":"function","function":{"name":"lookup"}}],"messages":[
		{"role":"user","content":"find a"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"a\"}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"found a"}]}`
	_ = decodeBody(t, forward(t, a, credential, "chat", chat, false))
	payload = <-payloads
	if parts := payloadParts(payload, 2); role(0) != "system" || !strings.HasPrefix(parts[0].(string), "<tool_calls>") || payloadParts(payload, 3)[1] != "found a" {
		t.Fatalf("chat history = %v", payload["messages"])
	}
}

// A block left open at the end of the reply, or wrapped in a Markdown fence,
// still becomes a call and leaves no markup as text.
func TestEmulatedCallUnclosedOrFenced(t *testing.T) {
	unclosed := `<tool_calls><tool_call><tool_name>lookup</tool_name><parameters>{"q":"x"}</parameters></tool_call>`
	for _, answer := range []string{unclosed, "```xml\n" + unclosed + "</tool_calls>\n```"} {
		_, object := respondTo(t, "responses", answer, `{"model":"gpt-5","tools":`+lookupTool+`,"input":"find x"}`)
		if calls := outputOf(object, "function_call"); len(calls) != 1 || calls[0]["arguments"] != `{"q":"x"}` || len(outputOf(object, "message")) != 0 {
			t.Fatalf("%q: output %v", answer, object["output"])
		}
	}
}

func TestToolCallsOnlyForDeclaredFunctionsWhenAllowed(t *testing.T) {
	plain := `<tool_calls><tool_call><tool_name>lookup</tool_name><parameters>{"q":"x"}</parameters></tool_call></tool_calls>`
	payload, object := respondTo(t, "responses", plain, `{"model":"gpt-5","tools":`+lookupTool+`,"tool_choice":"none","input":"x"}`)
	if payloadParts(payload, 0)[0] != plainMarkdown+"\n\n"+toolsUnavailable || len(outputOf(object, "function_call")) != 0 || outputText(t, object) != plain {
		t.Fatalf("tool_choice none: %v", object)
	}

	undeclared := strings.ReplaceAll(plain, "lookup", "rm")
	_, object = respondTo(t, "responses", undeclared, `{"model":"gpt-5","tools":`+lookupTool+`,"input":"x"}`)
	if len(outputOf(object, "function_call")) != 0 || outputText(t, object) != undeclared {
		t.Fatalf("undeclared tool: %v", object)
	}

	two := strings.Replace(plain, "</tool_calls>", `<tool_call><tool_name>lookup</tool_name><parameters>{"q":"y"}</parameters></tool_call></tool_calls>`, 1)
	payload, object = respondTo(t, "responses", two, `{"model":"gpt-5","parallel_tool_calls":false,"tools":`+lookupTool+`,"input":"x"}`)
	calls := outputOf(object, "function_call")
	if !strings.Contains(payloadParts(payload, 0)[0].(string), "Call at most one tool per reply.") || len(calls) != 1 || calls[0]["arguments"] != `{"q":"x"}` || len(outputOf(object, "message")) != 0 {
		t.Fatalf("parallel_tool_calls false: %v", object)
	}
}

const searchMessages = `data: {"message":{"id":"q","author":{"role":"assistant"},"recipient":"web.run","channel":"analysis","status":"finished_successfully","content":{"content_type":"code","text":"{\"search_query\":[{\"q\":\"rust release\"}]}"},"metadata":{"search_queries":[{"type":"search","q":"rust release"}]}}}

data: {"message":{"id":"r","author":{"role":"tool","name":"web.run"},"status":"finished_successfully","content":{"content_type":"text","parts":[""]},"metadata":{"search_result_groups":[{"type":"search_result_group","domain":"blog.rust-lang.org","entries":[{"type":"search_result","url":"https://blog.rust-lang.org/r","title":"Rust 1.80"}]}]}}}

data: {"type":"url_moderation","url_moderation_result":{"full_url":"https://bad.example/x","is_safe":false}}

`

const (
	searchAnswer     = "Rust 1.80 is out. citeturn0search0 Badciteturn0search1"
	searchReferences = `{"content_references":[{"matched_text":"citeturn0search0","alt":"([Rust Blog](https://blog.rust-lang.org/r))","type":"grouped_webpages","items":[{"title":"Announcing Rust 1.80","url":"https://blog.rust-lang.org/r"}]},{"matched_text":"citeturn0search1","alt":"([Bad](https://bad.example/x))","items":[{"url":"https://bad.example/x"}]}]}`
	citedText        = "Rust 1.80 is out. ([Rust Blog](https://blog.rust-lang.org/r)) Bad"
)

func searchUpstream(t *testing.T, payloads chan<- map[string]any) *Adapter {
	stream := searchMessages + answerSnapshots(searchAnswer, searchReferences) +
		`data: {"type":"server_ste_metadata","metadata":{"search_tool_call_count":1,"tool_invoked":true}}` + "\n\ndata: [DONE]\n\n"
	a, _ := newTestAdapter(t, answerUpstream(t, payloads, stream))
	return a
}

func TestWebSearchIsForcedAndReported(t *testing.T) {
	payloads := make(chan map[string]any, 1)
	a := searchUpstream(t, payloads)
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	object := decodeBody(t, forward(t, a, credential, "responses", `{"model":"gpt-5","tools":[{"type":"web_search_preview"}],"tool_choice":"required","input":"rust?"}`, false))
	payload := <-payloads
	messages := payload["messages"].([]any)
	hints := messages[len(messages)-1].(map[string]any)["metadata"].(map[string]any)["system_hints"]
	if strings.Join(stringsOf(payload["system_hints"]), ",") != "search" || strings.Join(stringsOf(hints), ",") != "search" || len(messages) != 2 || payloadParts(payload, 0)[0] != plainMarkdown {
		t.Fatalf("payload = %v", payload)
	}
	output := object["output"].([]any)
	search := output[0].(map[string]any)
	action := search["action"].(map[string]any)
	sources := action["sources"].([]any)
	if search["type"] != "web_search_call" || search["status"] != "completed" || action["query"] != "rust release" || len(sources) != 1 || sources[0].(map[string]any)["url"] != "https://blog.rust-lang.org/r" {
		t.Fatalf("web_search_call = %v", search)
	}
	part := outputOf(object, "message")[0]["content"].([]any)[0].(map[string]any)
	annotations := part["annotations"].([]any)
	start := utf8.RuneCountInString("Rust 1.80 is out. ")
	if part["text"] != citedText || len(annotations) != 1 {
		t.Fatalf("part = %v", part)
	}
	if a := annotations[0].(map[string]any); a["type"] != "url_citation" || a["url"] != "https://blog.rust-lang.org/r" || a["start_index"] != float64(start) || a["end_index"] != float64(start+utf8.RuneCountInString("([Rust Blog](https://blog.rust-lang.org/r))")) {
		t.Fatalf("annotation = %v", a)
	}

	var types []string
	text := ""
	for _, event := range events(t, forward(t, a, credential, "responses", `{"model":"gpt-5","stream":true,"tools":[{"type":"web_search"}],"input":"rust?"}`, true).Body) {
		types = append(types, event["type"].(string))
		if event["type"] == "response.output_text.delta" {
			text += event["delta"].(string)
		}
	}
	joined := strings.Join(types, ",")
	if text != citedText || !strings.Contains(joined, "response.web_search_call.completed,response.output_item.done,response.output_item.added,response.content_part.added") || !strings.Contains(joined, "response.output_text.annotation.added,response.output_text.done") {
		t.Fatalf("stream text %q events %v", text, types)
	}
	if payload := <-payloads; len(stringsOf(payload["system_hints"])) != 0 {
		t.Fatalf("optional search was forced: %v", payload["system_hints"])
	}
}

func TestWebSearchThroughChatAndMessages(t *testing.T) {
	a := searchUpstream(t, nil)
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	chat := decodeBody(t, forward(t, a, credential, "chat", `{"model":"gpt-5","web_search_options":{},"messages":[{"role":"user","content":"rust?"}]}`, false))
	message := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if annotations, _ := message["annotations"].([]any); message["content"] != citedText || len(annotations) != 1 || annotations[0].(map[string]any)["url_citation"].(map[string]any)["url"] != "https://blog.rust-lang.org/r" {
		t.Fatalf("chat = %v", message)
	}

	reply := decodeBody(t, forward(t, a, credential, "messages", `{"model":"gpt-5","max_tokens":1000,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"rust?"}]}`, false))
	blocks := reply["content"].([]any)
	use, result, text := blocks[0].(map[string]any), blocks[1].(map[string]any), blocks[2].(map[string]any)
	hits, _ := result["content"].([]any)
	if use["type"] != "server_tool_use" || use["input"].(map[string]any)["query"] != "rust release" || result["type"] != "web_search_tool_result" ||
		len(hits) != 1 || hits[0].(map[string]any)["url"] != "https://blog.rust-lang.org/r" || text["text"] != citedText || reply["stop_reason"] != "end_turn" {
		t.Fatalf("messages = %v", reply)
	}
	if usage := reply["usage"].(map[string]any); usage["server_tool_use"].(map[string]any)["web_search_requests"] != float64(1) {
		t.Fatalf("usage = %v", usage)
	}
}

func stringsOf(value any) []string {
	var out []string
	for _, raw := range toAnyList(value) {
		out = append(out, raw.(string))
	}
	return out
}

// The Web app's current search format: queries in search_model_queries,
// results with ref IDs, and <Cite/> components in the answer.
func TestWebSearchCiteComponentsBecomeLinks(t *testing.T) {
	results := `data: {"message":{"id":"r","author":{"role":"tool","name":"web.run"},"status":"finished_successfully","content":{"content_type":"text","parts":[""]},"metadata":{"search_model_queries":{"type":"search_model_queries","queries":["go release"]},"search_result_groups":[{"type":"search_result_group","domain":"go.dev","entries":[{"type":"search_result","url":"https://go.dev/doc/devel/release","title":"Release History","attribution":"go.dev","ref_id":{"turn_index":7,"ref_type":"search","ref_index":8}}]}]}}}` + "\n\n"
	answer := `Go 1.27.2 is out. <Cite refs={["turn7search8","turn7search9"]}/> See <Cite refs={["turn9search1"]}/>notes.`
	want := "Go 1.27.2 is out. ([go.dev](https://go.dev/doc/devel/release)) See notes."
	a, _ := newTestAdapter(t, answerUpstream(t, nil, results+answerSnapshots(answer, "{}")+"data: [DONE]\n\n"))
	credential := testCredential(t, a, credentials{AccessToken: "AT"})
	object := decodeBody(t, forward(t, a, credential, "responses", `{"model":"gpt-5","tools":[{"type":"web_search"}],"input":"go?"}`, false))
	search := outputOf(object, "web_search_call")
	if len(search) != 1 || search[0]["action"].(map[string]any)["query"] != "go release" {
		t.Fatalf("web_search_call = %v", search)
	}
	part := outputOf(object, "message")[0]["content"].([]any)[0].(map[string]any)
	annotations := part["annotations"].([]any)
	if part["text"] != want || len(annotations) != 1 || annotations[0].(map[string]any)["url"] != "https://go.dev/doc/devel/release" {
		t.Fatalf("part = %v", part)
	}
}
