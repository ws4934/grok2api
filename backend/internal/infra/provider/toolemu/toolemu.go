// Package toolemu emulates function calling for upstreams that only write
// text: declared functions become a prompt with an XML call convention, and
// calls are parsed back out of the reply, also while it streams.
package toolemu

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"
)

const (
	maxFunctionTools       = 128
	maxToolDescriptionSize = 16 << 10
)

var (
	namePattern         = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	syntaxPattern       = regexp.MustCompile(`(?i)<tool_calls|<tool_call|<function_call|<invoke\s|"tool_calls"\s*:`)
	rootOpenPattern     = regexp.MustCompile(`(?i)<tool_calls\s*>`)
	tagPattern          = regexp.MustCompile(`(?i)<(/?)(tool_calls|tool_call|tool_name|parameters)\s*>`)
	nameClosePattern    = regexp.MustCompile(`(?i)^</tool_name\s*>`)
	paramsEndPattern    = regexp.MustCompile(`(?i)</parameters\s*>`)
	paramsClosePattern  = regexp.MustCompile(`(?i)^\s*</parameters\s*>`)
	functionCallPattern = regexp.MustCompile(`(?is)<function_call\s*>(.*?)</function_call\s*>`)
	functionNamePattern = regexp.MustCompile(`(?is)<name\s*>(.*?)</name\s*>`)
	functionArgsPattern = regexp.MustCompile(`(?is)<arguments\s*>(.*?)</arguments\s*>`)
	invokePattern       = regexp.MustCompile(`(?is)<invoke\s+name=["']?([A-Za-z0-9_-]+)["']?\s*>(.*?)</invoke\s*>`)
)

// Function is one declared client function.
type Function struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Config is the tool setup of a Chat Completions or Responses request.
type Config struct {
	Functions []Function
	// HostedWebSearch is set when web_search or web_search_preview is declared.
	HostedWebSearch bool
	// ForcedWebSearch is set when tool_choice names the hosted web search tool.
	ForcedWebSearch bool
	// Unsupported lists the other declared tool types; the caller decides.
	Unsupported    []string
	Choice         string
	ForcedName     string
	ResponseTools  []any
	ResponseChoice any
	// Available holds the declared function names; only these are parsed.
	Available map[string]struct{}
}

// Call is one function call parsed from the reply.
type Call struct {
	ID        string
	Name      string
	Arguments string
}

type ParseResult struct {
	Calls     []Call
	SawSyntax bool
	Start     int
	End       int
}

type StreamResult struct {
	SafeText string
	Calls    []Call
	Complete bool
	Raw      string
}

// ParseConfig 兼容 Chat Completions 与 Responses 的函数工具结构。
func ParseConfig(rawTools, rawChoice json.RawMessage) (Config, error) {
	configuration := Config{Choice: "auto", ResponseChoice: "auto"}
	trimmed := bytes.TrimSpace(rawTools)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		var values []map[string]any
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return Config{}, errors.New("tools 必须是数组")
		}
		if len(values) > maxFunctionTools {
			return Config{}, fmt.Errorf("tools 不能超过 %d 个", maxFunctionTools)
		}
		configuration.ResponseTools = make([]any, 0, len(values))
		for _, value := range values {
			configuration.ResponseTools = append(configuration.ResponseTools, value)
			function, supported, err := parseFunctionTool(value)
			if err != nil {
				return Config{}, err
			}
			if supported {
				configuration.Functions = append(configuration.Functions, function)
				continue
			}
			typeName, _ := value["type"].(string)
			switch strings.ToLower(strings.TrimSpace(typeName)) {
			case "web_search", "web_search_preview":
				// 搜索由上游执行，这两个标准声明无需注入函数提示词。
				configuration.HostedWebSearch = true
			default:
				configuration.Unsupported = append(configuration.Unsupported, typeName)
			}
		}
	}

	choice, forcedName, responseChoice, err := parseToolChoice(rawChoice)
	if err != nil {
		return Config{}, err
	}
	if choice == "web_search" {
		if !configuration.HostedWebSearch {
			return Config{}, errors.New("tool_choice 指定了 web_search，但 tools 中没有声明")
		}
		choice, configuration.ForcedWebSearch = "required", true
	}
	configuration.Choice = choice
	configuration.ForcedName = forcedName
	configuration.ResponseChoice = responseChoice
	configuration.Available = make(map[string]struct{}, len(configuration.Functions))
	for _, function := range configuration.Functions {
		if _, exists := configuration.Available[function.Name]; exists {
			return Config{}, fmt.Errorf("function tool 名称 %q 重复", function.Name)
		}
		configuration.Available[function.Name] = struct{}{}
	}
	if forcedName != "" {
		if _, ok := configuration.Available[forcedName]; !ok {
			return Config{}, fmt.Errorf("tool_choice 指定的函数 %q 不存在", forcedName)
		}
	}
	if (choice == "required" || forcedName != "") && len(configuration.Functions) == 0 && !configuration.HostedWebSearch {
		return Config{}, errors.New("tool_choice 要求调用函数，但 tools 中没有可用函数")
	}
	return configuration, nil
}

// Enabled reports whether the model may call a declared function.
func (c Config) Enabled() bool {
	return len(c.Functions) > 0 && c.Choice != "none"
}

func parseFunctionTool(value map[string]any) (Function, bool, error) {
	typeName, _ := value["type"].(string)
	if strings.ToLower(strings.TrimSpace(typeName)) != "function" {
		return Function{}, false, nil
	}
	definition := value
	if nested, ok := value["function"].(map[string]any); ok {
		definition = nested
	}
	name, _ := definition["name"].(string)
	name = strings.TrimSpace(name)
	if !namePattern.MatchString(name) {
		return Function{}, false, errors.New("function tool 的 name 必须是 1 到 64 位字母、数字、下划线或连字符")
	}
	description, _ := definition["description"].(string)
	if len(description) > maxToolDescriptionSize {
		return Function{}, false, fmt.Errorf("函数 %q 的 description 过长", name)
	}
	parameters := json.RawMessage(`{"type":"object","properties":{}}`)
	if raw, ok := definition["parameters"]; ok {
		encoded, err := json.Marshal(raw)
		if err != nil || !json.Valid(encoded) {
			return Function{}, false, fmt.Errorf("函数 %q 的 parameters 不是有效 JSON", name)
		}
		parameters = encoded
	}
	return Function{Name: name, Description: strings.TrimSpace(description), Parameters: parameters}, true, nil
}

func parseToolChoice(raw json.RawMessage) (string, string, any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "auto", "", "auto", nil
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		text = strings.ToLower(strings.TrimSpace(text))
		switch text {
		case "auto", "none", "required":
			return text, "", text, nil
		default:
			return "", "", nil, errors.New("tool_choice 必须是 auto、none、required 或函数对象")
		}
	}
	var value map[string]any
	if json.Unmarshal(trimmed, &value) != nil {
		return "", "", nil, errors.New("tool_choice 格式无效")
	}
	typeName, _ := value["type"].(string)
	typeName = strings.ToLower(strings.TrimSpace(typeName))
	switch typeName {
	case "none", "auto", "required":
		return typeName, "", value, nil
	case "web_search", "web_search_preview":
		return "web_search", "", value, nil
	case "function":
		name, _ := value["name"].(string)
		if nested, ok := value["function"].(map[string]any); ok {
			name, _ = nested["name"].(string)
		}
		name = strings.TrimSpace(name)
		if !namePattern.MatchString(name) {
			return "", "", nil, errors.New("tool_choice.function.name 无效")
		}
		return "required", name, value, nil
	default:
		return "", "", nil, fmt.Errorf("暂不支持 tool_choice.type=%q", typeName)
	}
}

// Prompt 将函数定义转换为模型可稳定生成的 XML 调用约定；没有可调用函数时返回空串。
func Prompt(configuration Config) string {
	if !configuration.Enabled() {
		return ""
	}
	var definitions strings.Builder
	for index, function := range configuration.Functions {
		if index > 0 {
			definitions.WriteString("\n\n")
		}
		definitions.WriteString("Tool: ")
		definitions.WriteString(function.Name)
		if function.Description != "" {
			definitions.WriteString("\nDescription: ")
			definitions.WriteString(function.Description)
		}
		definitions.WriteString("\nParameters: ")
		definitions.Write(function.Parameters)
	}
	choiceInstruction := "Call a tool when it is clearly needed. Otherwise respond in plain text."
	if configuration.ForcedName != "" {
		choiceInstruction = fmt.Sprintf("You MUST call the tool named %q and must not write a plain-text reply.", configuration.ForcedName)
	} else if configuration.Choice == "required" && !configuration.HostedWebSearch {
		choiceInstruction = "You MUST call at least one available tool and must not write a plain-text reply."
	}
	return fmt.Sprintf(`You have access to the following tools.

AVAILABLE TOOLS:
%s

TOOL CALL FORMAT - follow these rules exactly:
- When calling a tool, output only the XML block below, with no text before or after it.
- <parameters> must contain one valid JSON object.
- Put multiple calls inside one <tool_calls> element.
- Do not use Markdown code fences.

<tool_calls>
  <tool_call>
    <tool_name>TOOL_NAME</tool_name>
    <parameters>{"key":"value"}</parameters>
  </tool_call>
</tool_calls>

WHEN TO CALL: %s`, definitions.String(), choiceInstruction)
}

// ValidName reports whether name is a valid function name.
func ValidName(name string) bool {
	return namePattern.MatchString(name)
}

// FormatCalls renders earlier calls in the prompt convention, for history.
func FormatCalls(calls []Call) string {
	var builder strings.Builder
	builder.WriteString("<tool_calls>")
	for _, call := range calls {
		builder.WriteString("\n  <tool_call>\n    <tool_name>")
		builder.WriteString(html.EscapeString(call.Name))
		builder.WriteString("</tool_name>\n    <parameters>")
		builder.WriteString(NormalizeArguments(call.Arguments))
		builder.WriteString("</parameters>\n  </tool_call>")
	}
	builder.WriteString("\n</tool_calls>")
	return builder.String()
}

// Parse 解析模型输出，并仅保留请求中声明过的函数名。
func Parse(text string, available map[string]struct{}) ParseResult {
	result := ParseResult{SawSyntax: syntaxPattern.MatchString(text), Start: -1, End: -1}
	if !result.SawSyntax {
		return result
	}
	if open := rootOpenPattern.FindStringIndex(text); open != nil {
		root := rootScan{pos: open[1]}
		if end := root.scan(text, true); end >= 0 {
			result.Start, result.End = unfence(text, open[0], end)
			for _, call := range root.calls {
				appendCall(&result.Calls, html.UnescapeString(strings.TrimSpace(call.name)), call.arguments, available)
			}
			return result
		}
	}
	if match := functionCallPattern.FindStringSubmatchIndex(text); match != nil {
		result.Start, result.End = match[0], match[1]
		inner := text[match[2]:match[3]]
		nameMatch := functionNamePattern.FindStringSubmatch(inner)
		if len(nameMatch) > 0 {
			arguments := "{}"
			if argsMatch := functionArgsPattern.FindStringSubmatch(inner); len(argsMatch) > 0 {
				arguments = argsMatch[1]
			}
			appendCall(&result.Calls, html.UnescapeString(strings.TrimSpace(nameMatch[1])), arguments, available)
		}
		return result
	}
	if match := invokePattern.FindStringSubmatchIndex(text); match != nil {
		result.Start, result.End = match[0], match[1]
		appendCall(&result.Calls, text[match[2]:match[3]], text[match[4]:match[5]], available)
		return result
	}
	return parseJSONCalls(text, available, result)
}

type rawCall struct{ name, arguments string }

// rootScan reads the content of a <tool_calls> element from pos, right after
// its opening tag. Arguments are read as JSON, so tags inside JSON strings
// stay arguments. It keeps its place, so a Sieve resumes it as the block
// grows and reads a long block once.
type rootScan struct {
	pos   int
	calls []rawCall
	call  *rawCall
	// args reads the <parameters> of call.
	args *argScan
}

// scan returns the end of the closing tag, or -1 when text ends first; unless
// final, content cut off by the end of text also returns -1, as more text may
// complete it. Each call passes text that extends the text of the last one.
func (r *rootScan) scan(text string, final bool) int {
	for {
		if r.args != nil {
			arguments, end := r.args.scan(text, final)
			if end < 0 {
				return -1
			}
			r.call.arguments, r.pos, r.args = arguments, end, nil
		}
		loc := tagPattern.FindStringSubmatchIndex(text[r.pos:])
		if loc == nil {
			return -1
		}
		closing, tag := loc[3] > loc[2], strings.ToLower(text[r.pos+loc[4]:r.pos+loc[5]])
		pos := r.pos + loc[1]
		switch {
		case closing && tag == "tool_calls":
			return pos
		case tag == "tool_call":
			if closing && r.call != nil {
				r.calls = append(r.calls, *r.call)
			}
			r.call = nil
			if !closing {
				r.call = &rawCall{arguments: "{}"}
			}
		case r.call == nil || closing:
		case tag == "tool_name":
			// The name runs to the next tag, which must close it; that tag
			// ends at the first '>' after its '<'.
			next := strings.IndexByte(text[pos:], '<')
			if !final && (next < 0 || strings.IndexByte(text[pos+next:], '>') < 0) {
				return -1
			}
			if next >= 0 {
				if end := nameClosePattern.FindStringIndex(text[pos+next:]); end != nil {
					r.call.name = text[pos : pos+next]
					pos += next + end[1]
				}
			}
		case tag == "parameters":
			r.args = &argScan{start: pos, end: jsonOpen, value: jsonScan{off: pos}}
		}
		r.pos = pos
	}
}

// argScan reads <parameters> content from start: one JSON value and the
// closing tag. Other content (HTML-escaped JSON, say) runs to the first
// </parameters>.
type argScan struct {
	start int
	value jsonScan
	// end is where the JSON value ends, or jsonOpen or jsonBad.
	end int
	// plain is set once the content is not a JSON value and its closing tag;
	// seek is where the search for the closing tag goes on.
	plain bool
	seek  int
}

// scan returns the arguments and the end of the closing tag, or -1 as
// rootScan.scan does.
func (a *argScan) scan(text string, final bool) (string, int) {
	if !a.plain && a.end == jsonOpen {
		if a.end = a.value.scan(text, final); a.end == jsonOpen && !final {
			return "", -1
		}
		a.seek = a.end
	}
	if !a.plain && a.end >= 0 {
		// A closing tag right after the value ends at the next '>'.
		if !final && strings.IndexByte(text[a.seek:], '>') < 0 {
			a.seek = len(text)
			return "", -1
		}
		if closing := paramsClosePattern.FindStringIndex(text[a.end:]); closing != nil {
			return text[a.start:a.end], a.end + closing[1]
		}
	}
	if !a.plain {
		a.plain, a.seek = true, a.start
	}
	if closing := paramsEndPattern.FindStringIndex(text[a.seek:]); closing != nil {
		return text[a.start : a.seek+closing[0]], a.seek + closing[1]
	}
	// More text can complete only a tag that starts at the last '<'.
	if last := strings.LastIndexByte(text[a.seek:], '<'); last >= 0 {
		a.seek += last
	} else {
		a.seek = len(text)
	}
	return "", -1
}

func parseJSONCalls(text string, available map[string]struct{}, result ParseResult) ParseResult {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return result
	}
	decoder := json.NewDecoder(strings.NewReader(text[start:]))
	decoder.UseNumber() // arguments are re-encoded below; keep big integers exact
	var envelope struct {
		ToolCalls []map[string]any `json:"tool_calls"`
	}
	if decoder.Decode(&envelope) != nil || len(envelope.ToolCalls) == 0 {
		return result
	}
	result.Start = start
	result.End = len(text)
	for _, value := range envelope.ToolCalls {
		name, _ := value["name"].(string)
		arguments := value["arguments"]
		if function, ok := value["function"].(map[string]any); ok {
			name, _ = function["name"].(string)
			arguments = function["arguments"]
		}
		if name == "" {
			name, _ = value["tool_name"].(string)
		}
		if arguments == nil {
			arguments = value["parameters"]
		}
		argumentText := "{}"
		if text, ok := arguments.(string); ok {
			argumentText = text
		} else if arguments != nil {
			encoded, _ := json.Marshal(arguments)
			argumentText = string(encoded)
		}
		appendCall(&result.Calls, strings.TrimSpace(name), argumentText, available)
	}
	return result
}

func appendCall(calls *[]Call, name, arguments string, available map[string]struct{}) {
	if _, ok := available[name]; !ok {
		return
	}
	arguments = strings.TrimSpace(arguments)
	if !json.Valid([]byte(arguments)) {
		// Some models HTML-escape the whole JSON; valid JSON is kept as written.
		arguments = html.UnescapeString(arguments)
	}
	// Compacted valid JSON starts with '{' only when it is an object.
	arguments = NormalizeArguments(arguments)
	if !strings.HasPrefix(arguments, "{") || !json.Valid([]byte(arguments)) {
		return
	}
	*calls = append(*calls, Call{ID: newCallID(), Name: name, Arguments: arguments})
}

func newCallID() string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return "call_" + hex.EncodeToString(value)
}

// NormalizeArguments compacts JSON arguments without re-encoding them, so
// numbers, key order and escapes stay as written; other text is only trimmed.
func NormalizeArguments(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}"
	}
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(value)) != nil {
		return value
	}
	return compact.String()
}

// Strip removes the parsed call markup from text.
func Strip(text string, parsed ParseResult) string {
	if parsed.Start < 0 || parsed.End <= parsed.Start || parsed.End > len(text) {
		return text
	}
	return strings.TrimSpace(text[:parsed.Start] + text[parsed.End:])
}

const (
	openTag  = "<tool_calls"
	closeTag = "</tool_calls>"
	// maxFence bounds the whitespace and language name of a Markdown fence
	// recognised around a block.
	maxFence = 64
)

// Sieve holds back tool markup in a text stream until its block is complete,
// so clients never see it. Text after a block stays buffered and is scanned
// with the next Feed (Feed("") drains it) or returned by Flush.
type Sieve struct {
	available map[string]struct{}
	// buffer is held text that may still begin a block.
	buffer string
	// block is the block being captured, from a Markdown fence opener before
	// it if any (open is where <tool_calls starts), and scanned how much of it
	// was searched for closeTag, so a long block costs linear time.
	block         strings.Builder
	capturing     bool
	open, scanned int
	// root reads the block's <tool_calls> element as it grows.
	root rootScan
	// unfence drops the closing fence after a fenced block that became calls.
	unfence bool
}

func NewSieve(available map[string]struct{}) *Sieve {
	return &Sieve{available: available}
}

// Feed 在流中发现工具 XML 后开始缓存，完整解析前不向客户端泄露内部标记。
func (s *Sieve) Feed(chunk string) StreamResult {
	safe := ""
	if s.capturing {
		s.block.WriteString(chunk)
	} else {
		text := s.buffer + chunk
		if s.unfence {
			n := fenceClose(text, false)
			if n == -2 {
				s.buffer = text
				return StreamResult{}
			}
			if s.unfence = false; n > 0 {
				text = text[n:]
			}
		}
		index := indexFold(text, openTag)
		if index < 0 {
			hold := holdFrom(text)
			s.buffer = text[hold:]
			return StreamResult{SafeText: text[:hold]}
		}
		start := index
		if fence, partial := fenceTail(text[:index]); fence >= 0 && !partial {
			start = fence
		}
		safe, s.buffer = text[:start], ""
		s.block.WriteString(text[start:])
		s.capturing, s.open, s.scanned = true, index-start, index-start
	}
	raw := s.block.String()
	for {
		at := indexFold(raw[s.scanned:], closeTag)
		if at < 0 {
			s.scanned = max(s.scanned, len(raw)-len(closeTag)+1)
			return StreamResult{SafeText: safe}
		}
		s.scanned += at + len(closeTag)
		// A </tool_calls> inside a JSON argument does not end the block.
		if end := s.blockEnd(raw, s.scanned); end >= 0 {
			calls := Parse(raw[:end], s.available).Calls
			s.buffer, s.capturing, s.unfence = strings.Clone(raw[end:]), false, s.open > 0 && len(calls) > 0
			s.block.Reset()
			s.root = rootScan{}
			return StreamResult{SafeText: safe, Calls: calls, Complete: true, Raw: raw[:end]}
		}
	}
}

// blockEnd is where a captured block ends: after the </tool_calls> that closes
// its root outside JSON arguments, or at candidate when it does not open with
// a plain <tool_calls>; -1 while the root is still open.
func (s *Sieve) blockEnd(raw string, candidate int) int {
	if s.root.pos == 0 {
		open := rootOpenPattern.FindStringIndex(raw[s.open:])
		if open == nil || open[0] != 0 {
			return candidate
		}
		s.root.pos = s.open + open[1]
	}
	return s.root.scan(raw, false)
}

// Flush ends the stream. Complete blocks are taken as Feed takes them (the
// markup of a block without declared calls becomes text), a block left open
// is parsed as if its root were closed, and the rest is released.
func (s *Sieve) Flush() StreamResult {
	var text strings.Builder
	var calls []Call
	for result := s.Feed(""); ; result = s.Feed("") {
		text.WriteString(result.SafeText)
		calls = append(calls, result.Calls...)
		if !result.Complete {
			break
		}
		if len(result.Calls) == 0 {
			text.WriteString(result.Raw)
		}
	}
	raw := s.buffer
	if s.capturing {
		raw = s.block.String()
	}
	if s.unfence {
		if n := fenceClose(raw, true); n >= 0 {
			raw = raw[n:]
		}
	}
	parsed := Parse(raw, s.available)
	if s.capturing && parsed.End < 0 {
		parsed = Parse(raw+closeTag, s.available)
		parsed.End = min(parsed.End, len(raw))
	}
	*s = Sieve{available: s.available}
	if len(parsed.Calls) > 0 {
		text.WriteString(raw[:parsed.Start])
		text.WriteString(raw[parsed.End:])
		calls = append(calls, parsed.Calls...)
	} else {
		text.WriteString(raw)
	}
	return StreamResult{SafeText: text.String(), Calls: calls, Complete: len(calls) > 0 || parsed.SawSyntax, Raw: raw}
}

// holdFrom is where a block may begin at the end of text: a partial
// <tool_calls, or a Markdown fence opener that may come before one.
func holdFrom(text string) int {
	hold := len(text)
	for size := min(len(openTag)-1, len(text)); size > 0; size-- {
		if hasFoldPrefix(text[len(text)-size:], openTag[:size]) {
			hold = len(text) - size
			break
		}
	}
	if fence, _ := fenceTail(text[:hold]); fence >= 0 {
		return fence
	}
	return hold
}

// unfence widens [start, end) of a parsed block over one Markdown fence that
// directly wraps it; a fence left open ends with the text.
func unfence(text string, start, end int) (int, int) {
	if fence, partial := fenceTail(text[:start]); fence >= 0 && !partial {
		if n := fenceClose(text[end:], true); n >= 0 {
			return fence, end + n
		}
	}
	return start, end
}

// fenceTail finds a Markdown fence opener (``` and a language name) that ends
// text, trailing whitespace aside. It returns where the opener starts, or -1;
// partial reports backticks at the end of text: more text may make them an
// opener, or a longer run that is none. A bare ``` does not count: it may
// close an earlier code block.
func fenceTail(text string) (int, bool) {
	limit := max(len(text)-maxFence, 0)
	end := len(text)
	for end > limit && isSpace(text[end-1]) {
		end--
	}
	i := end
	for i > limit && isInfo(text[i-1]) {
		i--
	}
	j := i
	for j > limit && text[j-1] == '`' {
		j--
	}
	switch run := i - j; {
	case j > 0 && text[j-1] == '`':
	case run == 3 && end > i:
		return j, false
	case run > 0 && i == len(text):
		return j, true
	}
	return -1, false
}

// fenceClose matches the line that closes a Markdown fence at the start of
// text: whitespace, then ``` with no language name after it. It returns the
// length to drop, or -1. At the end of the reply (final) whitespace alone
// also closes the fence; before that, -2 asks for more text.
func fenceClose(text string, final bool) int {
	i := 0
	for i < len(text) && i < maxFence && isSpace(text[i]) {
		i++
	}
	switch rest := text[i:]; {
	case len(rest) > 3 && strings.HasPrefix(rest, "```"):
		if isInfo(rest[3]) || rest[3] == '`' {
			return -1
		}
		return i + 3
	case !final && i < maxFence && strings.HasPrefix("```", rest):
		return -2
	case rest == "```" || final && rest == "":
		return len(text)
	}
	return -1
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// isInfo reports a byte of a fence's language name, such as xml or c++.
func isInfo(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("_-+.#", c) >= 0
}

// indexFold is strings.Index on text with ASCII letters lowercased, without
// copying text; sub is lowercase and starts with '<'.
func indexFold(text, sub string) int {
	for i := 0; ; i++ {
		next := strings.IndexByte(text[i:], '<')
		if next < 0 {
			return -1
		}
		if i += next; hasFoldPrefix(text[i:], sub) {
			return i
		}
	}
}

// hasFoldPrefix is strings.HasPrefix ignoring ASCII case; prefix is lowercase.
func hasFoldPrefix(text, prefix string) bool {
	if len(text) < len(prefix) {
		return false
	}
	for i := range len(prefix) {
		c := text[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}
