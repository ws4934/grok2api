package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/gin-gonic/gin"
)

func (h *Handler) writeMediaResult(c *gin.Context, result *gateway.Result) {
	errorCode := ""
	defer result.Body.Close()
	defer func() { result.Finalize(gateway.Usage{}, "", errorCode) }()
	if isUpstreamCredentialStatus(result.StatusCode) {
		errorCode = "upstream_unavailable"
		clientCode := readCredentialErrorCode(result.StatusCode, result.Body)
		writeOpenAIError(c, http.StatusServiceUnavailable, clientCode, credentialErrorMessage(clientCode))
		return
	}
	if result.StatusCode < http.StatusOK || (result.StatusCode >= http.StatusMultipleChoices && result.StatusCode < http.StatusBadRequest) {
		errorCode = "invalid_upstream_status"
		writeOpenAIError(c, http.StatusBadGateway, "invalid_upstream_response", "上游媒体服务返回了不安全的重定向响应")
		return
	}
	contentType, safeContentType := normalizeMediaResponseContentType(result.Header.Get("Content-Type"))
	if !safeContentType {
		errorCode = "unsafe_media_content_type"
		writeOpenAIError(c, http.StatusBadGateway, "invalid_media_type", "上游媒体服务返回了不受支持的内容类型")
		return
	}
	contentLength, contentLengthErr := strconv.ParseInt(result.Header.Get("Content-Length"), 10, 64)
	if contentLengthErr == nil && contentLength > maxMediaResponseTransferBytes {
		errorCode = "response_too_large"
		writeOpenAIError(c, http.StatusBadGateway, "media_too_large", "上游媒体超过 2 GiB 安全上限")
		return
	}
	setSafeMediaResponseHeaders(c, result.Header)
	if contentLengthErr == nil && contentLength >= 0 {
		c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	} else {
		c.Header("Trailer", mediaTransferErrorTrailer)
	}
	if err := writeMediaBody(c, result.Body, contentType, result.StatusCode, maxMediaResponseTransferBytes); err != nil {
		if errors.Is(err, errResponseTransferLimit) {
			errorCode = "response_too_large"
		} else {
			errorCode = "stream_interrupted"
		}
		if contentLengthErr != nil {
			c.Header(mediaTransferErrorTrailer, errorCode)
		}
	}
}

func normalizeMediaResponseContentType(value string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	mediaType = strings.ToLower(mediaType)
	switch mediaType {
	case "application/json":
		return "application/json; charset=utf-8", true
	case "text/plain":
		return "text/plain; charset=utf-8", true
	case "application/ogg":
		return mediaType, true
	}
	if strings.HasPrefix(mediaType, "audio/") {
		switch mediaType {
		case "audio/aac", "audio/flac", "audio/l16", "audio/mpeg", "audio/mp3", "audio/ogg", "audio/opus", "audio/pcm", "audio/wav", "audio/webm", "audio/x-flac", "audio/x-wav":
			return mediaType, true
		}
	}
	return "", false
}

func setSafeMediaResponseHeaders(c *gin.Context, upstream http.Header) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Header("Referrer-Policy", "no-referrer")
	for _, name := range []string{"Retry-After", "X-Request-Id"} {
		if value := strings.TrimSpace(upstream.Get(name)); value != "" {
			c.Header(name, value)
		}
	}
}

func setResponseWriteDeadline(writer http.ResponseWriter) error {
	err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// writeMediaBody binds the validated non-HTML content type before emitting the
// response body, so no caller can stream media bytes without their MIME context.
func writeMediaBody(c *gin.Context, source io.Reader, contentType string, statusCode int, limit int64) error {
	c.Header("Content-Type", contentType)
	c.Status(statusCode)
	buffer := make([]byte, 64<<10)
	var transferred int64
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			remaining := limit - transferred
			if remaining <= 0 {
				return errResponseTransferLimit
			}
			writeSize := n
			if int64(writeSize) > remaining {
				writeSize = int(remaining)
			}
			if err := setResponseWriteDeadline(c.Writer); err != nil {
				return err
			}
			written, writeErr := c.Writer.Write(buffer[:writeSize])
			transferred += int64(written)
			if writeErr != nil {
				return writeErr
			}
			if written != writeSize {
				return io.ErrShortWrite
			}
			if writeSize != n {
				return errResponseTransferLimit
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func (h *Handler) writeResult(c *gin.Context, result *gateway.Result, stream bool, protocol streamProtocol) {
	h.writeProtocolResult(c, result, stream, false, protocol, "")
}

func (h *Handler) writeResponsesResult(c *gin.Context, result *gateway.Result, stream bool, fallbackModel string) {
	h.writeProtocolResult(c, result, stream, false, streamProtocolResponses, fallbackModel)
}

func (h *Handler) writeAnthropicResult(c *gin.Context, result *gateway.Result, stream bool) {
	h.writeProtocolResult(c, result, stream, true, streamProtocolAnthropic, "")
}

func (h *Handler) writeProtocolResult(c *gin.Context, result *gateway.Result, stream, anthropic bool, protocol streamProtocol, fallbackModel string) {
	usage := gateway.Usage{}
	responseID := ""
	errorCode := ""
	defer result.Body.Close()
	defer func() { result.Finalize(usage, responseID, errorCode) }()
	if isUpstreamCredentialStatus(result.StatusCode) {
		errorCode = "upstream_unavailable"
		clientCode := readCredentialErrorCode(result.StatusCode, result.Body)
		if anthropic {
			writeAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", credentialErrorMessage(clientCode), clientCode)
		} else {
			writeOpenAIError(c, http.StatusServiceUnavailable, clientCode, credentialErrorMessage(clientCode))
		}
		return
	}
	body := io.Reader(result.Body)
	if !stream && result.StatusCode >= http.StatusOK && result.StatusCode < http.StatusMultipleChoices {
		var peekErr error
		body, peekErr = peekNonEmptyJSONBody(result.Body)
		if peekErr != nil {
			status, code, message := http.StatusBadGateway, "stream_interrupted", "读取上游响应失败"
			switch {
			case neterror.IsUpstreamStreamIdleTimeout(peekErr):
				status, code, message = http.StatusGatewayTimeout, "upstream_stream_idle_timeout", "上游响应长时间无数据"
			case neterror.IsUpstreamResponseEmpty(peekErr):
				status, code, message = http.StatusBadGateway, "upstream_response_empty", "上游响应为空"
			}
			errorCode = code
			if anthropic {
				writeAnthropicError(c, status, "api_error", message, code)
			} else {
				writeOpenAIError(c, status, code, message)
			}
			return
		}
	}
	transferLimit := int64(maxJSONResponseTransferBytes)
	if stream {
		transferLimit = maxStreamResponseTransferBytes
	}
	if contentLength, parseErr := strconv.ParseInt(result.Header.Get("Content-Length"), 10, 64); parseErr == nil && contentLength > transferLimit {
		errorCode = "response_too_large"
		writeOpenAIError(c, http.StatusBadGateway, "response_too_large", "上游响应超过代理安全上限")
		return
	}
	copyHeaders(c.Writer.Header(), result.Header)
	if result.StatusCode >= 400 {
		errorCode = "upstream_error"
		if stream && !isEventStreamContentType(result.Header.Get("Content-Type")) {
			raw, readErr := io.ReadAll(io.LimitReader(result.Body, maxJSONResponseTransferBytes+1))
			if readErr != nil {
				if anthropic {
					writeAnthropicError(c, http.StatusBadGateway, "api_error", "读取上游错误响应失败", "upstream_error")
				} else {
					writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "读取上游错误响应失败")
				}
				return
			}
			c.Writer.Header().Del("Content-Length")
			code, message := gateway.ClassifyUpstreamHTTPError(result.StatusCode, raw)
			errorCode = code
			if anthropic {
				writeAnthropicError(c, result.StatusCode, anthropicUpstreamHTTPErrorType(result.StatusCode), message, errorCode)
			} else {
				writeOpenAIError(c, result.StatusCode, errorCode, message)
			}
			return
		}
	}
	c.Status(result.StatusCode)
	var err error
	if stream {
		metadata, copyErr := copyStreamWithFallbackModel(c.Writer, result.Body, protocol, result.MarkFirstToken, fallbackModel)
		usage, responseID, err = metadata.Usage, metadata.ResponseID, copyErr
		if metadata.StreamFailure != nil && result.RecordStreamFailure != nil {
			result.RecordStreamFailure(*metadata.StreamFailure)
		}
	} else {
		metadata, copyErr := copyJSON(c.Writer, body, protocol)
		usage, responseID, err = metadata.Usage, metadata.ResponseID, copyErr
	}
	if err != nil {
		errorCode = classifyCopyError(c.Request.Context(), err)
	}
}

func classifyCopyError(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	if neterror.IsClientRequestCancel(ctx, err) {
		return "client_stream_interrupted"
	}
	switch {
	case errors.Is(err, errResponseTransferLimit):
		return "response_too_large"
	case errors.Is(err, errUpstreamStreamFailed):
		return "upstream_stream_error"
	case errors.Is(err, errUpstreamStreamIncomplete):
		return "upstream_stream_incomplete"
	case errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout):
		return "upstream_stream_idle_timeout"
	case errors.Is(err, neterror.ErrUpstreamResponseEmpty):
		return "upstream_response_empty"
	case errors.Is(err, neterror.ErrUpstreamOutputLoop):
		return "upstream_output_loop"
	case errors.Is(err, errUpstreamStreamRead):
		return "upstream_stream_interrupted"
	default:
		return "stream_interrupted"
	}
}

// peekNonEmptyJSONBody delays the downstream 2xx status until the upstream has
// produced at least one response byte. This lets an idle/empty non-streaming
// response become a real 502/504 instead of an empty 200 while preserving a
// streaming copy for large valid JSON bodies.
func peekNonEmptyJSONBody(source io.Reader) (io.Reader, error) {
	reader := bufio.NewReaderSize(source, responseCopyBufferBytes)
	if _, err := reader.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, neterror.ErrUpstreamResponseEmpty
		}
		return nil, err
	}
	return reader, nil
}

type responseMetadata struct {
	Usage                    gateway.Usage
	cacheCreationInputTokens int64
	ResponseID               string
	Model                    string
	SequenceNumber           int64
	StreamFailure            *gateway.StreamFailureDiagnostic
}

func copyStream(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol, onFirstToken func()) (responseMetadata, error) {
	return copyStreamWithFallbackModel(writer, source, protocol, onFirstToken, "")
}

func copyStreamWithFallbackModel(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol, onFirstToken func(), fallbackModel string) (responseMetadata, error) {
	inspector := &responseInspector{protocol: protocol, onFirstToken: onFirstToken}
	markerFilter := internalSSEMarkerFilter{enabled: protocol == streamProtocolChat || protocol == streamProtocolAnthropic}
	var compat responsesCompatState
	compat.model = strings.TrimSpace(fallbackModel)
	buffer := make([]byte, responseCopyBufferBytes)
	received := 0
	transferred := 0
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if received+n > maxStreamResponseTransferBytes {
				return inspector.Metadata(), fmt.Errorf("%w: 流式响应超过 %d MiB", errResponseTransferLimit, maxStreamResponseTransferBytes>>20)
			}
			received += n
			chunk := buffer[:n]
			if protocol == streamProtocolChat {
				// The internal reasoning marker is intentionally removed before
				// forwarding, but still counts as generation start.
				inspector.Inspect(chunk)
			}
			chunk = markerFilter.Filter(chunk, false)
			if protocol == streamProtocolResponses {
				chunk = rewriteResponsesStreamChunk(chunk, &compat)
			}
			if protocol != streamProtocolChat {
				// Inspect the actual downstream representation so compatibility
				// fields such as generated item IDs participate in timing and
				// output-observed classification.
				inspector.Inspect(chunk)
			}
			if transferred+len(chunk) > maxStreamResponseTransferBytes {
				return inspector.Metadata(), fmt.Errorf("%w: 流式响应超过 %d MiB", errResponseTransferLimit, maxStreamResponseTransferBytes>>20)
			}
			if len(chunk) > 0 {
				if err := setResponseWriteDeadline(writer); err != nil {
					return inspector.Metadata(), err
				}
				if _, err := writer.Write(chunk); err != nil {
					return inspector.Metadata(), err
				}
				writer.Flush()
				transferred += len(chunk)
			}
			inspector.markFirstTokenForwarded()
		}
		if readErr != nil {
			if tail := markerFilter.Filter(nil, true); len(tail) > 0 {
				if transferred+len(tail) > maxStreamResponseTransferBytes {
					return inspector.Metadata(), fmt.Errorf("%w: 流式响应超过 %d MiB", errResponseTransferLimit, maxStreamResponseTransferBytes>>20)
				}
				if err := setResponseWriteDeadline(writer); err != nil {
					return inspector.Metadata(), err
				}
				if _, err := writer.Write(tail); err != nil {
					return inspector.Metadata(), err
				}
				writer.Flush()
				transferred += len(tail)
			}
			if protocol == streamProtocolResponses {
				if tail := flushResponsesStreamTail(&compat); len(tail) > 0 {
					inspector.Inspect(tail)
					if transferred+len(tail) > maxStreamResponseTransferBytes {
						return inspector.Metadata(), fmt.Errorf("%w: 流式响应超过 %d MiB", errResponseTransferLimit, maxStreamResponseTransferBytes>>20)
					}
					if err := setResponseWriteDeadline(writer); err != nil {
						return inspector.Metadata(), err
					}
					if _, err := writer.Write(tail); err != nil {
						return inspector.Metadata(), err
					}
					writer.Flush()
					transferred += len(tail)
				}
			}
			inspector.Finish()
			inspector.markFirstTokenForwarded()
			terminalErr := inspector.TerminalError()
			if terminalErr == nil || errors.Is(terminalErr, errUpstreamStreamFailed) {
				return inspector.Metadata(), terminalErr
			}
			if errors.Is(readErr, io.EOF) {
				writeStreamAbortTrailer(writer, protocol, terminalErr, inspector.Metadata(), &compat, transferred)
				return inspector.Metadata(), terminalErr
			}
			writeStreamAbortTrailer(writer, protocol, readErr, inspector.Metadata(), &compat, transferred)
			return inspector.Metadata(), fmt.Errorf("%w: %w", errUpstreamStreamRead, readErr)
		}
	}
}

func writeStreamAbortTrailer(writer gin.ResponseWriter, protocol streamProtocol, cause error, meta responseMetadata, compat *responsesCompatState, transferred int) {
	trailer := streamAbortTrailer(protocol, cause, meta, compat)
	if len(trailer) == 0 || transferred+len(trailer) > maxStreamResponseTransferBytes {
		return
	}
	if err := setResponseWriteDeadline(writer); err != nil {
		return
	}
	if _, err := writer.Write(trailer); err == nil {
		writer.Flush()
	}
}

func streamAbortTrailer(protocol streamProtocol, cause error, meta responseMetadata, compat *responsesCompatState) []byte {
	code, message := "upstream_stream_interrupted", "上游流式响应中断"
	switch {
	case errors.Is(cause, neterror.ErrUpstreamStreamIdleTimeout):
		code, message = "upstream_stream_idle_timeout", "上游流式响应长时间无数据"
	case errors.Is(cause, neterror.ErrUpstreamOutputLoop):
		code, message = "upstream_output_loop", "上游输出陷入循环"
	case errors.Is(cause, errUpstreamStreamIncomplete):
		code, message = "upstream_stream_incomplete", "上游流式响应未完整结束"
	}
	switch protocol {
	case streamProtocolChat:
		payload, err := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"code":    code,
				"message": message,
				"type":    "server_error",
			},
		})
		if err != nil {
			return []byte("data: [DONE]\n\n")
		}
		return []byte("data: " + string(payload) + "\n\ndata: [DONE]\n\n")
	case streamProtocolResponses:
		if compat == nil {
			compat = &responsesCompatState{}
		}
		compat.rememberFromMeta(meta)
		id := compat.ensureID()
		// Grok TUI 0.2.93 treats any response.incomplete as fatal
		// max_tokens_truncation (not retryable), ignoring incomplete_details.reason.
		// Stream aborts are transport failures — emit response.failed so the
		// client can retry instead of killing the turn.
		model := strings.TrimSpace(meta.Model)
		if model == "" {
			model = strings.TrimSpace(compat.model)
		}
		response := map[string]any{
			"id":           id,
			"object":       "response",
			"created_at":   compat.createdAt,
			"completed_at": compat.createdAt,
			"status":       "failed",
			"model":        model,
			"output":       []any{},
			"error": map[string]any{
				"code":    "server_error",
				"message": code + ": " + message,
			},
		}
		event := map[string]any{
			"type":            "response.failed",
			"id":              id,
			"sequence_number": meta.SequenceNumber + 1,
			"response":        response,
		}
		sanitizeResponsesEvent(event, compat)
		payload, err := json.Marshal(event)
		if err != nil {
			return nil
		}
		return []byte("event: response.failed\ndata: " + string(payload) + "\n\n")
	case streamProtocolAnthropic:
		anthropicMessage := message
		if code == "upstream_output_loop" {
			anthropicMessage = code + ": " + message
		}
		payload, err := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": anthropicMessage},
		})
		if err != nil {
			return nil
		}
		return []byte("event: error\ndata: " + string(payload) + "\n\n")
	default:
		return nil
	}
}

type internalSSEMarkerFilter struct {
	enabled bool
	pending []byte
}

func (f *internalSSEMarkerFilter) Filter(chunk []byte, final bool) []byte {
	if !f.enabled {
		return chunk
	}
	f.pending = append(f.pending, chunk...)
	result := make([]byte, 0, len(f.pending))
	for {
		index, markerLength := nextInternalSSEMarker(f.pending)
		if index >= 0 {
			result = append(result, f.pending[:index]...)
			f.pending = f.pending[index+markerLength:]
			continue
		}
		if final {
			result = append(result, f.pending...)
			f.pending = nil
			return result
		}
		keep := 0
		for _, marker := range internalSSEMarkers {
			limit := min(len(f.pending), len(marker)-1)
			for size := limit; size > keep; size-- {
				if bytes.Equal(f.pending[len(f.pending)-size:], marker[:size]) {
					keep = size
					break
				}
			}
		}
		result = append(result, f.pending[:len(f.pending)-keep]...)
		f.pending = f.pending[len(f.pending)-keep:]
		return result
	}
}

func nextInternalSSEMarker(value []byte) (int, int) {
	index := -1
	length := 0
	for _, marker := range internalSSEMarkers {
		candidate := bytes.Index(value, marker)
		if candidate >= 0 && (index < 0 || candidate < index) {
			index = candidate
			length = len(marker)
		}
	}
	return index, length
}

func copyJSON(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol) (responseMetadata, error) {
	buffer := make([]byte, responseCopyBufferBytes)
	metadataBody := make([]byte, 0, responseCopyBufferBytes)
	metadataComplete := true
	transferred := 0
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if transferred+n > maxJSONResponseTransferBytes {
				return responseMetadata{}, fmt.Errorf("%w: 非流式响应超过 %d MiB", errResponseTransferLimit, maxJSONResponseTransferBytes>>20)
			}
			chunk := buffer[:n]
			if err := setResponseWriteDeadline(writer); err != nil {
				return responseMetadata{}, err
			}
			if _, err := writer.Write(chunk); err != nil {
				return responseMetadata{}, err
			}
			transferred += n
			if metadataComplete {
				if len(metadataBody)+len(chunk) <= maxJSONMetadataInspectionBytes {
					metadataBody = append(metadataBody, chunk...)
				} else {
					metadataBody = nil
					metadataComplete = false
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if transferred == 0 {
					return responseMetadata{}, neterror.ErrUpstreamResponseEmpty
				}
				if metadataComplete {
					return normalizeMetadataUsage(extractMetadata(metadataBody), protocol), nil
				}
				return responseMetadata{}, nil
			}
			return responseMetadata{Usage: gateway.Usage{OutputObserved: transferred > 0}}, readErr
		}
	}
}

func copyHeaders(destination, source http.Header) {
	excluded := map[string]struct{}{
		"connection": {}, "content-length": {}, "keep-alive": {}, "proxy-authenticate": {},
		"proxy-authorization": {}, "set-cookie": {}, "te": {}, "trailer": {},
		"transfer-encoding": {}, "upgrade": {}, "x-models-etag": {},
	}
	for _, value := range source.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" {
				excluded[name] = struct{}{}
			}
		}
	}
	for name, values := range source {
		lower := strings.ToLower(name)
		if _, skip := excluded[lower]; skip {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func isEventStreamContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}
