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
	"io"
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
	nameEndPattern      = regexp.MustCompile(`(?i)</tool_name\s*>`)
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
		if calls, end := scanRoot(text, open[1], true); end >= 0 {
			result.Start, result.End = open[0], end
			for _, call := range calls {
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

// scanRoot reads the content of a <tool_calls> element from pos, right after
// its opening tag, and returns its calls and the end of its closing tag, or -1
// when text ends first. Arguments are read as JSON, so tags inside JSON
// strings stay arguments; unless final, JSON cut off by the end of text
// also returns -1, as more text may close it.
func scanRoot(text string, pos int, final bool) ([]rawCall, int) {
	var calls []rawCall
	var call *rawCall
	for {
		loc := tagPattern.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			return calls, -1
		}
		closing, tag := loc[3] > loc[2], strings.ToLower(text[pos+loc[4]:pos+loc[5]])
		pos += loc[1]
		switch {
		case closing && tag == "tool_calls":
			return calls, pos
		case tag == "tool_call":
			if closing && call != nil {
				calls = append(calls, *call)
			}
			call = nil
			if !closing {
				call = &rawCall{arguments: "{}"}
			}
		case call == nil || closing:
		case tag == "tool_name":
			if end := nameEndPattern.FindStringIndex(text[pos:]); end != nil {
				call.name = text[pos : pos+end[0]]
				pos += end[1]
			}
		case tag == "parameters":
			arguments, end := scanArguments(text, pos, final)
			if end < 0 {
				return calls, -1
			}
			call.arguments, pos = arguments, end
		}
	}
}

// scanArguments reads <parameters> content from pos: one JSON value and the
// closing tag. Other content (HTML-escaped JSON, say) runs to the first
// </parameters>. It returns the arguments and the end of the closing tag, or
// -1 as scanRoot does.
func scanArguments(text string, pos int, final bool) (string, int) {
	decoder := json.NewDecoder(strings.NewReader(text[pos:]))
	var value json.RawMessage
	switch err := decoder.Decode(&value); {
	case err == nil:
		end := pos + int(decoder.InputOffset())
		if closing := paramsClosePattern.FindStringIndex(text[end:]); closing != nil {
			return text[pos:end], end + closing[1]
		}
	case !final && errors.Is(err, io.ErrUnexpectedEOF):
		return "", -1
	}
	if closing := paramsEndPattern.FindStringIndex(text[pos:]); closing != nil {
		return text[pos : pos+closing[0]], pos + closing[1]
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
)

// Sieve holds back tool markup in a text stream until its block is complete,
// so clients never see it. Text after a block stays buffered and is scanned
// with the next Feed (Feed("") drains it) or returned by Flush.
type Sieve struct {
	available map[string]struct{}
	// buffer is held text that may still begin a block.
	buffer string
	// block is the block being captured and scanned how much of it was
	// searched for closeTag, so a long block costs linear time.
	block     strings.Builder
	capturing bool
	scanned   int
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
		index := indexFold(text, openTag)
		if index < 0 {
			hold := holdFrom(text)
			s.buffer = text[hold:]
			return StreamResult{SafeText: text[:hold]}
		}
		safe, s.buffer = text[:index], ""
		s.block.WriteString(text[index:])
		s.capturing, s.scanned = true, 0
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
		if end := blockEnd(raw, s.scanned); end >= 0 {
			s.buffer, s.capturing = strings.Clone(raw[end:]), false
			s.block.Reset()
			return StreamResult{SafeText: safe, Calls: Parse(raw[:end], s.available).Calls, Complete: true, Raw: raw[:end]}
		}
	}
}

// blockEnd is where a captured block ends: after the </tool_calls> that closes
// its root outside JSON arguments, or at candidate when it does not open with
// a plain <tool_calls>; -1 while the root is still open.
func blockEnd(raw string, candidate int) int {
	open := rootOpenPattern.FindStringIndex(raw)
	if open == nil || open[0] != 0 {
		return candidate
	}
	_, end := scanRoot(raw, open[1], false)
	return end
}

// Flush ends the stream: held text is parsed once more or released.
func (s *Sieve) Flush() StreamResult {
	raw := s.buffer
	if s.capturing {
		raw = s.block.String()
	}
	s.buffer, s.capturing = "", false
	s.block.Reset()
	if raw == "" {
		return StreamResult{}
	}
	parsed := Parse(raw, s.available)
	if len(parsed.Calls) > 0 {
		return StreamResult{Calls: parsed.Calls, Complete: true, Raw: raw}
	}
	return StreamResult{SafeText: raw, Complete: parsed.SawSyntax, Raw: raw}
}

// holdFrom is where a block may begin at the end of text: a partial <tool_calls.
func holdFrom(text string) int {
	for size := min(len(openTag)-1, len(text)); size > 0; size-- {
		if hasFoldPrefix(text[len(text)-size:], openTag[:size]) {
			return len(text) - size
		}
	}
	return len(text)
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
