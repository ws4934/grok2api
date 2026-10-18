package channelhttp

import (
	"encoding/json"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type block struct {
	Index     int
	Reasoning bool
	ID        string
	Text      string
}

// blocks orders interleaved reasoning and text deltas into sequential output
// blocks (Responses output items, Messages content blocks). Reasoning that
// arrives before any text streams live as the first block and closes when
// text starts. Reasoning that arrives while text is open is held and emitted
// as its own block after the text closes. A closed index is never reopened,
// and the final object lists blocks exactly as they were streamed.
type blocks struct {
	open, close func(*block) error
	delta       func(*block, string) error
	all         []*block
	cur         *block
	held        string
}

func (s *blocks) add(reasoning bool, text string) error {
	if text == "" {
		return nil
	}
	if reasoning && s.cur != nil && !s.cur.Reasoning {
		s.held += text
		return nil
	}
	if s.cur == nil || s.cur.Reasoning != reasoning {
		if e := s.closeOpen(); e != nil {
			return e
		}
		if e := s.start(reasoning); e != nil {
			return e
		}
	}
	s.cur.Text += text
	return s.delta(s.cur, text)
}
func (s *blocks) start(reasoning bool) error {
	s.cur = &block{Index: len(s.all), Reasoning: reasoning}
	s.all = append(s.all, s.cur)
	return s.open(s.cur)
}
func (s *blocks) closeOpen() error {
	if s.cur == nil {
		return nil
	}
	b := s.cur
	s.cur = nil
	return s.close(b)
}

// finish closes the open block, emits held reasoning and makes sure the
// output has a text block, even an empty one.
func (s *blocks) finish() error {
	if e := s.closeOpen(); e != nil {
		return e
	}
	if s.held != "" {
		held := s.held
		s.held = ""
		if e := s.add(true, held); e != nil {
			return e
		}
		return s.closeOpen()
	}
	for _, b := range s.all {
		if !b.Reasoning {
			return nil
		}
	}
	if e := s.start(false); e != nil {
		return e
	}
	return s.closeOpen()
}

func responseItem(b *block, status string) gin.H {
	if b.Reasoning {
		return gin.H{"id": b.ID, "type": "reasoning", "summary": []any{gin.H{"type": "summary_text", "text": b.Text}}}
	}
	return gin.H{"id": b.ID, "type": "message", "status": status, "role": "assistant", "content": []any{gin.H{"type": "output_text", "text": b.Text, "annotations": []any{}}}}
}

// respond serves a text Responses request. Streams use the official event
// sequence with a monotonic sequence_number; non-stream replies return the
// object that response.completed would carry.
func (h *Handler) respond(c *gin.Context, key clientkey.Key, route model.Route, in chatInput) {
	out := &sse{c: c}
	id, created := "resp_"+uuid.NewString(), time.Now().Unix()
	var effort any
	if in.Request.ReasoningEffort != "" {
		effort = in.Request.ReasoningEffort
	}
	tools := []json.RawMessage{}
	if in.Tools != nil {
		tools = in.Tools
	}
	var choice any = "auto"
	if in.ToolChoice != nil {
		choice = in.ToolChoice
	}
	object := func(status string, output []any) gin.H {
		return gin.H{
			"id": id, "object": "response", "created_at": created, "status": status, "background": false,
			"error": nil, "incomplete_details": nil, "metadata": gin.H{}, "model": in.Request.Model, "output": output,
			"parallel_tool_calls": in.ParallelToolCalls, "previous_response_id": nil, "reasoning": gin.H{"effort": effort, "summary": nil},
			"store": false, "text": gin.H{"format": gin.H{"type": "text"}}, "tool_choice": choice, "tools": tools, "usage": nil,
		}
	}
	sequence := 0
	emit := func(name string, v gin.H) error {
		if !in.Stream {
			return nil
		}
		v["type"], v["sequence_number"] = name, sequence
		sequence++
		return out.send(name, v)
	}
	begun := false
	begin := func() error {
		if begun {
			return nil
		}
		begun = true
		if e := emit("response.created", gin.H{"response": object("in_progress", []any{})}); e != nil {
			return e
		}
		return emit("response.in_progress", gin.H{"response": object("in_progress", []any{})})
	}
	output := []any{}
	status := "completed"
	items := blocks{
		open: func(b *block) error {
			if e := begin(); e != nil {
				return e
			}
			if b.Reasoning {
				b.ID = "rs_" + uuid.NewString()
				if e := emit("response.output_item.added", gin.H{"output_index": b.Index, "item": gin.H{"id": b.ID, "type": "reasoning", "summary": []any{}}}); e != nil {
					return e
				}
				return emit("response.reasoning_summary_part.added", gin.H{"item_id": b.ID, "output_index": b.Index, "summary_index": 0, "part": gin.H{"type": "summary_text", "text": ""}})
			}
			b.ID = "msg_" + uuid.NewString()
			if e := emit("response.output_item.added", gin.H{"output_index": b.Index, "item": gin.H{"id": b.ID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}); e != nil {
				return e
			}
			return emit("response.content_part.added", gin.H{"item_id": b.ID, "output_index": b.Index, "content_index": 0, "part": gin.H{"type": "output_text", "text": "", "annotations": []any{}}})
		},
		delta: func(b *block, text string) error {
			if b.Reasoning {
				return emit("response.reasoning_summary_text.delta", gin.H{"item_id": b.ID, "output_index": b.Index, "summary_index": 0, "delta": text})
			}
			return emit("response.output_text.delta", gin.H{"item_id": b.ID, "output_index": b.Index, "content_index": 0, "delta": text})
		},
		close: func(b *block) error {
			item := responseItem(b, status)
			output = append(output, item)
			var e error
			if b.Reasoning {
				if e = emit("response.reasoning_summary_text.done", gin.H{"item_id": b.ID, "output_index": b.Index, "summary_index": 0, "text": b.Text}); e == nil {
					e = emit("response.reasoning_summary_part.done", gin.H{"item_id": b.ID, "output_index": b.Index, "summary_index": 0, "part": gin.H{"type": "summary_text", "text": b.Text}})
				}
			} else if e = emit("response.output_text.done", gin.H{"item_id": b.ID, "output_index": b.Index, "content_index": 0, "text": b.Text}); e == nil {
				e = emit("response.content_part.done", gin.H{"item_id": b.ID, "output_index": b.Index, "content_index": 0, "part": gin.H{"type": "output_text", "text": b.Text, "annotations": []any{}}})
			}
			if e != nil {
				return e
			}
			return emit("response.output_item.done", gin.H{"output_index": b.Index, "item": item})
		},
	}
	result, e := h.service.Chat(c.Request.Context(), key, route, in.Request, in.Stream, func(ev domain.ChatEvent) error {
		if ev.Done {
			return nil
		}
		if e := items.add(true, ev.Reasoning); e != nil {
			return e
		}
		return items.add(false, ev.Text)
	})
	if e != nil {
		f := h.chatFailure(route, e)
		if !out.started {
			publicError(c, f.status, f.code, f.message)
			return
		}
		failed := object("failed", output)
		failed["error"] = gin.H{"code": f.code, "message": f.message}
		_ = emit("response.failed", gin.H{"response": failed})
		return
	}
	if result.Truncated {
		status = "incomplete"
	}
	if e := begin(); e != nil {
		return
	}
	if e := items.finish(); e != nil {
		return
	}
	final := object(status, output)
	if result.Truncated {
		final["incomplete_details"] = gin.H{"reason": "max_output_tokens", "source": "gateway_estimate"}
	}
	final["usage"] = gin.H{
		"input_tokens": result.InputTokens, "input_tokens_details": gin.H{"cached_tokens": 0},
		"output_tokens": result.OutputTokens, "output_tokens_details": gin.H{"reasoning_tokens": h.service.EstimateTextTokens(result.Reasoning)},
		"total_tokens": result.InputTokens + result.OutputTokens,
	}
	if !in.Stream {
		c.JSON(200, final)
		return
	}
	_ = emit("response."+status, gin.H{"response": final})
}
