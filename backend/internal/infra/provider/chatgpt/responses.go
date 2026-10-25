package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/toolemu"
	"github.com/google/uuid"
	"github.com/tiktoken-go/tokenizer"
)

// toolsUnavailable is prepended when the client declares tools the model
// cannot call (non-function tools, or functions with tool_choice none), so it
// answers in plain text; onlyWebSearch replaces it when web search, which
// ChatGPT runs itself, is declared too.
const (
	toolsUnavailable = "Tools are unavailable in this session: you cannot call functions, run commands or read files. Answer directly in plain text and never claim to have used a tool."
	onlyWebSearch    = "Web search is the only tool available in this session: you cannot call functions, run commands or read files. Never claim to have used another tool."
)

// ChatGPT otherwise sends the declared functions through its own tool
// channel (functions, container), where they fail, or prefers its built-in
// tools (web search, weather and other widgets) to them.
const (
	clientTools    = "These tools run on the client and are not in your tool interface: a call sent to functions.exec, code execution or any other built-in tool never reaches them and is lost. To call one, write the <tool_calls> block in the visible text of your final reply."
	noBuiltinTools = "Do not search the web, browse, run code, or use any built-in tool or widget (weather, maps, images, canvas). When a listed tool can provide the information, call it instead of answering from your own knowledge."
)

// plainMarkdown overrides the Web app's generative UI, which tool results
// request "unless otherwise specified": API clients cannot render it.
const plainMarkdown = "Format every reply as plain Markdown for an API client that cannot render UI components. Do not write layout or widget tags such as <box>, <row>, <grid>, <card>, <title>, <text>, <badge>, <icon>, <divider> or <AsyncImage/>; present cards, widgets and tool results as Markdown text, lists or tables. <Cite/> citations remain allowed."

const defaultOutputTokenLimit = 16384

var errOutputLimit = errors.New("local output limit")

// turnRequest is a Responses request reduced to what ChatGPT Web can run.
type turnRequest struct {
	Messages          []turnMessage
	ReasoningEffort   string
	MaxOutputTokens   int
	Tools             []json.RawMessage
	ToolChoice        json.RawMessage
	ParallelToolCalls bool
	// Emulated holds the function tools, called through the toolemu prompt.
	Emulated toolemu.Config
	// Search asks ChatGPT to search: the client forces its web_search tool.
	Search bool
}

type turnMessage struct {
	Role  string
	Parts []turnPart
	// calls is earlier function calls, rendered in the toolemu convention.
	calls []toolemu.Call
}

// turnPart is text, or an image given as a data URI or https URL.
type turnPart struct {
	Text  string
	Image string
}

// ForwardResponse serves Responses natively; Chat Completions and Messages are
// converted to Responses and back, like the other adapters.
func (a *Adapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	operation := request.Operation
	if request.NormalizedMetadata != nil {
		*request.NormalizedMetadata = provider.NormalizedRequestMetadata{}
	}
	if request.Method != http.MethodPost || request.Path != "/responses" {
		return errorResponse(operation, http.StatusBadRequest, "unsupported_endpoint", "ChatGPT only supports POST /responses"), nil
	}
	body := request.Body
	var options conversation.ResponseOptions
	if request.NormalizeBody && (operation == conversation.OperationChat || operation == conversation.OperationMessages) {
		var err error
		if body, options, err = conversation.ConvertRequestWithOptions(body, request.Model, operation); err != nil {
			return errorResponse(operation, http.StatusBadRequest, "invalid_request", err.Error()), nil
		}
	}
	// ChatGPT reports none of the xAI usage extensions.
	options.OpenAIUsage = true
	in, err := parseTurn(body)
	if err != nil {
		return errorResponse(operation, http.StatusBadRequest, "invalid_request", err.Error()), nil
	}
	// Messages maps tool_choice web_search to "required" even beside functions.
	in.Search = in.Search || options.AnthropicWebSearchRequired
	if request.NormalizedMetadata != nil {
		request.NormalizedMetadata.ReasoningEffort = in.ReasoningEffort
	}
	c, err := a.decrypt(request.Credential)
	if err != nil {
		return nil, err
	}
	credential := request.Credential
	if request.ForcedEgressNodeID != 0 {
		credential.EgressNodeID = request.ForcedEgressNodeID
	}
	cfg := a.config()
	limit := int64(cfg.OutputTokenLimit)
	if limit <= 0 {
		limit = defaultOutputTokenLimit
	}
	if in.MaxOutputTokens > 0 && int64(in.MaxOutputTokens) < limit {
		limit = int64(in.MaxOutputTokens)
	}
	t := &turn{id: "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""), created: time.Now().Unix(), model: request.Model, request: in}
	m := &meter{limit: limit}
	// Conversations are continued only within the gateway's session identity,
	// which is scoped to the client key and the model.
	seed := ""
	if request.PromptCacheKey != "" {
		seed = request.PromptCacheKey + "\x00" + request.Model
	}
	run := func(ctx context.Context) (map[string]any, error) {
		next, err := a.converse(ctx, credential, c, request.Model, in, seed, t, m)
		if err != nil {
			return nil, err
		}
		// Only usage needs the input count, so it is taken after the reply
		// instead of delaying the upstream call, and a failed attempt skips it.
		inputTokens, err := estimateInput(in.Messages, cfg.VisionTokens)
		if err != nil {
			return nil, err
		}
		usage, err := m.usage(inputTokens)
		if err != nil {
			return nil, err
		}
		final, err := t.finish(usage, m.truncated)
		if err == nil && !m.truncated {
			a.remember(seed, in.Messages, t, credential.ID, next)
		}
		return final, err
	}
	if !request.Streaming {
		final, err := run(ctx)
		if err != nil {
			return failureResponse(operation, err)
		}
		data, err := json.Marshal(final)
		if err != nil {
			return nil, err
		}
		if operation == conversation.OperationChat || operation == conversation.OperationMessages {
			if data, err = conversation.ConvertResponseJSONWithOptions(data, operation, options); err != nil {
				return nil, err
			}
		}
		return jsonResponse(http.StatusOK, data), nil
	}
	reader, writer := io.Pipe()
	runCtx, cancel := context.WithCancel(ctx)
	started := make(chan error, 1)
	t.send = func(event string, value map[string]any) error {
		if !t.begun {
			t.begun = true
			started <- nil
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = writer.Write([]byte("event: " + event + "\ndata: " + string(data) + "\n\n"))
		return err
	}
	go func() {
		defer cancel()
		_, err := run(runCtx)
		if err != nil && !t.begun {
			started <- err
		} else if err != nil {
			code, message := publicFailure(err)
			_ = t.fail(code, message)
		}
		_ = writer.Close()
	}()
	if err := <-started; err != nil {
		return failureResponse(operation, err)
	}
	var stream io.ReadCloser = &streamBody{PipeReader: reader, cancel: cancel}
	if operation == conversation.OperationChat || operation == conversation.OperationMessages {
		stream = conversation.ConvertResponseStreamWithOptions(stream, operation, options)
	}
	header := http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: stream}, nil
}

// streamBody stops the upstream reply when the client side closes the stream.
type streamBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *streamBody) Close() error {
	b.cancel()
	return b.PipeReader.Close()
}

// converse runs the reply on the account's egress: a continuation of the
// account's conversation when it already holds the start of this history,
// else the whole history. It returns where a follow-up can continue.
func (a *Adapter) converse(ctx context.Context, credential account.Credential, c credentials, model string, in turnRequest, seed string, t *turn, m *meter) (chatCursor, error) {
	var from chatCursor
	var fromKey string
	at, resume := 0, false
	if seed != "" {
		from, fromKey, at, resume = a.conversations.find(historyKeys(seed, in.Messages), len(in.Messages), credential.ID, time.Now())
	}
	var next chatCursor
	_, err := a.attempt(ctx, credential, nil, func(ctx context.Context) error {
		w, err := wire(ctx, c)
		if err != nil {
			return err
		}
		if resume {
			err = a.reply(ctx, w, model, in, at, from, t, m, &next)
			if err == nil || m.used > 0 || !continuationRejected(err) {
				return err
			}
			// ChatGPT no longer continues it (expired or unknown): start over.
			a.conversations.drop(fromKey)
			resume = false
		}
		return a.reply(ctx, w, model, in, 0, chatCursor{}, t, m, &next)
	})
	return next, err
}

// reply streams one answer into the turn under the output limit: of the
// messages from at, continuing from when it is set.
func (a *Adapter) reply(ctx context.Context, w wireAccount, model string, in turnRequest, at int, from chatCursor, t *turn, m *meter, next *chatCursor) error {
	sent := in
	sent.Messages = in.Messages[at:]
	request, images, err := sent.wire(model, func(value string) (OpenAIImageInput, error) { return a.loadInput(ctx, value) })
	if err != nil {
		return err
	}
	request.ConversationID, request.ParentMessageID = from.conversation, from.parent
	// Each attempt filters its own answer: tool calls are sifted out first,
	// so their arguments keep any markup, then citations become Markdown
	// links and the remaining Web UI markup is rewritten.
	var sieve *toolemu.Sieve
	if in.Emulated.Enabled() {
		sieve = toolemu.NewSieve(in.Emulated.Available)
	}
	ui, cites := &richUIFilter{}, &citer{}
	err = NewOpenAIChat(a.image).Stream(ctx, w, request, func(event OpenAIChatEvent) error {
		if event.Done {
			next.conversation, next.parent = event.Conversation, event.Parent
		}
		reasoning, err := m.take(event.Thinking, &m.reasoning)
		if err != nil {
			return err
		}
		// The limit and usage count everything ChatGPT wrote, tool markup included.
		text, err := m.take(event.Text, &m.text)
		if err != nil {
			return err
		}
		// A cut reply drops what the filters hold instead of leaking partial markup.
		flush := event.Done && !m.truncated
		if sieve != nil {
			var calls []toolemu.Call
			text, calls = sift(sieve, text, flush)
			t.calls = append(t.calls, calls...)
		}
		text, cited := cites.Feed(text, event.Search)
		if flush {
			text += cites.Flush()
		}
		text = ui.Feed(text)
		if flush {
			text += ui.Flush()
		}
		t.search, t.cited = event.Search, append(t.cited, cited...)
		if err := t.add(true, reasoning); err != nil {
			return err
		}
		if err := t.add(false, text); err != nil {
			return err
		}
		if m.truncated {
			return errOutputLimit
		}
		return nil
	}, images...)
	if errors.Is(err, errOutputLimit) {
		return nil
	}
	return err
}

// remember records where the account's conversation now ends, keyed by the
// history plus this reply as the client will send them back.
func (a *Adapter) remember(seed string, history []turnMessage, t *turn, account uint64, next chatCursor) {
	if seed == "" || next.conversation == "" || next.parent == "" {
		return
	}
	messages := append(append([]turnMessage(nil), history...), t.echo()...)
	keys := historyKeys(seed, messages)
	if len(keys) == 0 || keys[len(keys)-1].end != len(messages) {
		return
	}
	next.account = account
	a.conversations.save(keys[len(keys)-1].key, next, time.Now())
}

// continuationRejected reports a failure of the conversation itself (gone,
// unknown parent), which sending the whole history avoids.
func continuationRejected(err error) bool {
	var upstream *upstreamError
	if !errors.As(err, &upstream) {
		return false
	}
	switch upstream.Status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusGone, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// sift moves complete tool call blocks out of text. Blocks with no declared
// function stay as text, like any other answer text.
func sift(sieve *toolemu.Sieve, text string, flush bool) (string, []toolemu.Call) {
	var out strings.Builder
	var calls []toolemu.Call
	for result := sieve.Feed(text); ; result = sieve.Feed("") {
		out.WriteString(result.SafeText)
		calls = append(calls, result.Calls...)
		if !result.Complete {
			break
		}
		if len(result.Calls) == 0 {
			out.WriteString(result.Raw)
		}
	}
	if flush {
		result := sieve.Flush()
		out.WriteString(result.SafeText)
		calls = append(calls, result.Calls...)
	}
	return out.String(), calls
}

// wire maps the turn to the Web conversation request; images are loaded here
// because a URL input is fetched through the request's egress.
func (in turnRequest) wire(model string, load func(string) (OpenAIImageInput, error)) (wireChatRequest, []OpenAIChatImage, error) {
	request := wireChatRequest{Model: model, ReasoningEffort: in.ReasoningEffort, Search: in.Search}
	var images []OpenAIChatImage
	for mi, message := range in.Messages {
		parts := []any{}
		for pi, part := range message.Parts {
			if part.Image == "" {
				parts = append(parts, map[string]any{"type": "text", "text": part.Text})
				continue
			}
			input, err := load(part.Image)
			if err != nil {
				return request, nil, err
			}
			images = append(images, OpenAIChatImage{MessageIndex: mi, PartIndex: pi, Input: input})
			parts = append(parts, map[string]any{"type": "image_url"})
		}
		request.Messages = append(request.Messages, wireMessage{Role: message.Role, Content: parts})
	}
	return request, images, nil
}

// parseTurn reads a Responses request leniently: sampling, caching and other
// options the Web conversation cannot honour are ignored.
func parseTurn(body []byte) (turnRequest, error) {
	in := turnRequest{ParallelToolCalls: true}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		return in, errors.New("request must be a JSON object")
	}
	set := func(key string) bool { v, ok := raw[key]; return ok && string(v) != "null" }
	effort, err := reasoningEffort(raw)
	if err != nil {
		return in, err
	}
	in.ReasoningEffort = effort
	if set("max_output_tokens") {
		if json.Unmarshal(raw["max_output_tokens"], &in.MaxOutputTokens) != nil || in.MaxOutputTokens < 1 {
			return in, errors.New("max_output_tokens must be a positive integer")
		}
	}
	if set("tools") && json.Unmarshal(raw["tools"], &in.Tools) != nil {
		return in, errors.New("tools must be an array")
	}
	if set("tool_choice") {
		in.ToolChoice = raw["tool_choice"]
	}
	if set("parallel_tool_calls") && json.Unmarshal(raw["parallel_tool_calls"], &in.ParallelToolCalls) != nil {
		return in, errors.New("parallel_tool_calls must be boolean")
	}
	if in.Emulated, err = toolemu.ParseConfig(raw["tools"], raw["tool_choice"]); err != nil {
		return in, err
	}
	tools := in.Emulated
	in.Search = tools.ForcedWebSearch || (tools.HostedWebSearch && tools.Choice == "required" && len(tools.Functions) == 0)
	notice := plainMarkdown
	if rules := toolNotice(tools, in.ParallelToolCalls); rules != "" {
		notice += "\n\n" + rules
	}
	in.Messages = append(in.Messages, textMessage("system", notice))
	if set("instructions") {
		var instructions string
		if json.Unmarshal(raw["instructions"], &instructions) != nil {
			return in, errors.New("instructions must be text")
		}
		if instructions != "" {
			in.Messages = append(in.Messages, textMessage("system", instructions))
		}
	}
	var text string
	var items []json.RawMessage
	if !set("input") {
		return in, errors.New("input is required")
	} else if raw["input"][0] == '"' && json.Unmarshal(raw["input"], &text) == nil {
		in.Messages = append(in.Messages, textMessage("user", text))
	} else if json.Unmarshal(raw["input"], &items) != nil {
		return in, errors.New("input must be text or an array of items")
	}
	images := 0
	for _, item := range items {
		message, ok, err := decodeItem(item, tools.Enabled())
		if err != nil {
			return in, err
		}
		if ok {
			for _, part := range message.Parts {
				if part.Image != "" {
					images++
				}
			}
			// Calls of one turn share a <tool_calls> block, as the model writes them.
			if last := len(in.Messages) - 1; message.calls != nil && last >= 0 && in.Messages[last].calls != nil {
				message.calls = append(in.Messages[last].calls, message.calls...)
				in.Messages = in.Messages[:last]
			}
			if message.calls != nil {
				message.Parts = []turnPart{{Text: toolemu.FormatCalls(message.calls)}}
			}
			in.Messages = append(in.Messages, message)
		}
	}
	if len(in.Messages) == 0 || len(in.Messages) > 1024 || images > 8 {
		return in, errors.New("invalid message or image count")
	}
	return in, nil
}

// toolNotice is the system text for the declared tools: the toolemu call
// convention when functions may be called, toolsUnavailable (onlyWebSearch
// beside web search) when a declared tool cannot run, nothing for web search
// alone (ChatGPT runs it).
func toolNotice(tools toolemu.Config, parallel bool) string {
	if !tools.Enabled() {
		switch {
		case len(tools.Functions) == 0 && len(tools.Unsupported) == 0:
			return ""
		case tools.HostedWebSearch && tools.Choice != "none":
			return onlyWebSearch
		}
		return toolsUnavailable
	}
	prompt := toolemu.Prompt(tools)
	if !parallel {
		prompt += "\nCall at most one tool per reply."
	}
	if len(tools.Unsupported) > 0 && tools.HostedWebSearch {
		prompt += "\nOnly the tools listed above and web search can be used; any other tool is unavailable in this session."
	} else if len(tools.Unsupported) > 0 {
		prompt += "\nOnly the tools listed above can be called; any other tool is unavailable in this session."
	}
	prompt += "\n" + clientTools
	if !tools.HostedWebSearch {
		prompt += "\n" + noBuiltinTools
	}
	return prompt
}

// reasoningEffort reads thinking_effort, then reasoning_effort, then
// reasoning.effort, and normalizes the value for the Web conversation.
func reasoningEffort(raw map[string]json.RawMessage) (string, error) {
	value, ok := raw["thinking_effort"]
	if !ok {
		value, ok = raw["reasoning_effort"]
	}
	if !ok {
		var reasoning struct {
			Effort json.RawMessage `json:"effort"`
		}
		if v, found := raw["reasoning"]; found && string(v) != "null" && json.Unmarshal(v, &reasoning) != nil {
			return "", errors.New("reasoning must be an object")
		}
		value = reasoning.Effort
	}
	if len(value) == 0 || string(value) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(value, &s) != nil {
		return "", errors.New("reasoning effort must be text")
	}
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "", "none", "minimal":
		return "", nil
	case "low", "medium", "high":
		return v, nil
	case "xhigh", "extended":
		return "extended", nil
	}
	return "", fmt.Errorf("unsupported reasoning effort: %s", s)
}

// decodeItem converts one Responses input item. Function calls stay calls
// when emulate is set; other tool history is flattened to text and other
// non-message items are dropped.
func decodeItem(raw json.RawMessage, emulate bool) (turnMessage, bool, error) {
	var v struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Input     string          `json:"input"`
		Output    json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return turnMessage{}, false, errors.New("invalid input item")
	}
	switch v.Type {
	case "function_call", "custom_tool_call":
		if emulate && v.Type == "function_call" {
			return turnMessage{Role: "assistant", calls: []toolemu.Call{{ID: v.CallID, Name: v.Name, Arguments: v.Arguments}}}, true, nil
		}
		return textMessage("assistant", fmt.Sprintf("Tool call %s (%s): %s", v.Name, v.CallID, v.Arguments+v.Input)), true, nil
	case "function_call_output", "custom_tool_call_output":
		parts, err := decodeContent(v.Output)
		if err != nil {
			parts = []turnPart{{Text: string(v.Output)}}
		}
		return turnMessage{Role: "user", Parts: append([]turnPart{{Text: fmt.Sprintf("Tool result (%s):", v.CallID)}}, parts...)}, true, nil
	case "", "message":
	default:
		return turnMessage{}, false, nil
	}
	out := turnMessage{Role: v.Role}
	switch v.Role {
	case "developer":
		out.Role = "system"
	case "system", "user", "assistant":
	default:
		return out, false, fmt.Errorf("unsupported message role: %s", v.Role)
	}
	parts, err := decodeContent(v.Content)
	if err != nil {
		return out, false, err
	}
	out.Parts = parts
	return out, len(parts) > 0, nil
}

// decodeContent reads string content or text and image parts.
func decodeContent(raw json.RawMessage) ([]turnPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// Decoded values carry no leading space; checking the first byte spares
	// an array a failed string decode that scans it twice.
	var text string
	if raw[0] == '"' && json.Unmarshal(raw, &text) == nil {
		return []turnPart{{Text: text}}, nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Refusal  string          `json:"refusal"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return nil, errors.New("invalid message content")
	}
	out := []turnPart{}
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			out = append(out, turnPart{Text: p.Text})
		case "refusal":
			out = append(out, turnPart{Text: p.Refusal})
		case "image_url", "input_image":
			var u string
			if json.Unmarshal(p.ImageURL, &u) != nil {
				var image struct {
					URL string `json:"url"`
				}
				_ = json.Unmarshal(p.ImageURL, &image)
				u = image.URL
			}
			if u == "" {
				return nil, errors.New("image_url is required")
			}
			out = append(out, turnPart{Image: u})
		default:
			return nil, fmt.Errorf("unsupported content part: %s", p.Type)
		}
	}
	return out, nil
}

func textMessage(role, text string) turnMessage {
	return turnMessage{Role: role, Parts: []turnPart{{Text: text}}}
}

var o200k = sync.OnceValues(func() (tokenizer.Codec, error) { return tokenizer.Get(tokenizer.O200kBase) })

// countTokens is safe for concurrent use: the codec only reads its vocabulary.
func countTokens(text string) (int64, error) {
	if text == "" {
		return 0, nil
	}
	codec, err := o200k()
	if err != nil {
		return 0, err
	}
	n, err := codec.Count(text)
	return int64(n), err
}

// estimateInput counts 3 priming tokens, 4 per message, the text and a fixed
// estimate per image.
func estimateInput(messages []turnMessage, vision int) (int64, error) {
	n := int64(3)
	for _, message := range messages {
		n += 4
		for _, part := range message.Parts {
			if part.Image != "" {
				n += int64(vision)
				continue
			}
			v, err := countTokens(part.Text)
			if err != nil {
				return 0, err
			}
			n += v
		}
	}
	return n, nil
}

// meter enforces the output limit on estimated tokens. Deltas are counted on
// their own, which never undercounts the joined text and keeps a long stream
// linear; usage is one exact count of the final output.
type meter struct {
	limit, used     int64
	truncated       bool
	text, reasoning strings.Builder
}

func (m *meter) take(part string, into *strings.Builder) (string, error) {
	if part == "" || m.truncated {
		return "", nil
	}
	n, err := countTokens(part)
	if err != nil {
		return "", err
	}
	if m.used+n > m.limit {
		m.truncated = true
		if part, n, err = fit(part, m.limit-m.used); err != nil {
			return "", err
		}
	}
	m.used += n
	into.WriteString(part)
	return part, nil
}

// fit returns the longest rune prefix of text whose estimate stays within budget.
func fit(text string, budget int64) (string, int64, error) {
	runes := []rune(text)
	lo, hi, used := 0, len(runes), int64(0)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		n, err := countTokens(string(runes[:mid]))
		if err != nil {
			return "", 0, err
		}
		if n <= budget {
			lo, used = mid, n
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]), used, nil
}

func (m *meter) usage(input int64) (map[string]any, error) {
	text, err := countTokens(m.text.String())
	if err != nil {
		return nil, err
	}
	reasoning, err := countTokens(m.reasoning.String())
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"input_tokens": input, "input_tokens_details": map[string]any{"cached_tokens": 0},
		"output_tokens": text + reasoning, "output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens": input + text + reasoning,
	}, nil
}

// turn renders one reply as an OpenAI Responses object or, when send is set,
// as the official event sequence with a monotonic sequence_number.
type turn struct {
	id, model string
	created   int64
	request   turnRequest
	send      func(string, map[string]any) error
	begun     bool
	sequence  int
	blocks    []*block
	current   *block
	held      string
	output    []any
	status    string
	// blank is whitespace that would open the message; it waits for text.
	blank string
	// calls are the emulated function calls, emitted after the text.
	calls []toolemu.Call
	// search and cited come from ChatGPT's own web search.
	search    *chatSearch
	cited     []citation
	webSearch map[string]any
}

// block is one output item: a reasoning summary or an assistant message.
type block struct {
	index     int
	reasoning bool
	id        string
	text      strings.Builder
}

func (t *turn) object(status string) map[string]any {
	var effort any
	if t.request.ReasoningEffort != "" {
		effort = t.request.ReasoningEffort
	}
	tools := []json.RawMessage{}
	if t.request.Tools != nil {
		tools = t.request.Tools
	}
	var choice any = "auto"
	if t.request.ToolChoice != nil {
		choice = t.request.ToolChoice
	}
	output := append([]any{}, t.output...)
	return map[string]any{
		"id": t.id, "object": "response", "created_at": t.created, "status": status, "background": false,
		"error": nil, "incomplete_details": nil, "metadata": map[string]any{}, "model": t.model, "output": output,
		"parallel_tool_calls": t.request.ParallelToolCalls, "previous_response_id": nil, "reasoning": map[string]any{"effort": effort, "summary": nil},
		"store": false, "text": map[string]any{"format": map[string]any{"type": "text"}}, "tool_choice": choice, "tools": tools, "usage": nil,
	}
}

func (t *turn) emit(name string, value map[string]any) error {
	if t.send == nil {
		return nil
	}
	value["type"], value["sequence_number"] = name, t.sequence
	t.sequence++
	return t.send(name, value)
}

func (t *turn) begin() error {
	if t.sequence > 0 {
		return nil
	}
	if err := t.emit("response.created", map[string]any{"response": t.object("in_progress")}); err != nil {
		return err
	}
	return t.emit("response.in_progress", map[string]any{"response": t.object("in_progress")})
}

// add orders interleaved reasoning and text into sequential output items.
// Reasoning before any text streams live as the first item; reasoning that
// arrives while text is open is held and emitted after the text closes.
// Whitespace that would open the message waits for real text, so the
// newlines around a tool call block do not make a blank message.
func (t *turn) add(reasoning bool, text string) error {
	if text == "" {
		return nil
	}
	if !reasoning && (t.current == nil || t.current.reasoning) {
		if strings.TrimSpace(text) == "" {
			t.blank += text
			return nil
		}
		text, t.blank = t.blank+text, ""
	}
	return t.write(reasoning, text)
}

func (t *turn) write(reasoning bool, text string) error {
	if reasoning && t.current != nil && !t.current.reasoning {
		t.held += text
		return nil
	}
	if t.current == nil || t.current.reasoning != reasoning {
		if err := t.closeCurrent(); err != nil {
			return err
		}
		if err := t.open(reasoning); err != nil {
			return err
		}
	}
	t.current.text.WriteString(text)
	if reasoning {
		return t.emit("response.reasoning_summary_text.delta", map[string]any{"item_id": t.current.id, "output_index": t.current.index, "summary_index": 0, "delta": text})
	}
	return t.emit("response.output_text.delta", map[string]any{"item_id": t.current.id, "output_index": t.current.index, "content_index": 0, "delta": text, "logprobs": []any{}})
}

func (t *turn) open(reasoning bool) error {
	if err := t.begin(); err != nil {
		return err
	}
	if !reasoning {
		if err := t.searched(); err != nil {
			return err
		}
	}
	b := &block{index: len(t.output), reasoning: reasoning}
	t.blocks = append(t.blocks, b)
	t.current = b
	if reasoning {
		b.id = "rs_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if err := t.emit("response.output_item.added", map[string]any{"output_index": b.index, "item": map[string]any{"id": b.id, "type": "reasoning", "summary": []any{}}}); err != nil {
			return err
		}
		return t.emit("response.reasoning_summary_part.added", map[string]any{"item_id": b.id, "output_index": b.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
	}
	b.id = "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := t.emit("response.output_item.added", map[string]any{"output_index": b.index, "item": map[string]any{"id": b.id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}); err != nil {
		return err
	}
	return t.emit("response.content_part.added", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
}

func (t *turn) closeCurrent() error {
	b := t.current
	if b == nil {
		return nil
	}
	t.current = nil
	status := t.status
	if status == "" {
		status = "completed"
	}
	text := b.text.String()
	if b.reasoning {
		item := map[string]any{"id": b.id, "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": text}}}
		t.output = append(t.output, item)
		if err := t.emit("response.reasoning_summary_text.done", map[string]any{"item_id": b.id, "output_index": b.index, "summary_index": 0, "text": text}); err != nil {
			return err
		}
		if err := t.emit("response.reasoning_summary_part.done", map[string]any{"item_id": b.id, "output_index": b.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text}}); err != nil {
			return err
		}
		return t.emit("response.output_item.done", map[string]any{"output_index": b.index, "item": item})
	}
	annotations := annotate(text, t.cited)
	part := map[string]any{"type": "output_text", "text": text, "annotations": annotations}
	item := map[string]any{"id": b.id, "type": "message", "status": status, "role": "assistant", "content": []any{part}}
	t.output = append(t.output, item)
	for i, annotation := range annotations {
		if err := t.emit("response.output_text.annotation.added", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "annotation_index": i, "annotation": annotation}); err != nil {
			return err
		}
	}
	if err := t.emit("response.output_text.done", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "text": text, "logprobs": []any{}}); err != nil {
		return err
	}
	if err := t.emit("response.content_part.done", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "part": part}); err != nil {
		return err
	}
	return t.emit("response.output_item.done", map[string]any{"output_index": b.index, "item": item})
}

// searched emits ChatGPT's web search once as a completed web_search_call:
// before the answer when the search is known by then, else after it.
func (t *turn) searched() error {
	if t.webSearch != nil || !t.search.used() {
		return nil
	}
	if err := t.begin(); err != nil {
		return err
	}
	id, index := "ws_"+strings.ReplaceAll(uuid.NewString(), "-", ""), len(t.output)
	t.webSearch = map[string]any{"id": id, "type": "web_search_call", "status": "completed", "action": t.search.action()}
	t.output = append(t.output, t.webSearch)
	// The added item already carries the action so Messages clients see the query.
	if err := t.emit("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"id": id, "type": "web_search_call", "status": "in_progress", "action": t.search.action()}}); err != nil {
		return err
	}
	for _, stage := range []string{"in_progress", "searching", "completed"} {
		if err := t.emit("response.web_search_call."+stage, map[string]any{"item_id": id, "output_index": index}); err != nil {
			return err
		}
	}
	return t.emit("response.output_item.done", map[string]any{"output_index": index, "item": t.webSearch})
}

// call emits one emulated function call as a completed function_call item.
func (t *turn) call(call toolemu.Call) error {
	id, index := "fc_"+strings.ReplaceAll(uuid.NewString(), "-", ""), len(t.output)
	item := map[string]any{"id": id, "type": "function_call", "status": "completed", "call_id": call.ID, "name": call.Name, "arguments": call.Arguments}
	t.output = append(t.output, item)
	if err := t.emit("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"id": id, "type": "function_call", "status": "in_progress", "call_id": call.ID, "name": call.Name, "arguments": ""}}); err != nil {
		return err
	}
	if err := t.emit("response.function_call_arguments.delta", map[string]any{"item_id": id, "output_index": index, "delta": call.Arguments}); err != nil {
		return err
	}
	if err := t.emit("response.function_call_arguments.done", map[string]any{"item_id": id, "output_index": index, "arguments": call.Arguments}); err != nil {
		return err
	}
	return t.emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
}

// finish closes the open item, emits held reasoning, makes sure the output
// has a message (even an empty one) unless it calls functions, emits the
// search and the calls and returns the final object.
func (t *turn) finish(usage map[string]any, truncated bool) (map[string]any, error) {
	t.status = "completed"
	if truncated {
		t.status = "incomplete"
	}
	if err := t.begin(); err != nil {
		return nil, err
	}
	// Whitespace alone is the answer only when there are no calls.
	if t.blank != "" && len(t.calls) == 0 {
		if err := t.write(false, t.blank); err != nil {
			return nil, err
		}
	}
	if err := t.closeCurrent(); err != nil {
		return nil, err
	}
	hasText := false
	for _, b := range t.blocks {
		hasText = hasText || !b.reasoning
	}
	if t.held != "" {
		held := t.held
		t.held = ""
		if err := t.add(true, held); err != nil {
			return nil, err
		}
	} else if !hasText && len(t.calls) == 0 {
		if err := t.open(false); err != nil {
			return nil, err
		}
	}
	if err := t.closeCurrent(); err != nil {
		return nil, err
	}
	if err := t.searched(); err != nil {
		return nil, err
	}
	if t.webSearch != nil {
		// The final object carries every source seen, also those after the item.
		t.webSearch["action"] = t.search.action()
	}
	calls := t.calls
	if !t.request.ParallelToolCalls && len(calls) > 1 {
		calls = calls[:1]
	}
	for _, call := range calls {
		if err := t.call(call); err != nil {
			return nil, err
		}
	}
	final := t.object(t.status)
	if truncated {
		final["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	final["usage"] = usage
	return final, t.emit("response."+t.status, map[string]any{"response": final})
}

// fail ends a started stream; the client already received part of the reply.
func (t *turn) fail(code, message string) error {
	failed := t.object("failed")
	failed["error"] = map[string]any{"code": code, "message": message}
	return t.emit("response.failed", map[string]any{"response": failed})
}

// failureResponse maps a failure before any output to the upstream status,
// so the gateway's refresh, egress and quota handling apply. Transport and
// protocol failures stay errors.
func failureResponse(operation string, err error) (*provider.Response, error) {
	var request *requestError
	var upstream *upstreamError
	switch {
	case errors.As(err, &request):
		return errorResponse(operation, http.StatusBadRequest, "invalid_request", request.message), nil
	case !errors.As(err, &upstream) || upstream.Status < 400 || upstream.Status > 599:
		return nil, err
	case upstream.Challenge:
		return errorResponse(operation, http.StatusForbidden, "cloudflare_challenge", "ChatGPT egress was challenged by Cloudflare"), nil
	case IsModelRejected(err):
		response := errorResponse(operation, http.StatusNotFound, "model_not_found", "ChatGPT rejected the model for this account")
		response.ModelCatalogChanged = true
		return response, nil
	case upstream.Status == http.StatusRequestEntityTooLarge:
		// ChatGPT Web takes about 120 KB per request.
		return errorResponse(operation, http.StatusBadRequest, "context_length_exceeded", "The conversation is too long for one ChatGPT Web request (about 120 KB); shorten or compact the history"), nil
	}
	code, message := "upstream_error", "ChatGPT request failed"
	switch upstream.Status {
	case http.StatusBadRequest:
		code, message = "upstream_rejected_request", "ChatGPT rejected the request"
		if detail := []rune(strings.TrimSpace(upstream.Message)); len(detail) > 0 && len(detail) <= 300 && !strings.HasPrefix(string(detail), "{") {
			message += ": " + string(detail)
		}
	case http.StatusUnauthorized:
		code, message = "invalid_credential", "ChatGPT rejected the account credential"
	case http.StatusForbidden:
		code, message = "upstream_forbidden", "ChatGPT denied the request"
	case http.StatusNotFound:
		code, message = "not_found", "ChatGPT resource not found"
	case http.StatusTooManyRequests:
		code, message = "rate_limit_exceeded", "ChatGPT rate limit reached; retry later"
	}
	response := errorResponse(operation, upstream.Status, code, message)
	if upstream.RetryAfter != "" {
		response.Header.Set("Retry-After", upstream.RetryAfter)
	}
	return response, nil
}

// publicFailure describes a failure after output started without upstream text.
func publicFailure(err error) (string, string) {
	var upstream *upstreamError
	var transport *transportError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "upstream_timeout", "ChatGPT reply timed out"
	case errors.As(err, &upstream) && upstream.Status == http.StatusTooManyRequests:
		return "rate_limit_exceeded", "ChatGPT rate limit reached"
	case errors.As(err, &transport):
		return "upstream_transport_error", "Connection to ChatGPT failed"
	}
	return "upstream_error", "ChatGPT reply failed"
}

// errorResponse renders an OpenAI error, or an Anthropic one for Messages.
func errorResponse(operation string, status int, code, message string) *provider.Response {
	openAIType, anthropicType := "invalid_request_error", "invalid_request_error"
	switch {
	case status >= 500:
		openAIType, anthropicType = "server_error", "api_error"
	case status == http.StatusUnauthorized:
		openAIType, anthropicType = "authentication_error", "authentication_error"
	case status == http.StatusForbidden:
		openAIType, anthropicType = "permission_error", "permission_error"
	case status == http.StatusNotFound:
		anthropicType = "not_found_error"
	case status == http.StatusTooManyRequests:
		openAIType, anthropicType = "rate_limit_error", "rate_limit_error"
	}
	body := map[string]any{"error": map[string]any{"type": openAIType, "code": code, "message": message, "param": nil}}
	if operation == conversation.OperationMessages {
		body = map[string]any{"type": "error", "error": map[string]any{"type": anthropicType, "message": message}}
	}
	data, _ := json.Marshal(body)
	return jsonResponse(status, data)
}

func jsonResponse(status int, data []byte) *provider.Response {
	header := http.Header{"Content-Type": {"application/json"}}
	return &provider.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: header, Body: io.NopCloser(bytes.NewReader(data))}
}
