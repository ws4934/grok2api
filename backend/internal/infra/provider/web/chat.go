package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/toolemu"
)

const webResponseTTL = 30 * 24 * time.Hour

const maxTrackedServerTools = 1024

const (
	maxTrackedCitationSources = 256
	maxTrackedAnnotations     = 2048
)

var (
	errWebAntiBot    = errors.New("Grok Web anti-bot rejection")
	errWebUsageLimit = errors.New("Grok Web usage limit reached")
)

var (
	grokRenderPattern   = regexp.MustCompile(`(?s)<grok:render\s+card_id="([^"]+)"\s+card_type="([^"]+)"\s+type="([^"]+)"[^>]*>.*?</grok:render>`)
	grokToolNamePattern = regexp.MustCompile(`(?is)<xai:tool_name>\s*(.*?)\s*</xai:tool_name>`)
)

type openAIRequest struct {
	Model              string          `json:"model"`
	Stream             bool            `json:"stream"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions"`
	PreviousResponseID string          `json:"previous_response_id"`
	Messages           []chatMessage   `json:"messages"`
	// Include is the xAI/OpenAI Responses include list (inline_citations / no_inline_citations).
	Include           []string        `json:"include"`
	Tools             json.RawMessage `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	ImageConfig       *struct {
		Count          *int   `json:"n"`
		ResponseFormat string `json:"response_format"`
		AspectRatio    string `json:"aspect_ratio"`
		Resolution     string `json:"resolution"`
	} `json:"image_config"`
}

type chatMessage struct {
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	CallID     string          `json:"call_id"`
	Name       string          `json:"name"`
	Arguments  string          `json:"arguments"`
	Output     json.RawMessage `json:"output"`
}

type normalizedChatInput struct {
	Prompt      string
	Attachments []chatAttachmentInput
}

type chatAttachmentInput struct {
	Source   string
	Filename string
	Image    bool
}

// hostedSearchCall tracks one mgw web_search / x_search invocation for xAI Responses output items.
type hostedSearchCall struct {
	ID      string
	Kind    string // web_search | x_search
	Query   string
	Status  string // in_progress | completed
	Sources []map[string]any
}

// trackedTextBuilder keeps OpenAI citation offsets in Unicode characters
// without rescanning the complete response for every citation.
type trackedTextBuilder struct {
	builder    strings.Builder
	characters int
}

func (b *trackedTextBuilder) WriteString(value string) (int, error) {
	written, err := b.builder.WriteString(value)
	b.characters += utf8.RuneCountInString(value[:written])
	return written, err
}

func (b *trackedTextBuilder) Reset() {
	b.builder.Reset()
	b.characters = 0
}

func (b *trackedTextBuilder) String() string { return b.builder.String() }

func (b *trackedTextBuilder) Len() int { return b.builder.Len() }

func (b *trackedTextBuilder) CharacterLen() int { return b.characters }

type parsedChat struct {
	ResponseID     string
	ConversationID string
	ParentID       string
	Text           trackedTextBuilder
	upstreamText   strings.Builder
	Reasoning      strings.Builder
	Images         []string
	SearchSources  []map[string]any
	Annotations    []map[string]any
	// ResponseOutput is populated by the Responses streaming state machine so
	// response.completed reuses the exact item IDs and ordering emitted in SSE.
	ResponseOutput []any
	// HostedSearchCalls are ordered web_search_call / x_search_call items (xAI Responses).
	HostedSearchCalls []hostedSearchCall
	hostedSearchByID  map[string]int
	// InlineCitations mirrors xAI default-on [[N]](url) embedding.
	DisableInlineCitations bool
	sourceKeys             map[string]struct{}
	serverToolKeys         map[string]struct{}
	webSearchKeys          map[string]struct{}
	xSearchKeys            map[string]struct{}
	cardCache              map[string]map[string]any
	moderatedImages        map[string]struct{}
	citationIndex          map[string]int
	lastCitation           int
	ServerTools            int64
	WebSearchTools         int64
	XSearchTools           int64
	InputTokens            int64
	ToolCalls              []toolemu.Call
	Tools                  []any
	ToolChoice             any
	ParallelTools          bool
}

func (p *parsedChat) textCharacterLen() int {
	if p == nil {
		return 0
	}
	return p.Text.CharacterLen()
}

func (p *parsedChat) appendText(value string) {
	if p == nil || value == "" {
		return
	}
	p.Text.WriteString(value)
}

func (p *parsedChat) resetText(value string) {
	if p == nil {
		return
	}
	p.Text.Reset()
	p.Text.WriteString(value)
}

func (a *Adapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	if request.Method == http.MethodGet || request.Method == http.MethodDelete {
		return a.handleResponseResource(ctx, request)
	}
	if request.Path == "/responses/compact" {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
			"type": "invalid_request_error", "code": "unsupported_operation",
			"message": "Grok Web 模型不支持 /responses/compact",
		}}), nil
	}
	if request.Method != http.MethodPost {
		return jsonProviderResponse(http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}}), nil
	}
	var conversationOptions conversation.ResponseOptions
	if request.Operation == conversation.OperationMessages {
		converted, options, err := conversation.ConvertRequestWithOptions(request.Body, request.Model, request.Operation)
		if err != nil {
			return jsonProviderResponse(http.StatusBadRequest, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}}), nil
		}
		request.Body = converted
		conversationOptions = options
	}

	var input openAIRequest
	if err := json.Unmarshal(request.Body, &input); err != nil {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "请求 JSON 无效", "type": "invalid_request_error"}}), nil
	}
	if len(input.Include) > 0 {
		conversationOptions.Include = append([]string{}, input.Include...)
	}
	spec, modelKnown := Resolve(request.Model)
	var normalized normalizedChatInput
	var err error
	if modelKnown && spec.Capability == modeldomain.CapabilityImage {
		normalized, err = normalizeLatestImageInput(input, request.Operation)
	} else {
		normalized, err = normalizeOpenAIInput(input, request.Operation)
	}
	if err != nil {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"}}), nil
	}
	tools, err := parseToolConfiguration(input.Tools, input.ToolChoice)
	if err != nil {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error", "code": "invalid_tools"}}), nil
	}
	parallelTools := true
	if input.ParallelToolCalls != nil {
		parallelTools = *input.ParallelToolCalls
	}
	if modelKnown && spec.Capability == modeldomain.CapabilityImage {
		if len(tools.ResponseTools) > 0 {
			return invalidImageRequest("图片生成模型不支持 tools")
		}
		return a.forwardImageChatCompletion(ctx, request, input, normalized, spec)
	}
	if modelKnown && spec.Capability == modeldomain.CapabilityImageEdit {
		return invalidImageRequest("图片编辑模型请使用 /v1/images/edits，并在当前请求中显式提供输入图片")
	}
	if !modelKnown || spec.Capability != modeldomain.CapabilityChat {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "模型不支持文本对话", "type": "invalid_request_error"}}), nil
	}

	normalized.Prompt = injectToolPrompt(normalized.Prompt, tools)
	responseID := newWebID("resp")
	streaming := input.Stream || request.Streaming
	upstream, lease, previous, openErr := a.openGatewayChat(ctx, request.Credential, input.PreviousResponseID, spec, normalized, gatewayOpenOptions{enforceStreamIdle: true})
	if openErr != nil {
		if errors.Is(openErr, errInvalidChatAttachment) || errors.Is(openErr, errInvalidChatImage) || errors.Is(openErr, errInvalidChatFile) {
			code := "invalid_attachment_input"
			if errors.Is(openErr, errInvalidChatImage) {
				code = "invalid_image_input"
			}
			return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
				"message": openErr.Error(), "type": "invalid_request_error", "code": code,
			}}), nil
		}
		return nil, openErr
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		if upstream.StatusCode == http.StatusForbidden {
			// A definitive account block must not penalize the egress node or its clearance.
			body, readErr := io.ReadAll(io.LimitReader(upstream.Body, 4<<20))
			_ = upstream.Body.Close()
			if readErr != nil {
				lease.Release()
				return nil, readErr
			}
			if provider.IsDefinitiveAccountBlockBody(body) {
				return &provider.Response{
					StatusCode: upstream.StatusCode, Status: upstream.Status, Header: http.Header(upstream.Header),
					UpstreamURL: responseUpstreamURL(upstream),
					Body: &releaseBody{ReadCloser: io.NopCloser(bytes.NewReader(body)), release: func() {
						lease.Release()
					}},
				}, nil
			}
			lease.InvalidateClearance()
			return &provider.Response{
				StatusCode: upstream.StatusCode, Status: upstream.Status, Header: http.Header(upstream.Header),
				UpstreamURL: responseUpstreamURL(upstream),
				Body: &releaseBody{ReadCloser: io.NopCloser(bytes.NewReader(body)), release: func() {
					a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, upstream.StatusCode, nil)
					lease.Release()
				}},
			}, nil
		}
		return &provider.Response{
			StatusCode: upstream.StatusCode, Status: upstream.Status, Header: http.Header(upstream.Header),
			UpstreamURL: responseUpstreamURL(upstream),
			Body: &releaseBody{ReadCloser: upstream.Body, release: func() {
				a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, upstream.StatusCode, nil)
				lease.Release()
			}},
		}, nil
	}

	if streaming {
		prepared, preflightErr := preflightUpstream(upstream.Body)
		if preflightErr == nil {
			body := a.streamOpenAIResponse(ctx, prepared, lease, request.Credential, responseID, input.Model, request.Operation, normalized.Prompt, previous, tools, parallelTools, conversationOptions)
			return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: body}, nil
		}
		_ = upstream.Body.Close()
		lease.Release()
		if errors.Is(preflightErr, errWebAntiBot) {
			a.feedbackAntiBot(ctx, lease)
			return antiBotProviderResponse(), nil
		}
		return nil, preflightErr
	}

	parsed, consumeErr := consumeUpstreamWithCitations(upstream.Body, nil, conversationOptions.InlineCitationsEnabled())
	_ = upstream.Body.Close()
	lease.Release()
	if consumeErr != nil {
		a.feedbackStreamError(ctx, lease, consumeErr)
		if errors.Is(consumeErr, errWebAntiBot) {
			return antiBotProviderResponse(), nil
		}
		return nil, consumeErr
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
	parsed.InputTokens = estimateTokens(normalized.Prompt)
	parsed.Tools = tools.ResponseTools
	parsed.ToolChoice = tools.ResponseChoice
	parsed.ParallelTools = parallelTools
	applyParsedToolCalls(&parsed, tools)
	if err := a.archiveChatImages(ctx, request.Credential, &parsed); err != nil {
		return nil, err
	}
	payload := buildOpenAIResult(request.Operation, responseID, input.Model, parsed, false, conversationOptions)
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if request.Operation == conversation.OperationResponses {
		a.saveResponseState(context.WithoutCancel(ctx), request.Credential.ID, responseID, parsed, data)
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func (a *Adapter) feedbackAntiBot(ctx context.Context, lease *infraegress.Lease) {
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusForbidden, nil)
}

// feedbackStreamError reports a stream that failed after the upstream accepted
// it. Only a failed read is an egress transport failure and anti-bot keeps its
// 403; any other error the upstream sent arrived over a working egress.
func (a *Adapter) feedbackStreamError(ctx context.Context, lease *infraegress.Lease, err error) {
	var readErr webStreamReadError
	switch {
	case errors.As(err, &readErr):
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
	case errors.Is(err, errWebAntiBot):
		a.feedbackAntiBot(ctx, lease)
	default:
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
	}
}

func preflightUpstream(source io.ReadCloser) (io.ReadCloser, error) {
	reader := bufio.NewReaderSize(source, 64<<10)
	var prefetched bytes.Buffer
	for prefetched.Len() <= 1<<20 {
		line, err := reader.ReadString('\n')
		if line != "" {
			prefetched.WriteString(line)
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			}
			if strings.HasPrefix(trimmed, "{") {
				var root map[string]any
				if json.Unmarshal([]byte(trimmed), &root) == nil {
					if errorValue, ok := root["error"].(map[string]any); ok {
						return nil, webResponseError(errorValue)
					}
					if event, ok := root["event"].(map[string]any); ok {
						if event["type"] == "error" {
							return nil, gatewayEventError(event)
						}
						return &readerCloser{Reader: io.MultiReader(bytes.NewReader(prefetched.Bytes()), reader), closer: source}, nil
					}
					if result, ok := root["result"].(map[string]any); ok && (result["conversation"] != nil || result["response"] != nil) {
						return &readerCloser{Reader: io.MultiReader(bytes.NewReader(prefetched.Bytes()), reader), closer: source}, nil
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && prefetched.Len() > 0 {
				return &readerCloser{Reader: bytes.NewReader(prefetched.Bytes()), closer: source}, nil
			}
			return nil, err
		}
	}
	return nil, fmt.Errorf("Grok Web 首个流事件超过安全检查上限")
}

func (a *Adapter) handleResponseResource(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	id := strings.TrimPrefix(request.Path, "/responses/")
	if before, _, ok := strings.Cut(id, "?"); ok {
		id = before
	}
	id, _ = url.PathUnescape(id)
	if request.Method == http.MethodDelete {
		if err := a.states.DeleteWebState(ctx, id); err != nil {
			return jsonProviderResponse(http.StatusNotFound, map[string]any{"error": map[string]any{"message": "Response 不存在", "type": "invalid_request_error"}}), nil
		}
		return jsonProviderResponse(http.StatusOK, map[string]any{"id": id, "object": "response.deleted", "deleted": true}), nil
	}
	state, err := a.states.GetWebState(ctx, id, time.Now().UTC())
	if err != nil {
		return jsonProviderResponse(http.StatusNotFound, map[string]any{"error": map[string]any{"message": "Response 不存在或已过期", "type": "invalid_request_error"}}), nil
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: io.NopCloser(strings.NewReader(state.ResponseJSON))}, nil
}

func (a *Adapter) saveResponseState(ctx context.Context, accountID uint64, responseID string, parsed parsedChat, data []byte) {
	if parsed.ConversationID == "" || parsed.ParentID == "" || a.states == nil {
		return
	}
	now := time.Now().UTC()
	_ = a.states.SaveWebState(ctx, inferencedomain.WebResponseState{
		ResponseID: responseID, AccountID: accountID, ConversationID: parsed.ConversationID,
		UpstreamParentResponseID: parsed.ParentID, ResponseJSON: string(data), Status: "completed",
		ExpiresAt: now.Add(webResponseTTL), CreatedAt: now, UpdatedAt: now,
	})
}

func estimateTokens(value string) int64 {
	count := utf8.RuneCountInString(value)
	if count == 0 {
		return 0
	}
	return int64((count + 3) / 4)
}

func newWebID(prefix string) string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return prefix + "_" + hex.EncodeToString(value)
}

func streamHeaders() http.Header {
	value := http.Header{}
	value.Set("Content-Type", "text/event-stream; charset=utf-8")
	value.Set("Cache-Control", "no-cache")
	value.Set("X-Accel-Buffering", "no")
	return value
}

func jsonHeaders() http.Header {
	value := http.Header{}
	value.Set("Content-Type", "application/json; charset=utf-8")
	return value
}

func jsonProviderResponse(status int, value any) *provider.Response {
	data, _ := json.Marshal(value)
	return &provider.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(data))}
}

type releaseBody struct {
	io.ReadCloser
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.release != nil {
		b.release()
		b.release = nil
	}
	return err
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

type readerCloser struct {
	io.Reader
	closer io.Closer
}

func (r *readerCloser) Close() error { return r.closer.Close() }

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	return err
}
