package inference

import (
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

var updateStreamGolden = flag.Bool("update-stream-golden", false, "rewrite testdata/stream_copy.golden")

// stepReader returns at most n bytes per Read, like a network body.
type stepReader struct {
	data []byte
	n    int
}

func (r *stepReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	k := min(len(p), r.n, len(r.data))
	copy(p, r.data[:k])
	r.data = r.data[k:]
	return k, nil
}

func sseLines(lines ...string) string {
	return strings.Join(lines, "\n")
}

func goldenImageB64(size int) string {
	raw := make([]byte, size)
	for i := range raw {
		raw[i] = byte(i*131 + i>>7)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

var streamGoldenCases = []struct {
	name     string
	protocol streamProtocol
	body     string
}{
	{"responses_text", streamProtocolResponses, sseLines(
		": keepalive",
		"",
		"event: response.created",
		`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1760000000,"status":"in_progress","model":"gpt-5","output":[]}}`,
		"",
		"event: response.in_progress",
		`data: {"type":"response.in_progress","id":"resp_1","sequence_number":1,"response":{"id":"resp_other","object":"response","created_at":1760000000,"status":"in_progress","model":"gpt-5","output":[]}}`,
		"",
		`data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		"",
		`data: {"type":"response.reasoning_summary_text.delta","sequence_number":3,"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"plan <a&b>"}`,
		"",
		`data: {"type":"response.reasoning_text.delta","sequence_number":4,"output_index":0,"delta":"no item id"}`,
		"",
		`data: {"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"gAAAA+/=="}}`,
		"",
		`data: {"type":"response.output_item.added","sequence_number":6,"output_index":1,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
		"",
		`data: {"type":"response.content_part.added","sequence_number":7,"item_id":"msg_1","output_index":1,"content_index":0,"part":{"type":"output_text","text":""}}`,
		"",
		`data: {"type":"response.output_text.delta","sequence_number":8,"item_id":"msg_1","output_index":1,"content_index":0,"delta":"Hi \u003cb\u003e & \"q\" 中文 \ud83d\ude00\n","logprobs":[]}`+"\r",
		"\r",
		`data:{"type":"response.output_text.delta","sequence_number":9007199254740993,"item_id":"msg_1","output_index":1,"content_index":0,"delta":"  spaced","obfuscation":"x1"}`,
		"",
		`data: {"type":"response.output_text.delta","sequence_number":10,"item_id":"msg_1","output_index":1,"content_index":0,"delta":""}`,
		"",
		"id: 11",
		"retry: 100",
		"data: not json",
		"data:",
		"",
		`data: {"type":"response.output_text.done","sequence_number":11,"item_id":"msg_1","output_index":1,"content_index":0,"text":"Hi"}`,
		"",
		`data: {"type":"response.content_part.done","sequence_number":12,"item_id":"msg_1","output_index":1,"content_index":0,"part":{"type":"output_text","text":"Hi","annotations":[]}}`,
		"",
		`data: {"type":"response.output_item.done","sequence_number":13,"output_index":1,"item":{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hi","annotations":[{"type":"url_citation","url":"https://x.test/?a=1&b=2"}]}]}}`,
		"",
		`data: {"type":"response.output_item.added","sequence_number":14,"output_index":2,"item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_1","name":"shell","arguments":""}}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","sequence_number":15,"item_id":"fc_1","output_index":2,"delta":"{\"cmd\":"}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","sequence_number":16,"output_index":2,"delta":"[\"ls\"]}"}`,
		"",
		`data: {"type":"response.function_call_arguments.done","sequence_number":17,"item_id":"fc_1","output_index":2,"arguments":"{\"cmd\":[\"ls\"]}"}`,
		"",
		`data: {"type":"response.output_item.done","sequence_number":18,"output_index":2,"item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"shell","arguments":"{\"cmd\":[\"ls\"]}"}}`,
		"",
		`data: {"type":"response.output_item.added","sequence_number":19,"output_index":3,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}}`,
		"",
		`data: {"type":"response.output_text.delta","sequence_number":20,"output_index":3,"content_index":0,"delta":"second"}`,
		"",
		`data: {"type":"response.output_item.done","sequence_number":21,"output_index":3,"item":{"id":"msg_2","type":"message","content":[{"type":"output_text","text":"second","annotations":null}]}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","sequence_number":22,"response":{"id":"resp_1","object":"response","created_at":1760000000,"status":"completed","model":"gpt-5","output":[{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"Hi","annotations":[]}]}],"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":100},"output_tokens":30,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":150}}}`,
		"",
		"data: [DONE]",
		"",
		"",
	)},
	{"responses_truncated_tail", streamProtocolResponses, sseLines(
		"event: response.created",
		`data: {"type":"response.created","id":"resp_t","sequence_number":1,"response":{"id":"resp_t","object":"response","created_at":1760000001,"status":"in_progress","model":"grok-4","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_t","output_index":0,"delta":"partial"}`,
		"",
		`data: {"type":"response.output_text.delta","sequence_number":3,"output_index":0,"delta":"tail"}`,
	)},
	{"responses_failed", streamProtocolResponses, sseLines(
		`data: {"type":"response.created","id":"resp_f","response":{"id":"resp_f","object":"response","created_at":1760000002,"status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","item_id":"m","output_index":0,"delta":"x"}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","id":"resp_f","response":{"id":"resp_f","object":"response","created_at":1760000002,"status":"failed","model":"grok-4","output":[],"error":{"code":"server_error","message":"boom"}}}`,
		"",
		"",
	)},
	{"responses_error_event", streamProtocolResponses, sseLines(
		`data: {"type":"response.created","id":"resp_e","response":{"id":"resp_e","object":"response","created_at":1760000003,"model":"grok-4","output":[]}}`,
		"",
		`data: {"type":"error","code":"server_error","message":"failed","sequence_number":3}`,
		"",
		"",
	)},
	{"responses_image", streamProtocolResponses, sseLines(
		`data: {"type":"response.created","id":"resp_i","sequence_number":0,"response":{"id":"resp_i","object":"response","created_at":1760000004,"status":"in_progress","model":"gpt-5","output":[]}}`,
		"",
		`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"ig_1","type":"image_generation_call","status":"in_progress"}}`,
		"",
		`data: {"type":"response.image_generation_call.partial_image","sequence_number":2,"item_id":"ig_1","output_index":0,"partial_image_index":0,"partial_image_b64":"`+goldenImageB64(6<<10)+`"}`,
		"",
		`data: {"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"`+goldenImageB64(72<<10)+`"}}`,
		"",
		`data: {"type":"response.completed","sequence_number":4,"response":{"id":"resp_i","object":"response","created_at":1760000004,"status":"completed","model":"gpt-5","output":[{"id":"ig_1","type":"image_generation_call","status":"completed","result":"`+goldenImageB64(72<<10)+`"}],"usage":{"input_tokens":10,"output_tokens":1000,"total_tokens":1010}}}`,
		"",
		"",
	)},
	{"chat", streamProtocolChat, sseLines(
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"grok-4","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		"",
		": grok2api-reasoning-start",
		"",
		`data: {"id":"chatcmpl_1","model":"grok-4","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
		"",
		`data: {"id":"chatcmpl_1","model":"grok-4","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		"",
		`data: {"id":"chatcmpl_1","model":"grok-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		"",
		`data: {"id":"chatcmpl_1","model":"grok-4","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60,"prompt_tokens_details":{"cached_tokens":30},"completion_tokens_details":{"reasoning_tokens":5}}}`,
		"",
		`data: {"id":"chatcmpl_2","model":"grok-4","choices":[{"index":0,"delta":{"content":"late"}}],"usage":"n/a"}`,
		"",
		"data: [DONE]",
		"",
		"",
	)},
	{"anthropic", streamProtocolAnthropic, sseLines(
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":20,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		"",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		"",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi"}}`,
		"",
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		"",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":20,"output_tokens":15,"cache_read_input_tokens":80,"cache_creation_input_tokens":5}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
		"",
	)},
	{"image", streamProtocolImage, sseLines(
		`data: {"type":"image_generation.partial_image","b64_json":"AAAA","partial_image_index":0}`,
		"",
		`data: {"type":"image_generation.completed","b64_json":"BBBB","usage":{"input_tokens":10,"output_tokens":100,"total_tokens":110}}`,
		"",
		"",
	)},
}

// TestCopyStreamGolden pins the bytes copyStream forwards and the metadata it
// reports, so decoding changes cannot alter either. The golden was captured
// before the inspector and Responses compat layer stopped re-decoding lines.
func TestCopyStreamGolden(t *testing.T) {
	var got strings.Builder
	for _, test := range streamGoldenCases {
		var first string
		for _, step := range []int{7, 4 << 10, 1 << 20} {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			firstTokens := 0
			metadata, err := copyStreamWithFallbackModel(context.Writer, &stepReader{data: []byte(test.body), n: step}, test.protocol, func() { firstTokens++ }, "fallback-model")
			result := formatStreamGolden(test.name, recorder.Body.Bytes(), metadata, err, firstTokens)
			if first == "" {
				first = result
				got.WriteString(result)
			} else if result != first {
				t.Fatalf("%s: %d-byte reads changed the result:\n%s\nwant:\n%s", test.name, step, result, first)
			}
		}
	}
	const path = "testdata/stream_copy.golden"
	if *updateStreamGolden {
		if err := os.WriteFile(path, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("stream output differs from %s:\n%s", path, got.String())
	}
}

func formatStreamGolden(name string, output []byte, metadata responseMetadata, err error, firstTokens int) string {
	var b strings.Builder
	failure := metadata.StreamFailure
	metadata.StreamFailure = nil
	fmt.Fprintf(&b, "== %s\nerr: %v\nfirst_token: %d\nmeta: %+v\n", name, err, firstTokens, metadata)
	if failure != nil {
		fmt.Fprintf(&b, "failure: %s truncated=%v\n", failure.Body, failure.BodyTruncated)
	}
	if len(output) > 8<<10 {
		fmt.Fprintf(&b, "output: %d bytes sha256 %x\n\n", len(output), sha256.Sum256(output))
	} else {
		fmt.Fprintf(&b, "output:\n%s\n", output)
	}
	return b.String()
}
