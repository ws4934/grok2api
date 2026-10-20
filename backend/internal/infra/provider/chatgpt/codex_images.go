package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Codex image routes draw through the Codex backend's Images API
// (chatgpt.com/backend-api/codex/images), which paid plans may use with any
// ChatGPT login. Each route names the Images API model after its prefix.
var codexImageModels = []string{"codex-gpt-image-2", "codex-gpt-image-2.5", "codex-gpt-image-2.5-flare", "codex-gpt-image-2.5-sunburst"}

const codexImagePrefix = "codex-"

// Codex quota windows: 5-hour and weekly usage as shown to administrators,
// and the tighter of the two, which routes Codex image requests.
const (
	quotaModeCodex       = "codex"
	quotaModeCodex5Hour  = "codex_5h"
	quotaModeCodexWeekly = "codex_weekly"
)

// The Codex backend serves the Codex CLI; requests identify as it.
const (
	codexOriginator = "codex-tui"
	codexUserAgent  = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
)

// codexImageEngine returns the Images API model of a Codex image route.
func codexImageEngine(model string) (string, bool) {
	for _, id := range codexImageModels {
		if id == model {
			return strings.TrimPrefix(id, codexImagePrefix), true
		}
	}
	return "", false
}

// codexPlan reports whether a plan may use Codex images; free plans get 403.
func codexPlan(plan string) bool {
	switch strings.ToLower(plan) {
	case "plus", "pro", "team", "business", "enterprise", "edu":
		return true
	}
	return false
}

// planOf prefers the plan the access token carries (Codex tokens do; it is
// current after each refresh) over the plan stored at import.
func planOf(credential account.Credential, c credentials) string {
	return provider.FirstNonEmptyFailure(claimsIdentity(c.AccessToken).Plan, credential.Plan)
}

// AsyncImageModel reports whether a route can draw as a resumable image job.
// The Codex Images API answers with the image itself; there is no job to resume.
func (a *Adapter) AsyncImageModel(upstreamModel string) bool {
	_, codex := codexImageEngine(upstreamModel)
	return !codex
}

// codexAccountID is the workspace the Codex backend bills: the token's claim,
// else the account found at import (platform tokens carry no claim).
func codexAccountID(credential account.Credential, c credentials) string {
	return provider.FirstNonEmptyFailure(claimsIdentity(c.AccessToken).TeamID, credential.TeamID)
}

// codexDo calls the Codex backend on ctx's egress. The image timeout comes
// from ctx, not from a client timeout.
func (a *Adapter) codexDo(ctx context.Context, credential account.Credential, c credentials, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, method, a.image.BaseURL+"/backend-api"+path, reader)
	if err != nil {
		return nil, errors.New("invalid Codex endpoint")
	}
	r.Header.Set("Authorization", "Bearer "+c.AccessToken)
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Originator", codexOriginator)
	r.Header.Set("User-Agent", codexUserAgent)
	if id := codexAccountID(credential, c); id != "" {
		r.Header.Set("Chatgpt-Account-Id", id)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	if proxy := environmentFrom(ctx).ProxyURL; proxy != "" {
		egress, err := parseEgressProxy(proxy)
		if err != nil {
			return nil, errors.New("invalid Codex egress")
		}
		egress.apply(transport)
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(r)
	if err != nil {
		return nil, &transportError{cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := readOpenAIHTTPError(resp, path, c.AccessToken)
		resp.Body.Close()
		return nil, e
	}
	return resp, nil
}

// generateCodexImage draws one image through the Codex Images API. The
// request is sent at most once; inputs and mask go as data URLs and the mask
// keeps the Images API alpha convention.
func (a *Adapter) generateCodexImage(ctx context.Context, credential account.Credential, c credentials, job imageJob, engine string, submitted func()) (imageOutput, bool, error) {
	payload := map[string]any{"model": engine, "prompt": job.prompt, "n": 1}
	for key, value := range map[string]string{"size": job.size, "quality": job.quality, "background": job.background, "output_format": job.outputFormat} {
		if value != "" {
			payload[key] = value
		}
	}
	path := "/codex/images/generations"
	if job.edit {
		path = "/codex/images/edits"
		var images []map[string]string
		for _, value := range job.inputs {
			input, err := a.loadInput(ctx, value)
			if err != nil {
				return imageOutput{}, false, err
			}
			images = append(images, map[string]string{"image_url": dataURL(input)})
		}
		payload["images"] = images
		if job.mask != "" {
			mask, err := a.loadInput(ctx, job.mask)
			if err != nil {
				return imageOutput{}, false, err
			}
			payload["mask"] = map[string]string{"image_url": dataURL(mask)}
		}
	}
	var onSubmit func() error
	if submitted != nil {
		onSubmit = func() error { submitted(); return nil }
	}
	var raw []byte
	sent, err := a.attempt(ctx, credential, onSubmit, func(ctx context.Context) error {
		if err := markSubmitted(ctx); err != nil {
			return err
		}
		// The generation costs quota once sent: finish it even if the client
		// went away, so the image still lands in the gallery.
		drawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.image.RequestTimeout)
		defer cancel()
		started := time.Now()
		resp, err := a.codexDo(drawCtx, credential, c, http.MethodPost, path, payload)
		if err == nil {
			raw, err = decodeCodexImage(resp)
		}
		a.logImageStage(ctx, credential, "codex", started, err, "engine", engine, "bytes", len(raw))
		return err
	})
	var upstream *upstreamError
	if errors.As(err, &upstream) && upstream.Status >= 400 && upstream.Status < 500 {
		// Rejected before drawing: the gateway may refresh the credential on
		// 401, reconcile the Codex window on 429 or try another account.
		sent = false
	}
	if err != nil {
		return imageOutput{}, sent, err
	}
	mime, decoded, err := decodeImageDownload(raw)
	if err != nil {
		return imageOutput{}, true, err
	}
	output, err := a.archiveImage(ctx, job, downloadedImage{raw: raw, mime: mime, decoded: decoded})
	return output, true, err
}

func dataURL(input OpenAIImageInput) string {
	mime := input.MIME
	if mime == "" {
		mime = http.DetectContentType(input.Data)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(input.Data)
}

// decodeCodexImage reads the first image of an Images API response.
func decodeCodexImage(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	var body struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 96<<20)).Decode(&body); err != nil {
		return nil, errors.New("decode Codex image response: " + err.Error())
	}
	if len(body.Data) == 0 || body.Data[0].B64JSON == "" {
		return nil, errors.New("Codex image response carried no image")
	}
	return base64.StdEncoding.DecodeString(body.Data[0].B64JSON)
}

// codexQuotaWindows maps the Codex usage windows (/wham/usage) to percentage
// windows: Remaining and Total are percent points.
func (a *Adapter) codexQuotaWindows(ctx context.Context, credential account.Credential, c credentials, now time.Time) ([]account.QuotaWindow, error) {
	var usage struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Allowed      *bool             `json:"allowed"`
			LimitReached bool              `json:"limit_reached"`
			Primary      *codexUsageWindow `json:"primary_window"`
			Secondary    *codexUsageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	_, err := a.attempt(ctx, credential, nil, func(ctx context.Context) error {
		resp, err := a.codexDo(ctx, credential, c, http.MethodGet, "/wham/usage", nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&usage)
	})
	// A closed route is probed again after the stale interval, not every few minutes.
	reopen := now.Add(6 * time.Hour)
	closed := []account.QuotaWindow{{AccountID: credential.ID, Mode: quotaModeCodex, Total: 100, UsagePercent: 100, ResetAt: &reopen, SyncedAt: &now, Source: account.QuotaSourceUpstream, UpdatedAt: now}}
	var upstream *upstreamError
	if errors.As(err, &upstream) && !upstream.Challenge && upstream.Status >= 400 && upstream.Status < 500 && upstream.Status != http.StatusTooManyRequests {
		// Codex refuses this login: close the Codex routes until the next sync.
		return closed, nil
	}
	if err != nil {
		return nil, err
	}
	if usage.PlanType != "" && !codexPlan(usage.PlanType) {
		return closed, nil
	}
	limited := usage.RateLimit.LimitReached || (usage.RateLimit.Allowed != nil && !*usage.RateLimit.Allowed)
	var windows []account.QuotaWindow
	for _, item := range []struct {
		mode   string
		source *codexUsageWindow
	}{{quotaModeCodex5Hour, usage.RateLimit.Primary}, {quotaModeCodexWeekly, usage.RateLimit.Secondary}} {
		if item.source != nil {
			windows = append(windows, item.source.window(credential.ID, item.mode, now))
		}
	}
	if len(windows) == 0 {
		if limited {
			return closed, nil
		}
		return nil, nil
	}
	// The tighter window routes; when both are spent, the later reset frees it.
	route := windows[0]
	for _, window := range windows[1:] {
		if window.Remaining < route.Remaining || (window.Remaining == route.Remaining && window.ResetAt != nil && (route.ResetAt == nil || window.ResetAt.After(*route.ResetAt))) {
			route = window
		}
	}
	route.Mode = quotaModeCodex
	if limited {
		route.Remaining, route.UsagePercent = 0, 100
	}
	return append(windows, route), nil
}

type codexUsageWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowSeconds int     `json:"limit_window_seconds"`
	ResetAt       int64   `json:"reset_at"`
}

func (w codexUsageWindow) window(accountID uint64, mode string, now time.Time) account.QuotaWindow {
	used := min(max(w.UsedPercent, 0), 100)
	window := account.QuotaWindow{
		AccountID: accountID, Mode: mode, Remaining: int(100 - used), Total: 100, UsagePercent: used,
		WindowSeconds: w.WindowSeconds, SyncedAt: &now, Source: account.QuotaSourceUpstream, UpdatedAt: now,
	}
	if w.ResetAt > 0 {
		reset := time.Unix(w.ResetAt, 0).UTC()
		window.ResetAt = &reset
	}
	return window
}
