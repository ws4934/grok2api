package web

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/toolemu"
)

const maxDeferredSearchTextBytes = 8 << 20

func applyWebStopSequences(text string, sequences []string) (string, string) {
	matchAt := -1
	matched := ""
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		if index := strings.Index(text, sequence); index >= 0 && (matchAt < 0 || index < matchAt) {
			matchAt = index
			matched = sequence
		}
	}
	if matchAt < 0 {
		return text, ""
	}
	return text[:matchAt], matched
}

func nullableWebString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func webAnthropicToolID(value string) string {
	if strings.HasPrefix(value, "toolu_") {
		return value
	}
	return "toolu_" + value
}

func webMessagesSearchBlocks(id string, parsed parsedChat, options conversation.ResponseOptions) []any {
	use := map[string]any{
		"type": "server_tool_use", "id": id, "name": "web_search",
		"input": map[string]any{"query": options.AnthropicWebSearchQuery},
	}
	hits := make([]any, 0, searchresult.MaxResults)
	seen := make(map[string]struct{}, searchresult.MaxResults)
	for _, source := range parsed.SearchSources {
		if len(hits) >= searchresult.MaxResults {
			break
		}
		rawURL, _ := source["url"].(string)
		rawURL, valid := searchresult.NormalizeURL(rawURL)
		if !valid {
			continue
		}
		if _, exists := seen[rawURL]; exists {
			continue
		}
		seen[rawURL] = struct{}{}
		title, _ := source["title"].(string)
		title = searchresult.NormalizeTitle(title, rawURL)
		hits = append(hits, map[string]any{"type": "web_search_result", "title": title, "url": rawURL})
	}
	var content any = hits
	if parsed.WebSearchTools == 0 && len(hits) == 0 {
		content = map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}
	}
	result := map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": content}
	return []any{use, result}
}

func shouldEmitWebMessagesSearch(parsed parsedChat, options conversation.ResponseOptions) bool {
	return options.AnthropicWebSearch && (options.AnthropicWebSearchRequired || parsed.WebSearchTools > 0 || len(parsed.SearchSources) > 0)
}

func webMessagesSearchRequests(parsed parsedChat) int64 {
	if parsed.WebSearchTools > 0 {
		return parsed.WebSearchTools
	}
	return 1
}

type webMessagesStream struct {
	writer          io.Writer
	responseID      string
	model           string
	inputTokens     int64
	options         conversation.ResponseOptions
	started         bool
	thinkingStarted bool
	thinkingClosed  bool
	thinkingIndex   int
	textStarted     bool
	textClosed      bool
	textIndex       int
	nextIndex       int
	hasTools        bool
	webSearchID     string
	webSearchUse    bool
	pendingText     strings.Builder
	stopSequence    string
	stopFilter      *webStopFilter
}

func newWebMessagesStream(writer io.Writer, responseID, model string, inputTokens int64, options conversation.ResponseOptions) *webMessagesStream {
	return &webMessagesStream{
		writer: writer, responseID: responseID, model: model, inputTokens: inputTokens,
		options: options, stopFilter: newWebStopFilter(options.StopSequences),
	}
}

func (s *webMessagesStream) Start() error {
	if s.started {
		return nil
	}
	s.started = true
	if err := writeSSE(s.writer, "message_start", map[string]any{
		"type": "message_start", "message": map[string]any{
			"id": strings.Replace(s.responseID, "resp_", "msg_", 1), "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": s.inputTokens, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
		},
	}); err != nil {
		return err
	}
	if s.options.AnthropicWebSearchRequired && !s.options.AnthropicThinking {
		return s.startWebSearch()
	}
	return nil
}

func (s *webMessagesStream) Delta(kind, delta string) error {
	if err := s.Start(); err != nil {
		return err
	}
	if s.stopSequence != "" {
		return nil
	}
	if kind == "reasoning" {
		if !s.options.AnthropicThinking {
			return nil
		}
		if !s.thinkingStarted {
			s.thinkingStarted = true
			s.thinkingIndex = s.nextIndex
			s.nextIndex++
			if err := writeSSE(s.writer, "content_block_start", map[string]any{
				"type": "content_block_start", "index": s.thinkingIndex,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}); err != nil {
				return err
			}
		}
		return writeSSE(s.writer, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.thinkingIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
		})
	}
	if kind != "text" {
		return nil
	}
	if s.options.AnthropicWebSearch {
		if err := s.closeThinking(); err != nil {
			return err
		}
		if s.options.AnthropicWebSearchRequired {
			if err := s.startWebSearch(); err != nil {
				return err
			}
		}
		return s.bufferSearchText(delta)
	}
	if err := s.startText(); err != nil {
		return err
	}
	emit, matched := s.stopFilter.Push(delta)
	if matched != "" {
		s.stopSequence = matched
	}
	if emit == "" {
		return nil
	}
	return s.writeTextDelta(emit)
}

func (s *webMessagesStream) bufferSearchText(delta string) error {
	pending := s.pendingText.Len()
	if pending >= maxDeferredSearchTextBytes || len(delta) > maxDeferredSearchTextBytes-pending {
		return fmt.Errorf("WebSearch 延迟文本缓冲超过 %d MiB", maxDeferredSearchTextBytes>>20)
	}
	s.pendingText.WriteString(delta)
	return nil
}

func (s *webMessagesStream) startText() error {
	if s.textStarted && !s.textClosed {
		return nil
	}
	if err := s.closeThinking(); err != nil {
		return err
	}
	s.textStarted = true
	s.textClosed = false
	s.textIndex = s.nextIndex
	s.nextIndex++
	return writeSSE(s.writer, "content_block_start", map[string]any{
		"type": "content_block_start", "index": s.textIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *webMessagesStream) writeTextDelta(delta string) error {
	if delta == "" {
		return nil
	}
	return writeSSE(s.writer, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.textIndex,
		"delta": map[string]any{"type": "text_delta", "text": delta},
	})
}

func (s *webMessagesStream) Tools(calls []toolemu.Call) error {
	if err := s.Start(); err != nil {
		return err
	}
	if err := s.closeThinking(); err != nil {
		return err
	}
	if err := s.closeText(); err != nil {
		return err
	}
	for _, call := range calls {
		index := s.nextIndex
		s.nextIndex++
		id := webAnthropicToolID(call.ID)
		if err := writeSSE(s.writer, "content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": call.Name, "input": map[string]any{}},
		}); err != nil {
			return err
		}
		if err := writeSSE(s.writer, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Arguments},
		}); err != nil {
			return err
		}
		if err := writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
			return err
		}
		s.hasTools = true
	}
	return nil
}

func (s *webMessagesStream) startWebSearch() error {
	if s.webSearchUse {
		return nil
	}
	s.webSearchUse = true
	if s.webSearchID == "" {
		s.webSearchID = newWebID("srvtoolu")
	}
	index := s.nextIndex
	s.nextIndex++
	if err := writeSSE(s.writer, "content_block_start", map[string]any{
		"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "server_tool_use", "id": s.webSearchID, "name": "web_search", "input": map[string]any{}},
	}); err != nil {
		return err
	}
	if query := s.options.AnthropicWebSearchQuery; query != "" {
		encoded, _ := json.Marshal(map[string]string{"query": query})
		if err := writeSSE(s.writer, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(encoded)},
		}); err != nil {
			return err
		}
	}
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (s *webMessagesStream) finishWebSearch(parsed parsedChat) error {
	if !shouldEmitWebMessagesSearch(parsed, s.options) {
		return nil
	}
	if err := s.startWebSearch(); err != nil {
		return err
	}
	blocks := webMessagesSearchBlocks(s.webSearchID, parsed, s.options)
	result := blocks[1]
	index := s.nextIndex
	s.nextIndex++
	if err := writeSSE(s.writer, "content_block_start", map[string]any{
		"type": "content_block_start", "index": index, "content_block": result,
	}); err != nil {
		return err
	}
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (s *webMessagesStream) Finish(parsed parsedChat, payload map[string]any) error {
	if err := s.Start(); err != nil {
		return err
	}
	if err := s.closeThinking(); err != nil {
		return err
	}
	if err := s.finishWebSearch(parsed); err != nil {
		return err
	}
	if s.options.AnthropicWebSearch && s.pendingText.Len() > 0 {
		if err := s.startText(); err != nil {
			return err
		}
		emit, matched := s.stopFilter.Push(s.pendingText.String())
		if matched != "" {
			s.stopSequence = matched
		}
		if err := s.writeTextDelta(emit); err != nil {
			return err
		}
	}
	if s.stopSequence == "" {
		if pending := s.stopFilter.Flush(); pending != "" {
			if err := s.startText(); err != nil {
				return err
			}
			if err := s.writeTextDelta(pending); err != nil {
				return err
			}
		}
	}
	if err := s.closeText(); err != nil {
		return err
	}
	stopReason := "end_turn"
	if s.hasTools || len(parsed.ToolCalls) > 0 {
		stopReason = "tool_use"
	} else if s.stopSequence != "" {
		stopReason = "stop_sequence"
	}
	usage, _ := payload["usage"].(map[string]any)
	finalUsage := map[string]any{"output_tokens": usage["output_tokens"]}
	if shouldEmitWebMessagesSearch(parsed, s.options) {
		finalUsage["server_tool_use"] = map[string]any{"web_search_requests": webMessagesSearchRequests(parsed)}
	}
	if err := writeSSE(s.writer, "message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nullableWebString(s.stopSequence)},
		"usage": finalUsage,
	}); err != nil {
		return err
	}
	return writeSSE(s.writer, "message_stop", map[string]any{"type": "message_stop"})
}

func (s *webMessagesStream) closeThinking() error {
	if !s.thinkingStarted || s.thinkingClosed {
		return nil
	}
	s.thinkingClosed = true
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.thinkingIndex})
}

func (s *webMessagesStream) closeText() error {
	if !s.textStarted || s.textClosed {
		return nil
	}
	s.textClosed = true
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.textIndex})
}

type webStopFilter struct {
	sequences []string
	pending   string
	matched   string
}

func newWebStopFilter(sequences []string) *webStopFilter {
	filtered := make([]string, 0, len(sequences))
	for _, sequence := range sequences {
		if sequence != "" {
			filtered = append(filtered, sequence)
		}
	}
	return &webStopFilter{sequences: filtered}
}

func (f *webStopFilter) Push(delta string) (string, string) {
	if f == nil || len(f.sequences) == 0 {
		return delta, ""
	}
	if f.matched != "" {
		return "", f.matched
	}
	f.pending += delta
	matchAt := -1
	matched := ""
	for _, sequence := range f.sequences {
		if index := strings.Index(f.pending, sequence); index >= 0 && (matchAt < 0 || index < matchAt) {
			matchAt = index
			matched = sequence
		}
	}
	if matchAt >= 0 {
		emit := f.pending[:matchAt]
		f.pending = ""
		f.matched = matched
		return emit, matched
	}
	hold := 0
	for _, sequence := range f.sequences {
		maxPrefix := min(len(sequence)-1, len(f.pending))
		for size := maxPrefix; size > hold; size-- {
			if strings.HasSuffix(f.pending, sequence[:size]) {
				hold = size
				break
			}
		}
	}
	emitAt := len(f.pending) - hold
	emit := f.pending[:emitAt]
	f.pending = f.pending[emitAt:]
	return emit, ""
}

func (f *webStopFilter) Flush() string {
	if f == nil || f.matched != "" {
		return ""
	}
	value := f.pending
	f.pending = ""
	return value
}
