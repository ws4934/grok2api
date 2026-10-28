package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type imagineModelConfig struct {
	Pro            bool
	ExpectedCount  int
	MaxReturnCount int
}

type imagineImageValue struct {
	ID       string
	URL      string
	Blob     string
	Position int
	Width    int
	Height   int
	position bool
}

type imagineSlot struct {
	image          imagineImageValue
	preview        imagineImageValue
	final          bool
	previewReady   bool
	previewEmitted bool
	completed      bool
	moderated      bool
	emitted        bool
}

type imagineCollector struct {
	requestID     string
	slots         map[string]*imagineSlot
	terminalCount int
}

func resolveImagineModel(model string, pro bool, count int) (imagineModelConfig, bool) {
	if model != "imagine" {
		return imagineModelConfig{}, false
	}
	return imagineModelConfig{Pro: pro, ExpectedCount: count, MaxReturnCount: 10}, true
}

func newImagineCollector(requestID ...string) *imagineCollector {
	var reqID string
	if len(requestID) > 0 {
		reqID = strings.TrimSpace(requestID[0])
	}
	return &imagineCollector{requestID: reqID, slots: make(map[string]*imagineSlot)}
}

func (c *imagineCollector) Accept(message map[string]any) {
	typeName, _ := message["type"].(string)
	if typeName != "image" && typeName != "json" {
		return
	}
	if c.requestID != "" {
		msgReqID := firstString(message, "requestId", "request_id", "req_id", "reqId")
		if msgReqID == "" {
			if item, ok := message["item"].(map[string]any); ok {
				msgReqID = firstString(item, "requestId", "request_id")
			}
		}
		if msgReqID != c.requestID {
			return
		}
	}
	rawURL, _ := message["url"].(string)
	imageID := firstString(message, "image_id", "job_id", "id")
	if imageID == "" && rawURL != "" {
		imageID = imageIDFromURL(rawURL)
	}
	if imageID == "" {
		return
	}
	slot := c.slots[imageID]
	if slot == nil {
		slot = &imagineSlot{image: imagineImageValue{ID: imageID}}
		c.slots[imageID] = slot
	}
	if typeName == "image" {
		if position, ok := firstInt(message, "side_by_side_index", "order", "grid_index"); ok {
			slot.image.Position = position
			slot.image.position = true
		}
		width, _ := numberAsInt(message["width"])
		height, _ := numberAsInt(message["height"])
		progress, hasProgress := numberAsInt(message["percentage_complete"])
		if hasProgress && progress < 100 {
			slot.preview = imagineImageValue{
				ID: imageID, URL: absoluteAssetURL(rawURL), Position: slot.image.Position,
				Width: width, Height: height, position: slot.image.position,
			}
			slot.preview.Blob, _ = message["blob"].(string)
			slot.previewReady = true
			return
		}
		slot.image.URL = absoluteAssetURL(rawURL)
		slot.image.Blob, _ = message["blob"].(string)
		slot.image.Width = width
		slot.image.Height = height
		slot.final = true
		return
	}
	status, _ := message["current_status"].(string)
	if position, ok := numberAsInt(message["order"]); ok && !slot.image.position {
		slot.image.Position = position
		slot.image.position = true
	}
	if width, ok := numberAsInt(message["width"]); ok && slot.image.Width == 0 {
		slot.image.Width = width
	}
	if height, ok := numberAsInt(message["height"]); ok && slot.image.Height == 0 {
		slot.image.Height = height
	}
	if status != "completed" {
		return
	}
	if rawURL != "" && !slot.final {
		slot.image.URL = absoluteAssetURL(rawURL)
		slot.image.Blob, _ = message["blob"].(string)
		slot.final = true
	}
	if !slot.completed {
		slot.completed = true
		c.terminalCount++
	}
	slot.moderated, _ = message["moderated"].(bool)
}

func (c *imagineCollector) Done(expected int) bool {
	if expected <= 0 || c.terminalCount < expected {
		return false
	}
	for _, slot := range c.slots {
		if slot.completed && !slot.moderated && (!slot.final || (slot.image.URL == "" && slot.image.Blob == "")) {
			return false
		}
	}
	return true
}

func (c *imagineCollector) Images() []imagineImageValue {
	values := make([]imagineImageValue, 0, len(c.slots))
	for _, slot := range c.slots {
		if slot.completed && !slot.moderated && slot.final && (slot.image.URL != "" || slot.image.Blob != "") {
			values = append(values, slot.image)
		}
	}
	sortImagineImages(values)
	return values
}

func (c *imagineCollector) ReadyImages() []imagineImageValue {
	values := make([]imagineImageValue, 0, len(c.slots))
	for _, slot := range c.slots {
		if slot.completed && !slot.moderated && slot.final && !slot.emitted && (slot.image.URL != "" || slot.image.Blob != "") {
			slot.emitted = true
			values = append(values, slot.image)
		}
	}
	sortImagineImages(values)
	return values
}

func (c *imagineCollector) ReadyPreviews() []imagineImageValue {
	values := make([]imagineImageValue, 0)
	for _, slot := range c.slots {
		if slot.previewReady && !slot.previewEmitted {
			slot.previewEmitted = true
			values = append(values, slot.preview)
		}
	}
	sortImagineImages(values)
	return values
}

func sortImagineImages(values []imagineImageValue) {
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].position != values[j].position {
			return values[i].position
		}
		if values[i].Position != values[j].Position {
			return values[i].Position < values[j].Position
		}
		return values[i].ID < values[j].ID
	})
}

func (c *imagineCollector) UsableCount() int {
	count := 0
	for _, slot := range c.slots {
		if slot.completed && !slot.moderated && slot.final && (slot.image.URL != "" || slot.image.Blob != "") {
			count++
		}
	}
	return count
}

// A completed, entirely moderated batch is a policy outcome, not an empty
// transport response. Keep only counts in diagnostics, never blocked images.
type imagineModerationError struct{ count int }

func (e *imagineModerationError) Error() string {
	return fmt.Sprintf("Imagine WebSocket 内容未通过上游安全审核（completed=%d, moderated=%d）", e.count, e.count)
}

func (*imagineModerationError) HTTPStatusCode() int        { return http.StatusBadRequest }
func (*imagineModerationError) RequestScopedFailure() bool { return true }
func (*imagineModerationError) PublicErrorMessage() string { return provider.ContentPolicyMessage }

func (c *imagineCollector) moderationError() error {
	completed, moderated := 0, 0
	for _, slot := range c.slots {
		if slot.completed {
			completed++
			if slot.moderated {
				moderated++
			}
		}
	}
	if completed > 0 && moderated == completed {
		return &imagineModerationError{count: moderated}
	}
	return nil
}

func (a *Adapter) generateWSImage(ctx context.Context, request provider.ImageGenerationRequest, count int, format, ratio string, modelConfig imagineModelConfig) (*provider.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		response, err := a.generateWSImageAttempt(ctx, request, count, format, ratio, modelConfig)
		if err == nil {
			return response, nil
		}
		var upstreamErr *webMediaUpstreamError
		if !errors.As(err, &upstreamErr) || !isClearanceRefreshableMediaError(upstreamErr) || attempt > 0 {
			if errors.As(err, &upstreamErr) {
				return upstreamErr.providerResponse(), nil
			}
			return nil, err
		}
		a.log().Warn("web_image_clearance_retry", "operation", "imagine", "status", upstreamErr.status, "body_kind", upstreamErr.bodyKind)
	}
	return nil, fmt.Errorf("Imagine WebSocket Clearance 重试耗尽")
}

func (a *Adapter) generateWSImageAttempt(ctx context.Context, request provider.ImageGenerationRequest, count int, format, ratio string, modelConfig imagineModelConfig) (*provider.Response, error) {
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, request.Credential)
	if err != nil {
		return nil, err
	}
	leaseOwned := true
	defer func() {
		if leaseOwned {
			lease.Release()
		}
	}()
	wsURL, err := imagineURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	headers := fhttp.Header{}
	headers.Set("Origin", cfg.BaseURL)
	headers.Set("User-Agent", lease.UserAgent)
	headers.Set("Cookie", egress.BuildSSOCookie(token, lease.CFCookies))
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	connection, response, err := lease.DialWebSocketDeferredForbidden(ctx, wsURL, headers, 30*time.Second)
	if err != nil {
		if response != nil {
			var body []byte
			if response.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
				_ = response.Body.Close()
			}
			truncated := len(body) > webMediaDiagnosticBodyLimit
			if truncated {
				body = body[:webMediaDiagnosticBodyLimit]
			}
			upstreamErr := newWebMediaUpstreamError(response.StatusCode, body, truncated)
			if isClearanceRefreshableMediaError(upstreamErr) {
				lease.InvalidateClearance()
			}
			a.logWebMediaUpstreamRejection("image_imagine_handshake", &http.Response{
				StatusCode: response.StatusCode,
				Header:     http.Header(response.Header).Clone(),
			}, upstreamErr)
			if response.StatusCode != http.StatusForbidden {
				a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, err)
			}
			return nil, upstreamErr
		}
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, fmt.Errorf("连接 Imagine WebSocket: %w", err)
	}
	connectionOwned := true
	defer func() {
		if connectionOwned {
			_ = connection.Close()
		}
	}()
	connection.SetReadLimit(64 << 20)
	deadline := time.Now().Add(time.Duration(cfg.ImageTimeoutSeconds) * time.Second)
	_ = connection.SetReadDeadline(deadline)
	_ = connection.SetWriteDeadline(deadline)
	if err := connection.WriteJSON(imagineResetMessage()); err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, err
	}
	reqID := newWebID("img")
	if err := connection.WriteJSON(imagineRequestMessage(reqID, request.Prompt, ratio, cfg.AllowNSFW, modelConfig.Pro, modelConfig.ExpectedCount)); err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return nil, err
	}
	if request.Streaming {
		reader, writer := io.Pipe()
		streamCtx, cancel := context.WithCancel(ctx)
		leaseOwned = false
		connectionOwned = false
		go a.streamImagineImages(streamCtx, writer, connection, lease, request.Credential, count, request.PartialImages, modelConfig, reqID)
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: &cancelBody{ReadCloser: reader, cancel: cancel}, QuotaUnits: count, UpstreamURL: wsURL}, nil
	}

	collector := newImagineCollector(reqID)
	var transportErr error
	for collector.UsableCount() < count && !collector.Done(modelConfig.ExpectedCount) {
		messageType, data, readErr := connection.ReadMessage()
		if readErr != nil {
			transportErr = readErr
			if ctx.Err() == nil && collector.UsableCount() > 0 {
				break // Keep final, unmoderated results; never replay the missing slots.
			}
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, readErr)
			return nil, fmt.Errorf("读取 Imagine WebSocket: %w", readErr)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var message map[string]any
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if message["type"] == "error" {
			if collector.UsableCount() > 0 {
				break
			}
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
			return nil, fmt.Errorf("Imagine WebSocket 返回错误")
		}
		collector.Accept(message)
	}
	if transportErr != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, transportErr)
	} else {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
	}
	images := collector.Images()
	if len(images) == 0 {
		if err := collector.moderationError(); err != nil {
			return nil, &url.Error{Op: "Imagine WebSocket", URL: wsURL, Err: err}
		}
		return nil, fmt.Errorf("Imagine WebSocket 完成但没有可用图片")
	}
	urls := make([]string, 0, len(images))
	blobs := make([]string, 0, len(images))
	for _, image := range images {
		urls = append(urls, image.URL)
		blobs = append(blobs, image.Blob)
	}
	result, err := a.imageResponse(ctx, request.Credential, urls, blobs, min(count, len(images)), format)
	if result != nil {
		result.QuotaUnits = len(images)
	}
	return result, err
}

func (a *Adapter) streamImagineImages(ctx context.Context, writer *io.PipeWriter, connection *websocket.Conn, lease *egress.Lease, credential account.Credential, count, partialImages int, modelConfig imagineModelConfig, requestID ...string) {
	defer lease.Release()
	defer connection.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-done:
		}
	}()
	collector := newImagineCollector(requestID...)
	emitted := 0
	partialIndex := 0
	for emitted < count {
		messageType, data, readErr := connection.ReadMessage()
		if readErr != nil {
			if ctx.Err() != nil {
				_ = writer.CloseWithError(ctx.Err())
				return
			}
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, readErr)
			_ = writer.CloseWithError(readErr)
			return
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var message map[string]any
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if message["type"] == "error" {
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
			_ = writer.CloseWithError(fmt.Errorf("Imagine WebSocket 返回错误"))
			return
		}
		collector.Accept(message)
		if partialImages > 0 {
			for _, image := range collector.ReadyPreviews() {
				if partialIndex >= partialImages {
					continue
				}
				raw, err := a.imageBytes(ctx, credential, image)
				if err != nil {
					if ctx.Err() != nil {
						_ = writer.CloseWithError(ctx.Err())
						return
					}
					continue
				}
				if err := writeSSE(writer, "image_generation.partial_image", openAIImageStreamEvent("image_generation.partial_image", image, raw, partialIndex)); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
				partialIndex++
			}
		}
		for _, image := range collector.ReadyImages() {
			if emitted >= count {
				break
			}
			raw, err := a.imageBytes(ctx, credential, image)
			if err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			if err := a.saveStreamImage(ctx, raw); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			if err := writeSSE(writer, "image_generation.completed", openAIImageStreamEvent("image_generation.completed", image, raw, 0)); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			emitted++
		}
		if collector.Done(modelConfig.ExpectedCount) && emitted < count {
			if emitted == 0 && collector.moderationError() != nil {
				if err := writeSSE(writer, "error", map[string]any{
					"type": "error", "error": map[string]any{
						"type": "invalid_request_error", "code": provider.ContentPolicyViolation,
						"message": provider.ContentPolicyMessage, "param": nil,
					},
				}); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
				a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
				_ = writer.Close()
				return
			}
			incompleteErr := fmt.Errorf("上游仅返回 %d/%d 张可用图片", emitted, count)
			_ = writer.CloseWithError(incompleteErr)
			return
		}
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
	_ = writer.Close()
}

func openAIImageStreamEvent(eventType string, image imagineImageValue, raw []byte, partialIndex int) map[string]any {
	width, height := image.Width, image.Height
	size := "auto"
	if width > 0 && height > 0 {
		size = fmt.Sprintf("%dx%d", width, height)
	}
	value := map[string]any{
		"type": eventType, "b64_json": base64.StdEncoding.EncodeToString(raw),
		"created_at": time.Now().Unix(), "size": size, "quality": "auto",
		"background": "auto", "output_format": imageOutputFormat(raw),
	}
	if eventType == "image_generation.partial_image" {
		value["partial_image_index"] = partialIndex
	}
	return value
}

func imagineURL(baseURL string) (string, error) {
	value, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	switch value.Scheme {
	case "https":
		value.Scheme = "wss"
	case "http":
		value.Scheme = "ws"
	default:
		return "", fmt.Errorf("Grok Web Base URL 协议无效")
	}
	value.Path = "/ws/imagine/listen"
	value.RawQuery = ""
	return value.String(), nil
}

func imagineResetMessage() map[string]any {
	return map[string]any{"type": "conversation.item.create", "timestamp": time.Now().UnixMilli(), "item": map[string]any{"type": "message", "content": []any{map[string]any{"type": "reset"}}}}
}

func imagineRequestMessage(id, prompt, ratio string, nsfw, pro bool, generations int) map[string]any {
	properties := map[string]any{
		"section_count": 0, "is_kids_mode": false, "enable_nsfw": nsfw,
		"skip_upsampler": pro, "enable_side_by_side": true, "is_initial": false,
		"aspect_ratio": ratio, "enable_pro": pro, "num_generations": generations,
	}
	if pro && (ratio == "" || ratio == "auto") {
		delete(properties, "aspect_ratio")
	}
	return map[string]any{"type": "conversation.item.create", "timestamp": time.Now().UnixMilli(), "item": map[string]any{"type": "message", "content": []any{map[string]any{"requestId": id, "text": prompt, "type": "input_text", "properties": properties}}}}
}

func imageIDFromURL(value string) string {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) == 0 {
		return value
	}
	name := parts[len(parts)-1]
	if index := strings.IndexByte(name, '.'); index > 0 {
		return name[:index]
	}
	return name
}
