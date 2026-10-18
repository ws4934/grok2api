// Package chatgpt serves ChatGPT Web accounts as a native gateway provider.
package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// Config holds the ChatGPT runtime settings the adapter reads per request.
type Config struct {
	// ImageConversationModel overrides the Web model that drives every image
	// route; empty uses each route's default (gpt-image-2 → gpt-5-3, 2.5 → auto).
	ImageConversationModel string
	OutputTokenLimit       int
	VisionTokens           int
}

type Adapter struct {
	mu       sync.RWMutex
	cfg      Config
	egress   *infraegress.Manager
	cipher   *security.Cipher
	assets   provider.ImageAssetStore
	image    *OpenAIImage
	oauth    *http.Client
	authURL  string
	tokenURL string
}

func NewAdapter(cfg Config, egress *infraegress.Manager, cipher *security.Cipher, assets provider.ImageAssetStore) *Adapter {
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Adapter{
		cfg: cfg, egress: egress, cipher: cipher, assets: assets,
		image:   &OpenAIImage{BaseURL: "https://chatgpt.com", Client: client, RequestTimeout: 10 * time.Minute, browsers: map[string]*browserHTTP{}, pow: make(chan struct{}, 2)},
		oauth:   client,
		authURL: "https://auth.openai.com/api/accounts/authorize", tokenURL: "https://auth.openai.com/oauth/token",
	}
}

func (a *Adapter) Provider() account.Provider { return account.ProviderChatGPT }

func (a *Adapter) UpdateConfig(cfg Config) {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
}

func (a *Adapter) config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// Close releases idle browser transports during shutdown.
func (a *Adapter) Close() error {
	a.image.browserMu.Lock()
	defer a.image.browserMu.Unlock()
	for _, b := range a.image.browsers {
		b.CloseIdleConnections()
	}
	return nil
}

func wire(ctx context.Context, c credentials) (wireAccount, error) {
	p := browserprofile.Default()
	if c.BrowserProfile != "" {
		found := false
		for _, candidate := range browserprofile.Candidates() {
			if candidate.Name == c.BrowserProfile {
				p = candidate
				found = true
				break
			}
		}
		if !found {
			if json.Unmarshal([]byte(c.BrowserProfile), &p) != nil || !p.Valid() {
				return wireAccount{}, errors.New("unsupported browser profile")
			}
		}
	}
	fields := map[string]any{"access_token": c.AccessToken, "browser_profile": p, "oai_device_id": c.DeviceID, "oai_session_id": c.SessionID}
	if env := environmentFrom(ctx); env.CFCookies != "" {
		if solver, ok := browserprofile.ForUserAgent(p, env.UserAgent); ok {
			fields["clearance_profile"], fields["clearance_cookies"] = solver, env.CFCookies
		}
	}
	return wireAccount{Token: c.AccessToken, Fields: fields}, nil
}

// loadInput reads a client image given as a base64 data URI or an https URL.
func (a *Adapter) loadInput(ctx context.Context, s string) (OpenAIImageInput, error) {
	var raw []byte
	if strings.HasPrefix(s, "data:") {
		head, data, ok := strings.Cut(s, ",")
		if !ok || !strings.HasSuffix(head, ";base64") {
			return OpenAIImageInput{}, invalidRequest("invalid input image")
		}
		if len(data) > 28<<20 {
			return OpenAIImageInput{}, invalidRequest("input image too large")
		}
		b, e := base64.StdEncoding.DecodeString(data)
		if e != nil {
			return OpenAIImageInput{}, invalidRequest("invalid image encoding")
		}
		raw = b
	} else {
		u, e := url.Parse(s)
		if e != nil || u.Scheme != "https" || u.User != nil {
			return OpenAIImageInput{}, invalidRequest("input image must be a data URI or an https URL")
		}
		r, e := a.image.doAbsolute(ctx, "GET", s, wireAccount{}, nil, nil, false)
		if e != nil {
			return OpenAIImageInput{}, invalidRequest("input image could not be fetched")
		}
		defer r.Body.Close()
		raw, e = readBounded(r.Body, 20<<20)
		if e != nil {
			return OpenAIImageInput{}, invalidRequest("input image too large")
		}
	}
	if len(raw) > 20<<20 {
		return OpenAIImageInput{}, invalidRequest("input image too large")
	}
	mime, e := validatedImageMIME(raw)
	if e != nil {
		return OpenAIImageInput{}, invalidRequest("input is not a supported image")
	}
	return OpenAIImageInput{Name: "image", MIME: mime, Data: raw}, nil
}

// requestError is a client mistake that no account or egress can fix.
type requestError struct{ message string }

func invalidRequest(message string) error          { return &requestError{message: message} }
func (e *requestError) Error() string              { return e.message }
func (e *requestError) HTTPStatusCode() int        { return http.StatusBadRequest }
func (e *requestError) RequestScopedFailure() bool { return true }
func (e *requestError) PublicErrorMessage() string { return e.message }
func (e *upstreamError) HTTPStatusCode() int       { return e.Status }
