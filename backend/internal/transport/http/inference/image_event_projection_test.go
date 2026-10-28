package inference

import (
	"bytes"
	"encoding/json"

	"github.com/chenyme/grok2api/backend/internal/pkg/imagejson"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestLargeImageStreamKeepsCompletionAndUsage(t *testing.T) {
	image := strings.Repeat("A", maxStreamEventInspectionBytes+(64<<10))
	for _, typ := range []string{"image_generation.completed", "image_edit.completed"} {
		for _, lateType := range []bool{false, true} {
			name := typ + "/early-type"
			data := `{"type":"` + typ + `","b64_json":"` + image + `","model":"模型一","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`
			if lateType {
				name = typ + "/late-type"
				data = `{"b64_json":"` + image + `","model":"模型一","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"type":"` + typ + `"}`
			}
			t.Run(name, func(t *testing.T) {
				body := "event: " + typ + "\r\ndata: " + data + "\r\n\r\n"
				recorder, usage, code := writeImageStream(t, body)
				if code != "" || usage.OutputImages != 1 || usage.InputTokens != 3 || usage.OutputTokens != 5 || usage.ResponseModel != "模型一" {
					t.Fatalf("code=%q usage=%+v", code, usage)
				}
				if recorder.Body.String() != body {
					t.Fatal("forwarded image changed")
				}
			})
		}
	}
}

func TestImageInspectorBoundsPayloadAcrossChunkBoundaries(t *testing.T) {
	image := strings.Repeat("A", maxStreamEventInspectionBytes+(64<<10))
	data := []byte(`data: {"b64_json":"` + image + `","type":"image_edit.completed","id":"图一","usage":{"input_tokens":4,"output_tokens":9}}`)
	for _, size := range []int{7, 4093, responseCopyBufferBytes} {
		inspector := &responseInspector{protocol: streamProtocolImage}
		for start := 0; start < len(data); start += size {
			inspector.Inspect(data[start:min(start+size, len(data))])
			if cap(inspector.pending) > 64<<10 {
				t.Fatalf("size=%d retained %d bytes of image", size, cap(inspector.pending))
			}
		}
		inspector.Finish() // No final newline, no event: header.
		meta := inspector.Metadata()
		if err := inspector.TerminalError(); err != nil || meta.Usage.OutputImages != 1 || meta.ResponseID != "图一" || meta.Usage.OutputTokens != 9 {
			t.Fatalf("size=%d meta=%+v err=%v", size, meta, err)
		}
		inspector.Finish()
		if inspector.Metadata().Usage.OutputImages != 1 {
			t.Fatal("Finish counted an image twice")
		}
	}
}

func TestImageProjectionValidatesSkippedStrings(t *testing.T) {
	for _, data := range []string{
		`{"type":"image_generation.completed","b64_json":"中文\"\\\/\b\f\n\r\t\u4e2d"}`,
		`{"b64_\u006ason":"AAAA","type":"image_edit.completed"}`,
		`{"type":"image_generation.completed","b64_json":"bad\x"}`,
		`{"type":"image_generation.completed","b64_json":"bad\u00z0"}`,
		`{"type":"image_generation.completed","b64_json":"bad` + "\t" + `"}`,
		`{"type":"image_generation.completed","b64_json":"unfinished`,
		`{"type":"image_generation.completed","b64_json":"AAAA"`,
	} {
		for size := 1; size <= 17; size++ {
			inspector := &responseInspector{protocol: streamProtocolImage}
			line := []byte("data: " + data + "\n\n")
			for start := 0; start < len(line); start += size {
				inspector.Inspect(line[start:min(start+size, len(line))])
			}
			inspector.Finish()
			valid := json.Valid([]byte(data))
			if (inspector.TerminalError() == nil) != valid || (inspector.Metadata().Usage.OutputImages == 1) != valid {
				t.Fatalf("size=%d valid=%v images=%d err=%v input=%s", size, valid, inspector.Metadata().Usage.OutputImages, inspector.TerminalError(), data)
			}
		}
	}
}

func TestImageSuccessPassThroughStillLocalizesNextError(t *testing.T) {
	image := strings.Repeat("A", 2<<20)
	for _, success := range []string{
		"data: " + `{"type":"image_generation.completed","b64_json":"` + image + `"}`,
		"event: image_generation.completed\ndata: " + `{"b64_json":"` + image + `","type":"image_generation.completed"}`,
	} {
		body := success + "\n\n" + "event: error\ndata: " + `{"type":"error","error":{"code":"content_policy_violation","message":"Generated video rejected by content moderation.","param":"prompt"}}` + "\n\n"
		recorder, usage, code := writeImageStream(t, body)
		if usage.OutputImages != 1 || code != "upstream_stream_error" || !strings.Contains(recorder.Body.String(), `"param":"prompt"`) || !strings.Contains(recorder.Body.String(), "内容") || strings.Contains(recorder.Body.String(), "Generated video rejected") {
			t.Fatalf("images=%d code=%q output suffix=%s", usage.OutputImages, code, recorder.Body.String()[max(0, recorder.Body.Len()-500):])
		}
	}
}

func TestInterruptedImagePayloadIsObservedButNotCounted(t *testing.T) {
	inspector := &responseInspector{protocol: streamProtocolImage}
	inspector.Inspect([]byte(`data: {"type":"image_edit.completed","b64_json":"`))
	for range 260 {
		inspector.Inspect(bytes.Repeat([]byte{'A'}, responseCopyBufferBytes))
	}
	inspector.Finish()
	usage := inspector.Metadata().Usage
	if !usage.OutputObserved || usage.OutputImages != 0 || inspector.TerminalError() != errUpstreamStreamIncomplete {
		t.Fatalf("usage=%+v err=%v", usage, inspector.TerminalError())
	}
}

func TestImageProjectionDoesNotAcceptMalformedLargeCompletion(t *testing.T) {
	body := "data: " + `{"type":"image_generation.completed","b64_json":"` + strings.Repeat("A", maxStreamEventInspectionBytes+(64<<10)) + `\x"}` + "\n\n"
	recorder, usage, code := writeImageStream(t, body)
	if usage.OutputImages != 0 || code != "upstream_stream_incomplete" || recorder.Body.String() != body {
		t.Fatalf("code=%q images=%d forwarded=%d", code, usage.OutputImages, recorder.Body.Len())
	}
}

func FuzzImageEventProjection(f *testing.F) {
	for _, seed := range []string{`{"b64_json":"AAAA","type":"image_generation.completed"}`, `{"b64_json":"bad\x"}`, `{"b64_\u006ason":"中文\u1234"}`, `{"b64_json":{"a":[null,1,"b64_json"]}}`} {
		f.Add(seed, uint8(3))
	}
	f.Fuzz(func(t *testing.T, raw string, chunk uint8) {
		var projection imagejson.Projection
		var compact []byte
		for start, size := 0, max(1, int(chunk)); start < len(raw); start += size {
			compact = projection.Append(compact, []byte(raw[start:min(start+size, len(raw))]))
		}
		if valid := !projection.Invalid() && json.Valid(compact); valid != json.Valid([]byte(raw)) {
			t.Fatalf("JSON validity changed: input=%q compact=%q invalid=%v", raw, compact, projection.Invalid())
		}
	})
}

type discardStreamHTTP struct{ header http.Header }

func (w *discardStreamHTTP) Header() http.Header            { return w.header }
func (*discardStreamHTTP) Write(p []byte) (int, error)      { return len(p), nil }
func (*discardStreamHTTP) WriteHeader(int)                  {}
func (*discardStreamHTTP) Flush()                           {}
func (*discardStreamHTTP) SetWriteDeadline(time.Time) error { return nil }

func BenchmarkImageSSE2MiB(b *testing.B) {
	raw := []byte("event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"" + strings.Repeat("A", 2<<20) + "\"}\n\n")
	benchmarkImageStream(b, raw)
}

// Web and ChatGPT encode event maps with b64_json before type. The SSE event
// field must enable the same no-buffer path regardless of JSON member order.
func BenchmarkImageSSE2MiBLateType(b *testing.B) {
	raw := []byte("event: image_generation.completed\ndata: {\"b64_json\":\"" + strings.Repeat("A", 2<<20) + "\",\"type\":\"image_generation.completed\"}\n\n")
	benchmarkImageStream(b, raw)
}

func benchmarkImageStream(b *testing.B, raw []byte) {
	b.Helper()
	gin.SetMode(gin.TestMode)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		c, _ := gin.CreateTestContext(&discardStreamHTTP{header: make(http.Header)})
		metadata, err := copyStream(c.Writer, bytes.NewReader(raw), streamProtocolImage, nil)
		if err != nil || metadata.Usage.OutputImages != 1 {
			b.Fatalf("metadata=%+v err=%v", metadata, err)
		}
	}
}
