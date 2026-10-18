package channelhttp

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestChatResponsesNormalizeIdentically(t *testing.T) {
	chat := `{"model":"m","messages":[{"role":"system","content":"规则"},{"role":"user","content":"问题"},{"role":"assistant","content":"answer"},{"role":"user","content":[{"type":"text","text":"继续"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	responses := `{"model":"m","store":false,"instructions":"规则","input":[{"role":"user","content":[{"type":"input_text","text":"问题"}]},{"role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"role":"user","content":[{"type":"input_text","text":"继续"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
	a, _, e := DecodeChat([]byte(chat), false)
	if e != nil {
		t.Fatal(e)
	}
	b, _, e := DecodeChat([]byte(responses), true)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("semantic mismatch: %+v %+v", a, b)
	}
}
func TestUnsupportedChatParametersAreRejected(t *testing.T) {
	for _, responses := range []bool{false, true} {
		base := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
		if responses {
			delete(base, "messages")
			base["input"] = "hello"
		}
		for _, parameter := range []string{"tools", "tool_choice", "previous_response_id", "temperature", "top_p", "max_tokens", "max_output_tokens", "reasoning"} {
			base[parameter] = 1
			raw, _ := json.Marshal(base)
			if _, _, e := DecodeChat(raw, responses); e == nil {
				t.Fatalf("accepted %s", parameter)
			}
			delete(base, parameter)
		}
	}
	if _, _, e := DecodeChat([]byte(`{"model":"m","input":"hello","store":true}`), true); e == nil {
		t.Fatal("stored Responses accepted")
	}
}
