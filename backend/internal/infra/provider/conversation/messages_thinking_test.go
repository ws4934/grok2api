package conversation

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// checkMessagesBlocks fails on content blocks that overlap, skip an index,
// or get deltas and stops while not open.
func checkMessagesBlocks(t *testing.T, stream string) {
	t.Helper()
	open, next := -1, 0
	scanner := bufio.NewScanner(strings.NewReader(stream))
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		_ = json.Unmarshal([]byte(data), &event)
		switch event.Type {
		case "content_block_start":
			if open >= 0 || event.Index != next {
				t.Fatalf("block %d starts while %d is open, next is %d:\n%s", event.Index, open, next, stream)
			}
			open, next = event.Index, next+1
		case "content_block_delta", "content_block_stop":
			if event.Index != open {
				t.Fatalf("%s for block %d while %d is open:\n%s", event.Type, event.Index, open, stream)
			}
			if event.Type == "content_block_stop" {
				open = -1
			}
		}
	}
}

func messagesStream(t *testing.T, events ...string) string {
	t.Helper()
	var source strings.Builder
	for _, event := range append(events, `{"type":"response.completed","response":{"status":"completed"}}`) {
		var head struct{ Type string }
		_ = json.Unmarshal([]byte(event), &head)
		source.WriteString("event: " + head.Type + "\ndata: " + event + "\n\n")
	}
	converted, err := io.ReadAll(ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(source.String())), OperationMessages, ResponseOptions{AnthropicThinking: true}))
	if err != nil {
		t.Fatal(err)
	}
	return string(converted)
}

// TestMessagesStreamThinkingNeverOverlapsText: reasoning that arrives after
// text (ChatGPT emits held reasoning once the text closes) never writes to a
// stopped thinking block or opens one over the text block.
func TestMessagesStreamThinkingNeverOverlapsText(t *testing.T) {
	reasoning := func(id, delta string) []string {
		return []string{
			`{"type":"response.output_item.added","item":{"id":"` + id + `","type":"reasoning"}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"` + id + `","delta":"` + delta + `"}`,
			`{"type":"response.output_item.done","item":{"id":"` + id + `","type":"reasoning"}}`,
		}
	}
	text := func(delta string) string {
		return `{"type":"response.output_text.delta","delta":"` + delta + `"}`
	}
	after := messagesStream(t, append(append(reasoning("rs_1", "think one"), text("Hello"), text(" world")), reasoning("rs_2", "think two")...)...)
	checkMessagesBlocks(t, after)
	if !strings.Contains(after, `"thinking":"think one"`) || strings.Contains(after, "think two") || !strings.Contains(after, `"text":" world"`) {
		t.Fatalf("reasoning after text:\n%s", after)
	}
	first := messagesStream(t, append([]string{text("Hello")}, reasoning("rs_1", "late")...)...)
	checkMessagesBlocks(t, first)
	if !strings.Contains(first, `"thinking":"late"`) || !strings.Contains(first, `"text":"Hello"`) {
		t.Fatalf("reasoning only after text:\n%s", first)
	}
}
