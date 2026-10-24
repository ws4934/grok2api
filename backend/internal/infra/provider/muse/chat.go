package muse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tiktoken-go/tokenizer"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

type responsesRequest struct {
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions"`
	Stream       bool            `json:"stream"`
}

type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// ForwardResponse serves Responses; Chat Completions and Messages are
// converted to Responses and back. Tools are not offered: the Muse agent does
// not emit function calls.
func (a *Adapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	operation := request.Operation
	if request.Method != http.MethodPost || request.Path != "/responses" {
		return errorResponse(operation, http.StatusBadRequest, "unsupported_endpoint", "Muse 只支持 POST /responses"), nil
	}
	if request.Model != modelChat {
		return errorResponse(operation, http.StatusBadRequest, "invalid_model", "该 Muse 模型不支持文本对话"), nil
	}
	body := request.Body
	var options conversation.ResponseOptions
	if request.NormalizeBody && (operation == conversation.OperationChat || operation == conversation.OperationMessages) {
		var err error
		if body, options, err = conversation.ConvertRequestWithOptions(body, request.Model, operation); err != nil {
			return errorResponse(operation, http.StatusBadRequest, "invalid_request", err.Error()), nil
		}
	}
	options.OpenAIUsage = true
	var in responsesRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return errorResponse(operation, http.StatusBadRequest, "invalid_request", "请求 JSON 无效"), nil
	}
	prompt, imageRefs, err := buildChatPrompt(in)
	if err != nil {
		return failureResponse(operation, err)
	}
	turn := chatTurn{Prompt: prompt}
	for _, ref := range imageRefs {
		input, err := a.readInput(ctx, ref)
		if err != nil {
			return failureResponse(operation, err)
		}
		turn.Images = append(turn.Images, input)
	}
	reply := newReply(request.Model, prompt)
	if !request.Streaming && !in.Stream {
		var final string
		err := a.withSession(ctx, request.Credential, func(ctx context.Context, s session) error {
			var err error
			final, err = a.engine.Chat(ctx, s, turn, func(string) error { return nil })
			return err
		})
		if err != nil {
			return failureResponse(operation, err)
		}
		data, err := json.Marshal(reply.object("completed", final))
		if err != nil {
			return nil, err
		}
		if operation != conversation.OperationResponses {
			if data, err = conversation.ConvertResponseJSONWithOptions(data, operation, options); err != nil {
				return nil, err
			}
		}
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	}

	// The stream starts with the first text, so a failure before it (login,
	// delivery, quota) still reaches the gateway as a status and fails over.
	reader, writer := io.Pipe()
	runCtx, cancel := context.WithCancel(ctx)
	started := make(chan error, 1)
	stream := &replyStream{reply: reply, write: func(data []byte) error { _, err := writer.Write(data); return err }}
	begin := func() error {
		if stream.begun {
			return nil
		}
		stream.begun = true
		started <- nil
		return stream.start()
	}
	go func() {
		defer cancel()
		var final string
		err := a.withSession(runCtx, request.Credential, func(ctx context.Context, s session) error {
			var err error
			final, err = a.engine.Chat(ctx, s, turn, func(snapshot string) error {
				delta := stream.pending(snapshot)
				if delta == "" {
					return nil
				}
				if err := begin(); err != nil {
					return err
				}
				return stream.delta(delta)
			})
			return err
		})
		switch {
		case err != nil && !stream.begun:
			started <- err
		case err != nil:
			_ = stream.fail(classify(err))
		default:
			if err := begin(); err == nil {
				if rest := strings.TrimPrefix(final, stream.sent); rest != "" && strings.HasPrefix(final, stream.sent) {
					_ = stream.delta(rest)
				}
				_ = stream.finish(final)
			}
		}
		_ = writer.Close()
	}()
	if err := <-started; err != nil {
		cancel()
		return failureResponse(operation, err)
	}
	var out io.ReadCloser = &streamBody{PipeReader: reader, cancel: cancel}
	if operation != conversation.OperationResponses {
		out = conversation.ConvertResponseStreamWithOptions(out, operation, options)
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}, Body: out}, nil
}

// streamBody stops the browser session when the client side closes the stream.
type streamBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *streamBody) Close() error {
	b.cancel()
	return b.PipeReader.Close()
}

// buildChatPrompt flattens the Responses input into one message: each
// request is a new side chat, so the history travels in the prompt. Images
// of the last user message become attachments; earlier ones are noted.
func buildChatPrompt(in responsesRequest) (string, []string, error) {
	var system []string
	if text := strings.TrimSpace(in.Instructions); text != "" {
		system = append(system, text)
	}
	type turn struct{ role, text string }
	var turns []turn
	var lastImages []string
	var items []inputItem
	var text string
	if json.Unmarshal(in.Input, &text) == nil {
		items = []inputItem{{Type: "message", Role: "user", Content: in.Input}}
	} else if err := json.Unmarshal(in.Input, &items); err != nil {
		return "", nil, badRequest("input 必须是字符串或数组")
	}
	for _, item := range items {
		switch item.Type {
		case "", "message":
			body, images, err := messageContent(item.Content)
			if err != nil {
				return "", nil, err
			}
			switch item.Role {
			case "system", "developer":
				if body != "" {
					system = append(system, body)
				}
			case "user":
				lastImages = images
				if len(images) > 0 && body == "" {
					body = "[image]"
				}
				if body != "" {
					turns = append(turns, turn{"user", body})
				}
			case "assistant":
				if body != "" {
					turns = append(turns, turn{"assistant", body})
				}
			}
		case "function_call":
			turns = append(turns, turn{"assistant", "Called tool " + item.Name + " with " + item.Arguments})
		case "function_call_output":
			if output := outputText(item.Output); output != "" {
				turns = append(turns, turn{"user", "Tool result:\n" + output})
			}
		}
	}
	if len(turns) == 0 || turns[len(turns)-1].role != "user" {
		return "", nil, badRequest("最后一条消息必须来自用户")
	}
	// Earlier user images are only described; the last message's go along.
	if len(turns) == 1 && len(system) == 0 {
		return turns[0].text, lastImages, nil
	}
	var builder strings.Builder
	if len(system) > 0 {
		builder.WriteString("Instructions:\n" + strings.Join(system, "\n\n") + "\n\n")
	}
	for _, value := range turns {
		label := "User"
		if value.role == "assistant" {
			label = "Assistant"
		}
		builder.WriteString(label + ":\n" + value.text + "\n\n")
	}
	builder.WriteString("Reply to the last User message.")
	return builder.String(), lastImages, nil
}

// messageContent returns the text of a message and the image references of
// its input_image parts.
func messageContent(raw json.RawMessage) (string, []string, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text), nil, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, badRequest("消息 content 格式无效")
	}
	var texts, images []string
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			if value := strings.TrimSpace(part.Text); value != "" {
				texts = append(texts, value)
			}
		case "input_image":
			var ref string
			if json.Unmarshal(part.ImageURL, &ref) != nil {
				var object struct {
					URL string `json:"url"`
				}
				_ = json.Unmarshal(part.ImageURL, &object)
				ref = object.URL
			}
			if ref = strings.TrimSpace(ref); ref != "" {
				images = append(images, ref)
			}
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

func outputText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	body, _, _ := messageContent(raw)
	return body
}

// reply is one Responses object built around the final Markdown.
type reply struct {
	id, itemID, model, prompt string
	created                   int64
}

func newReply(model, prompt string) *reply {
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	return &reply{id: "resp_" + id, itemID: "msg_" + id, model: model, prompt: prompt, created: time.Now().Unix()}
}

func (r *reply) message(status, text string) map[string]any {
	content := []any{}
	if status == "completed" {
		content = []any{outputPart(text)}
	}
	return map[string]any{"id": r.itemID, "type": "message", "status": status, "role": "assistant", "content": content}
}

func outputPart(text string) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
}

func (r *reply) object(status, text string) map[string]any {
	value := map[string]any{
		"id": r.id, "object": "response", "created_at": r.created, "status": status, "model": r.model,
		"output": []any{}, "error": nil, "incomplete_details": nil, "parallel_tool_calls": false, "tool_choice": "auto", "tools": []any{},
		"text": map[string]any{"format": map[string]any{"type": "text"}}, "usage": nil, "store": false, "metadata": map[string]any{},
	}
	if status == "completed" {
		input, output := countTokens(r.prompt), countTokens(text)
		value["output"] = []any{r.message("completed", text)}
		value["usage"] = map[string]any{
			"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
			"input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0},
		}
	}
	return value
}

// replyStream writes the official Responses event sequence.
type replyStream struct {
	reply    *reply
	write    func([]byte) error
	sequence int
	begun    bool
	// sent is the text already streamed.
	sent string
}

// pending returns the text of a snapshot that is safe to stream: complete
// paragraphs only, since the page re-renders the tail while it streams. A
// snapshot that no longer extends what was sent waits for the final text.
func (s *replyStream) pending(snapshot string) string {
	cut := strings.LastIndex(snapshot, "\n\n")
	if cut < 0 {
		return ""
	}
	stable := snapshot[:cut+2]
	if len(stable) <= len(s.sent) || !strings.HasPrefix(stable, s.sent) {
		return ""
	}
	return stable[len(s.sent):]
}

func (s *replyStream) emit(name string, value map[string]any) error {
	s.sequence++
	value["type"], value["sequence_number"] = name, s.sequence
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.write([]byte("event: " + name + "\ndata: " + string(data) + "\n\n"))
}

func (s *replyStream) start() error {
	if err := s.emit("response.created", map[string]any{"response": s.reply.object("in_progress", "")}); err != nil {
		return err
	}
	if err := s.emit("response.in_progress", map[string]any{"response": s.reply.object("in_progress", "")}); err != nil {
		return err
	}
	if err := s.emit("response.output_item.added", map[string]any{"output_index": 0, "item": s.reply.message("in_progress", "")}); err != nil {
		return err
	}
	return s.emit("response.content_part.added", map[string]any{"item_id": s.reply.itemID, "output_index": 0, "content_index": 0, "part": outputPart("")})
}

func (s *replyStream) delta(text string) error {
	s.sent += text
	return s.emit("response.output_text.delta", map[string]any{"item_id": s.reply.itemID, "output_index": 0, "content_index": 0, "delta": text})
}

// finish closes the message. The text is what was streamed, so a final
// render that rewrote earlier paragraphs cannot contradict the deltas.
func (s *replyStream) finish(final string) error {
	text := s.sent
	if strings.HasPrefix(final, s.sent) {
		text = final
	}
	base := map[string]any{"item_id": s.reply.itemID, "output_index": 0, "content_index": 0}
	steps := []struct {
		name  string
		value map[string]any
	}{
		{"response.output_text.done", merge(base, map[string]any{"text": text})},
		{"response.content_part.done", merge(base, map[string]any{"part": outputPart(text)})},
		{"response.output_item.done", map[string]any{"output_index": 0, "item": s.reply.message("completed", text)}},
		{"response.completed", map[string]any{"response": s.reply.object("completed", text)}},
	}
	for _, step := range steps {
		if err := s.emit(step.name, step.value); err != nil {
			return err
		}
	}
	return nil
}

func (s *replyStream) fail(err error) error {
	code, message := "upstream_error", "Muse 回复失败"
	if known, ok := err.(*museError); ok {
		code, message = known.code, known.message
	}
	failed := s.reply.object("failed", "")
	failed["error"] = map[string]any{"code": code, "message": message}
	return s.emit("response.failed", map[string]any{"response": failed})
}

func merge(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

var o200k = sync.OnceValues(func() (tokenizer.Codec, error) { return tokenizer.Get(tokenizer.O200kBase) })

// countTokens estimates usage with o200k_base; the upstream reports none.
func countTokens(text string) int {
	codec, err := o200k()
	if err != nil {
		return len(text) / 4
	}
	count, err := codec.Count(text)
	if err != nil {
		return len(text) / 4
	}
	return count
}
