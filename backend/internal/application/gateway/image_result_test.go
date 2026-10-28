package gateway

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestImageResultCountsActualOutputsWithoutRetainingBase64(t *testing.T) {
	raw := `{"data":[{"b64_json":"` + strings.Repeat("A", 2<<20) + `"},{"url":"https://example.test/2.png"},{"b64_json":""}]}`
	body, counter, err := prefetchImageResult(io.NopCloser(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	buffer := make([]byte, 997)
	var forwarded strings.Builder
	for {
		n, err := body.Read(buffer)
		forwarded.Write(buffer[:n])
		if cap(counter.compact) > 64<<10 {
			t.Fatalf("retained %d bytes of image payload", cap(counter.compact))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if counter.outputCount() != 2 || forwarded.String() != raw {
		t.Fatalf("count=%d or response bytes changed", counter.outputCount())
	}
}

func TestMergedImageResultsKeepLargePayloadsAndExactMetadata(t *testing.T) {
	payload := strings.Repeat("A", 2<<20)
	first := `{"created":999999999999999999,"data":[{"b64_json":"` + payload + `"}],"usage":{"input_tokens":5,"output_tokens":11,"total_tokens":16}}`
	second := `{"created":2,"data":[{"b64_json":"\\u0041"}],"usage":{"input_tokens":5,"output_tokens":13,"total_tokens":18}}`
	reader, err := mergeImageBodies([][]byte{[]byte(first), []byte(second)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	var body struct {
		Created json.Number `json:"created"`
		Data    []struct {
			Base64 string `json:"b64_json"`
		}
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		}
	}
	if err != nil || json.Unmarshal(raw, &body) != nil || body.Created.String() != "999999999999999999" || len(body.Data) != 2 || body.Data[0].Base64 != payload || body.Data[1].Base64 != `\u0041` || !strings.Contains(string(raw), `"b64_json":"\\u0041"`) {
		t.Fatalf("merged response changed its metadata or image payload: created=%s items=%d err=%v", body.Created, len(body.Data), err)
	}
	if body.Usage.InputTokens != 5 || body.Usage.OutputTokens != 24 || body.Usage.TotalTokens != 29 {
		t.Fatalf("merged usage=%+v", body.Usage)
	}
}

func BenchmarkImageResultInspection(b *testing.B) {
	raw := `{"data":[{"b64_json":"` + strings.Repeat("A", 2<<20) + `"}]}`
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		body, counter, err := prefetchImageResult(io.NopCloser(strings.NewReader(raw)))
		if err != nil {
			b.Fatal(err)
		}
		_, err = io.Copy(io.Discard, body)
		_ = body.Close()
		if err != nil || counter.outputCount() != 1 {
			b.Fatalf("count=%d err=%v", counter.outputCount(), err)
		}
	}
}

func TestImageResultRejectsZeroOutputsBeforeDownstreamHeaders(t *testing.T) {
	for _, raw := range []string{`{"data":[]}`, `{"data":[{"b64_json":""}]}`, `{"data":[{"url":""}]}`, `{"data":[`, `{}`} {
		body, _, err := prefetchImageResult(io.NopCloser(strings.NewReader(raw)))
		if body != nil {
			_ = body.Close()
		}
		if err == nil {
			t.Fatalf("empty or invalid result accepted: %s", raw)
		}
	}
}
