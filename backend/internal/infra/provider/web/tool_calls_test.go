package web

import (
	"encoding/json"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/toolemu"
)

// TestApplyParsedToolCallsReadsEveryBlock: a whole reply is parsed as it
// would be streamed: an unclosed root, a block without declared functions
// and several blocks all give their calls.
func TestApplyParsedToolCallsReadsEveryBlock(t *testing.T) {
	configuration, err := toolemu.ParseConfig(json.RawMessage(`[{"type":"function","name":"lookup"}]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	call := `<tool_call><tool_name>lookup</tool_name><parameters>{"q":1}</parameters></tool_call>`
	undeclared := `<tool_calls><tool_call><tool_name>rm</tool_name><parameters>{}</parameters></tool_call></tool_calls>`
	for name, c := range map[string]struct {
		reply, text string
		calls       int
	}{
		"unclosed root":          {"Checking. <tool_calls>" + call, "Checking.", 1},
		"undeclared block first": {undeclared + " then <tool_calls>" + call + "</tool_calls>", undeclared + " then", 1},
		"two blocks":             {"<tool_calls>" + call + "</tool_calls>\n<tool_calls>" + call + "</tool_calls>", "", 2},
		"no calls":               {"Plain " + undeclared, "Plain " + undeclared, 0},
	} {
		parsed := parsedChat{}
		parsed.appendText(c.reply)
		applyParsedToolCalls(&parsed, configuration)
		if parsed.Text.String() != c.text || len(parsed.ToolCalls) != c.calls {
			t.Errorf("%s: text %q, calls %#v", name, parsed.Text.String(), parsed.ToolCalls)
		}
		for _, parsedCall := range parsed.ToolCalls {
			if parsedCall.Name != "lookup" || parsedCall.Arguments != `{"q":1}` {
				t.Errorf("%s: call %#v", name, parsedCall)
			}
		}
	}
}
