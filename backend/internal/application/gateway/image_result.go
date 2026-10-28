package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/pkg/imagejson"
)

// imageResultBody inspects only a compact copy. Large Base64 values continue
// to the caller unchanged and are never decoded or retained for counting.
type imageResultBody struct {
	io.ReadCloser
	projection imagejson.Projection
	compact    []byte
	count      int
	finished   bool
	err        error
}

type invalidImageResult struct{ cause error }

func (e invalidImageResult) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("读取图片生成结果失败: %v", e.cause)
	}
	return "上游未返回有效的图片结果"
}
func (e invalidImageResult) Unwrap() error            { return e.cause }
func (invalidImageResult) PublicErrorMessage() string { return "未获得可用图片" }
func (invalidImageResult) HTTPStatusCode() int        { return http.StatusBadGateway }
func (invalidImageResult) RequestScopedFailure() bool { return true }

func (b *imageResultBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.compact = b.projection.Append(b.compact, p[:n])
	}
	if err == io.EOF {
		b.finish()
		if b.err != nil {
			err = b.err
		}
	}
	return n, err
}

func (b *imageResultBody) finish() {
	if b.finished {
		return
	}
	b.finished = true
	var body struct {
		Data []struct {
			URL    string `json:"url"`
			Base64 string `json:"b64_json"`
		} `json:"data"`
	}
	if b.projection.Invalid() || json.Unmarshal(b.compact, &body) != nil {
		b.err = invalidImageResult{}
		return
	}
	for _, item := range body.Data {
		if item.URL != "" || item.Base64 != "" {
			b.count++
		}
	}
	if b.count == 0 {
		b.err = invalidImageResult{}
	}
	b.compact = nil
}

func (b *imageResultBody) outputCount() int {
	b.finish()
	return b.count
}

// prefetchImageResult catches empty/invalid small responses before sending
// downstream 200. Base64 bodies are released as soon as image bytes arrive;
// only their short prefix is replayed, not the whole image response.
func prefetchImageResult(body io.ReadCloser) (io.ReadCloser, *imageResultBody, error) {
	inspector := &imageResultBody{ReadCloser: body}
	var prefix bytes.Buffer
	buffer := make([]byte, 4096)
	for !inspector.finished && !inspector.projection.PayloadObserved() {
		n, err := inspector.Read(buffer)
		_, _ = prefix.Write(buffer[:n])
		if prefix.Len() > 128<<20 {
			return nil, inspector, invalidImageResult{}
		}
		if err != nil && err != io.EOF {
			return nil, inspector, err
		}
	}
	return &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), inspector), source: body}, inspector, nil
}
