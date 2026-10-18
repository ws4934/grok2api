package toolemu

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseConfigSupportsChatAndResponsesSchemas(t *testing.T) {
	chatTools := json.RawMessage(`[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]`)
	chatChoice := json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`)
	chat, err := ParseConfig(chatTools, chatChoice)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Functions) != 1 || chat.Functions[0].Name != "get_weather" || chat.ForcedName != "get_weather" || chat.Choice != "required" {
		t.Fatalf("chat tool config = %#v", chat)
	}

	responsesTools := json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"web_search_preview"},{"type":"code_interpreter"}]`)
	responses, err := ParseConfig(responsesTools, json.RawMessage(`"auto"`))
	if err != nil {
		t.Fatal(err)
	}
	if len(responses.Functions) != 1 || responses.Functions[0].Name != "lookup" || len(responses.ResponseTools) != 3 || !responses.HostedWebSearch || len(responses.Unsupported) != 1 || responses.Unsupported[0] != "code_interpreter" {
		t.Fatalf("responses tool config = %#v", responses)
	}
}

func TestParseConfigAllowsRequiredHostedWebSearch(t *testing.T) {
	configuration, err := ParseConfig(
		json.RawMessage(`[{"type":"web_search","max_uses":8}]`),
		json.RawMessage(`"required"`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Choice != "required" || len(configuration.Functions) != 0 || len(configuration.ResponseTools) != 1 {
		t.Fatalf("configuration = %#v", configuration)
	}
	forced, err := ParseConfig(json.RawMessage(`[{"type":"web_search_preview"}]`), json.RawMessage(`{"type":"web_search_preview"}`))
	if err != nil || !forced.ForcedWebSearch || forced.Choice != "required" {
		t.Fatalf("forced = %#v, %v", forced, err)
	}
	if _, err := ParseConfig(json.RawMessage(`[{"type":"function","name":"lookup"}]`), json.RawMessage(`{"type":"web_search"}`)); err == nil {
		t.Fatal("web_search tool_choice accepted without a web_search tool")
	}
}

func TestPromptDoesNotForceClientFunctionWhenHostedSearchIsRequired(t *testing.T) {
	configuration, err := ParseConfig(
		json.RawMessage(`[
			{"type":"web_search"},
			{"type":"function","name":"lookup","description":"Look up local data","parameters":{"type":"object"}}
		]`),
		json.RawMessage(`"required"`),
	)
	if err != nil {
		t.Fatal(err)
	}
	prompt := Prompt(configuration)
	if !strings.Contains(prompt, "Tool: lookup") {
		t.Fatalf("client function definition missing from prompt: %s", prompt)
	}
	if strings.Contains(prompt, "You MUST call at least one available tool") {
		t.Fatalf("hosted-search required incorrectly forced a client function: %s", prompt)
	}
	if !strings.Contains(prompt, "Call a tool when it is clearly needed") {
		t.Fatalf("mixed hosted/client tool guidance is not optional: %s", prompt)
	}
	configuration.Choice = "none"
	if Prompt(configuration) != "" {
		t.Fatal("tool_choice none still declares tools")
	}
}

func TestFormatCallsRoundTripsThroughParse(t *testing.T) {
	text := FormatCalls([]Call{{Name: "lookup", Arguments: `{ "q": "x" }`}, {Name: "other", Arguments: ""}})
	parsed := Parse(text, map[string]struct{}{"lookup": {}, "other": {}})
	if len(parsed.Calls) != 2 || parsed.Calls[0].Arguments != `{"q":"x"}` || parsed.Calls[1].Arguments != `{}` || parsed.Calls[0].ID == parsed.Calls[1].ID {
		t.Fatalf("parsed = %#v from %s", parsed, text)
	}
}

func writeFileBlock(arguments string) string {
	return "<tool_calls><tool_call><tool_name>write_file</tool_name><parameters>" + arguments + "</parameters></tool_call></tool_calls>"
}

// TestParseKeepsArgumentsExact: valid JSON is only compacted, never decoded
// and re-encoded or HTML-unescaped; HTML-escaped JSON is still accepted.
func TestParseKeepsArgumentsExact(t *testing.T) {
	available := map[string]struct{}{"write_file": {}}
	for _, c := range []struct{ in, want string }{
		{`{"id": 1820000000000000123, "z": 1.50, "a": 2}`, `{"id":1820000000000000123,"z":1.50,"a":2}`},
		{`{"content":"<p>a &lt; b &amp; c</p>"}`, `{"content":"<p>a &lt; b &amp; c</p>"}`},
		{`{"content":"<a title=\"&quot;x&quot;\">"}`, `{"content":"<a title=\"&quot;x&quot;\">"}`},
		{`{&quot;q&quot;:&quot;x&quot;}`, `{"q":"x"}`},
	} {
		if parsed := Parse(writeFileBlock(c.in), available); len(parsed.Calls) != 1 || parsed.Calls[0].Arguments != c.want {
			t.Fatalf("%s: calls = %#v", c.in, parsed.Calls)
		}
	}
	if got := NormalizeArguments(` {"n": 1820000000000000123, "s": "<b>"} `); got != `{"n":1820000000000000123,"s":"<b>"}` {
		t.Fatalf("NormalizeArguments = %s", got)
	}
	if got := NormalizeArguments(` not json `); got != "not json" {
		t.Fatalf("NormalizeArguments(text) = %s", got)
	}
	envelope := Parse(`{"tool_calls":[{"name":"write_file","arguments":{"id":1820000000000000123}}]}`, available)
	if len(envelope.Calls) != 1 || envelope.Calls[0].Arguments != `{"id":1820000000000000123}` {
		t.Fatalf("JSON calls = %#v", envelope.Calls)
	}
}

// sieveAll streams reply through a Sieve in chunks of size bytes.
func sieveAll(available map[string]struct{}, reply string, size int) (string, []Call) {
	sieve := NewSieve(available)
	var text strings.Builder
	var calls []Call
	for i := 0; i < len(reply); i += size {
		// As the ChatGPT adapter does: drain every complete block, show the
		// raw markup of blocks without declared calls.
		for result := sieve.Feed(reply[i:min(i+size, len(reply))]); ; result = sieve.Feed("") {
			text.WriteString(result.SafeText)
			calls = append(calls, result.Calls...)
			if !result.Complete {
				break
			}
			if len(result.Calls) == 0 {
				text.WriteString(result.Raw)
			}
		}
	}
	result := sieve.Flush()
	text.WriteString(result.SafeText)
	return text.String(), append(calls, result.Calls...)
}

// TestClosingTagsInsideArguments: an agent writing HTML or XML puts the
// convention's closing tags inside JSON strings; they stay arguments, also
// while streaming.
func TestClosingTagsInsideArguments(t *testing.T) {
	available := map[string]struct{}{"write_file": {}}
	arguments := `{"path":"a.xml","content":"</parameters></tool_call></tool_calls><tool_calls>"}`
	reply := "before " + writeFileBlock(arguments) + " after"
	parsed := Parse(reply, available)
	if len(parsed.Calls) != 1 || parsed.Calls[0].Arguments != arguments || Strip(reply, parsed) != "before  after" {
		t.Fatalf("parsed = %#v", parsed)
	}
	for _, size := range []int{1, 7, len(reply)} {
		if text, calls := sieveAll(available, reply, size); text != "before  after" || len(calls) != 1 || calls[0].Arguments != arguments {
			t.Fatalf("size %d: text %q calls %#v", size, text, calls)
		}
	}
	// JSON that never closes holds the block to the end, then shows it.
	broken := writeFileBlock(`{"content":"x</tool_calls> y`) + " tail"
	if text, calls := sieveAll(available, broken, 3); text != broken || len(calls) != 0 {
		t.Fatalf("broken: text %q calls %#v", text, calls)
	}
}

func TestSieveEndOfStreamEdgeCases(t *testing.T) {
	available := map[string]struct{}{"lookup": {}}
	call := `<tool_call><tool_name>lookup</tool_name><parameters>{"q":1}</parameters></tool_call>`
	block := "<tool_calls>" + call + "</tool_calls>"
	// A reply that ends without </tool_calls> keeps its complete calls.
	if text, calls := sieveAll(available, "Hi <tool_calls>"+call, 5); text != "Hi " || len(calls) != 1 || calls[0].Arguments != `{"q":1}` {
		t.Fatalf("unclosed root: text %q calls %#v", text, calls)
	}
	// Grok Web feeds each delta once, so Flush finds text before a block.
	sieve := NewSieve(available)
	undeclared := strings.ReplaceAll(block, "lookup", "rm")
	first := sieve.Feed(undeclared + " visible " + block)
	if rest := sieve.Flush(); first.Raw != undeclared || len(first.Calls) != 0 || rest.SafeText != " visible " || len(rest.Calls) != 1 {
		t.Fatalf("first %#v rest %#v", first, rest)
	}
	// One Markdown fence directly around a parsed block goes with it; other
	// fences stay.
	fenced := "Checking.\n```xml\n" + block + "\n```\nDone ```go\nx\n```"
	want := "Checking.\n\nDone ```go\nx\n```"
	if parsed := Parse(fenced, available); len(parsed.Calls) != 1 || Strip(fenced, parsed) != want {
		t.Fatalf("parsed %#v strip %q", parsed, Strip(fenced, parsed))
	}
	for _, size := range []int{1, 4, len(fenced)} {
		if text, calls := sieveAll(available, fenced, size); text != want || len(calls) != 1 {
			t.Fatalf("fenced size %d: text %q calls %#v", size, text, calls)
		}
	}
	if text, calls := sieveAll(available, "```xml\n<tool_calls>"+call+"\n```", 2); text != "" || len(calls) != 1 {
		t.Fatalf("fenced unclosed root: text %q calls %#v", text, calls)
	}
	// A bare ``` before a block may close a code block: it stays.
	code := "```go\nx\n```\n"
	if parsed := Parse(code+block, available); len(parsed.Calls) != 1 || Strip(code+block, parsed) != strings.TrimSpace(code) {
		t.Fatalf("code block: strip %q", Strip(code+block, parsed))
	}
	for _, size := range []int{1, len(code + block)} {
		if text, calls := sieveAll(available, code+block, size); text != code || len(calls) != 1 {
			t.Fatalf("code block size %d: text %q calls %#v", size, text, calls)
		}
	}
	// Without declared calls a fenced block stays text as written.
	shown := strings.ReplaceAll(fenced, "lookup", "rm")
	if text, calls := sieveAll(available, shown, 3); text != shown || len(calls) != 0 {
		t.Fatalf("undeclared fenced: text %q calls %#v", text, calls)
	}
}

func TestParseKeepsOnlyDeclaredFunctions(t *testing.T) {
	parsed := Parse(`<tool_calls><tool_call><tool_name>rm</tool_name><parameters>{}</parameters></tool_call><tool_call><tool_name>lookup</tool_name><parameters>{"q":1}</parameters></tool_call></tool_calls>`, map[string]struct{}{"lookup": {}})
	if len(parsed.Calls) != 1 || parsed.Calls[0].Name != "lookup" || !strings.HasPrefix(parsed.Calls[0].ID, "call_") {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestSieveHandlesSplitXML(t *testing.T) {
	sieve := NewSieve(map[string]struct{}{"lookup": {}})
	first := sieve.Feed("before <tool_ca")
	if first.SafeText != "before " || first.Complete {
		t.Fatalf("first = %#v", first)
	}
	second := sieve.Feed(`lls><tool_call><tool_name>lookup</tool_name><parameters>{"q":"x"}</parameters></tool_call></tool_calls>`)
	if !second.Complete || len(second.Calls) != 1 || second.Calls[0].Arguments != `{"q":"x"}` || strings.Contains(second.SafeText, "tool_calls") {
		t.Fatalf("second = %#v", second)
	}
}

func TestSievePreservesInvalidSyntaxAndTrailingText(t *testing.T) {
	sieve := NewSieve(map[string]struct{}{"lookup": {}})
	result := sieve.Feed(`<tool_calls><tool_call><tool_name>unknown</tool_name><parameters>{}</parameters></tool_call></tool_calls> trailing`)
	if !result.Complete || len(result.Calls) != 0 || !strings.HasPrefix(result.Raw, "<tool_calls>") {
		t.Fatalf("result = %#v", result)
	}
	if rest := sieve.Flush(); rest.SafeText != " trailing" {
		t.Fatalf("rest = %#v", rest)
	}
}

// TestSieveNeverLeaksMarkupFedCharByChar feeds runes one at a time, with a
// multi-byte rune whose lowercase form is longer, text between two blocks and
// text after them.
func TestSieveNeverLeaksMarkupFedCharByChar(t *testing.T) {
	reply := "İstanbul <tool_calls>\n<tool_call><tool_name>lookup</tool_name><parameters>{\"q\":\"a<b\"}</parameters></tool_call>\n</tool_calls> mid <TOOL_CALLS><tool_call><tool_name>lookup</tool_name><parameters>{\"q\":2}</parameters></tool_call></TOOL_CALLS> end"
	sieve := NewSieve(map[string]struct{}{"lookup": {}})
	var text strings.Builder
	var calls []Call
	take := func(result StreamResult) {
		if strings.Contains(strings.ToLower(result.SafeText), "tool") {
			t.Fatalf("markup leaked: %q", result.SafeText)
		}
		text.WriteString(result.SafeText)
		calls = append(calls, result.Calls...)
	}
	for _, char := range reply {
		take(sieve.Feed(string(char)))
	}
	take(sieve.Flush())
	var first struct{ Q string }
	if text.String() != "İstanbul  mid  end" || len(calls) != 2 || json.Unmarshal([]byte(calls[0].Arguments), &first) != nil || first.Q != "a<b" || calls[1].Arguments != `{"q":2}` {
		t.Fatalf("text %q calls %#v", text.String(), calls)
	}
}
