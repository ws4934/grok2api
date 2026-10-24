package muse

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

//go:embed scripts/page.js
var pageJS string

const chunkSize = 1 << 20

var threadPath = regexp.MustCompile(`^/thread/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// page is one attached muse.ai tab.
type page struct {
	conn    *cdpConn
	session string
	log     *slog.Logger
}

func (p *page) cmd(ctx context.Context, method string, params, result any) error {
	return p.conn.call(ctx, p.session, method, params, result)
}

// js calls a helper of scripts/page.js with JSON arguments and decodes its
// return value into out when non-nil.
func (p *page) js(ctx context.Context, fn string, out any, args ...any) error {
	var expr strings.Builder
	expr.WriteString("(")
	expr.WriteString(pageJS)
	expr.WriteString(").")
	expr.WriteString(fn)
	expr.WriteString("(")
	for i, a := range args {
		data, err := json.Marshal(a)
		if err != nil {
			return err
		}
		if i > 0 {
			expr.WriteString(",")
		}
		expr.Write(data)
	}
	expr.WriteString(")")
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := p.cmd(ctx, "Runtime.evaluate", map[string]any{"expression": expr.String(), "returnByValue": true, "awaitPromise": true}, &r); err != nil {
		return err
	}
	if d := r.ExceptionDetails; d != nil {
		msg := d.Exception.Description
		if msg == "" {
			msg = d.Text
		}
		return fmt.Errorf("muse page script %s: %s", fn, firstLine(msg))
	}
	if out == nil || len(r.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(r.Result.Value, out)
}

// click presses the left mouse button at a viewport point. Menus open on
// pointer events, which a scripted el.click() does not send.
func (p *page) click(ctx context.Context, x, y int) error {
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		if err := p.cmd(ctx, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1}, nil); err != nil {
			return err
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fatal reports errors a poll loop must not retry: a dead browser or an
// ended context. Script errors during re-renders are retried.
func fatal(ctx context.Context, err error) bool {
	return errors.Is(err, errBrowserUnavailable) || ctx.Err() != nil
}

func (p *page) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	state := "loading"
	for {
		var st string
		err := p.js(ctx, "state", &st)
		switch {
		case err == nil:
			state = st
		case fatal(ctx, err):
			return fmt.Errorf("muse page not ready (%s): %w", state, err)
		}
		switch state {
		case "ready":
			return nil
		case "login":
			return errLoggedOut
		case "error":
			return errors.New("muse page failed to load")
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			return fmt.Errorf("muse page not ready (%s): %w", state, err)
		}
	}
}

// send types prompt into the composer, clicks Send and confirms the message
// left the composer. It returns when it was sent.
func (p *page) send(ctx context.Context, prompt string) (time.Time, error) {
	prompt = strings.ReplaceAll(strings.ReplaceAll(prompt, "\r\n", "\n"), "\r", "\n")
	var users int
	if err := p.js(ctx, "userCount", &users); err != nil {
		return time.Time{}, err
	}
	// The app ignores input for a moment after it looks ready, so the text is
	// set again until React enables Send.
	clicked := false
	for try := 0; try < 10 && !clicked; try++ {
		var ok bool
		if err := p.js(ctx, "setPrompt", &ok, prompt); err != nil {
			return time.Time{}, err
		}
		if !ok {
			return time.Time{}, fmt.Errorf("%w: composer did not take the prompt", errNotDelivered)
		}
		for i := 0; i < 7 && !clicked; i++ {
			if err := sleep(ctx, 150*time.Millisecond); err != nil {
				return time.Time{}, err
			}
			if err := p.js(ctx, "clickSend", &clicked); err != nil {
				return time.Time{}, err
			}
		}
	}
	if !clicked {
		return time.Time{}, fmt.Errorf("%w: Send never became enabled", errNotDelivered)
	}
	sentAt := time.Now()
	for time.Since(sentAt) < 10*time.Second {
		var sent bool
		if err := p.js(ctx, "sent", &sent, users); err != nil {
			return time.Time{}, err
		}
		if sent {
			return sentAt, nil
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			return time.Time{}, err
		}
	}
	return time.Time{}, fmt.Errorf("%w: message never left the composer", errNotDelivered)
}

// attach uploads files into the composer and waits until each shows its
// remove button, which the app renders once the upload is accepted.
func (p *page) attach(ctx context.Context, files []attachment) error {
	var stale int
	if err := p.js(ctx, "clearAttachments", &stale); err != nil {
		return err
	}
	if len(files) == 0 && stale == 0 {
		return nil
	}
	if err := p.waitAttachments(ctx, func(n int) bool { return n == 0 }, 5*time.Second); err != nil {
		return fmt.Errorf("muse composer kept stale attachments: %w", err)
	}
	if len(files) == 0 {
		return nil
	}
	type parked struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	list := make([]parked, 0, len(files))
	for i, f := range files {
		key := randomKey()
		for off := 0; off < len(f.Data); off += chunkSize {
			end := min(off+chunkSize, len(f.Data))
			if err := p.js(ctx, "putChunk", nil, key, base64.StdEncoding.EncodeToString(f.Data[off:end])); err != nil {
				return err
			}
		}
		name, typ := f.Name, f.MIME
		if typ == "" {
			typ = http.DetectContentType(f.Data)
		}
		if name == "" {
			name = fmt.Sprintf("input-%d%s", i+1, extension(typ))
		}
		list = append(list, parked{Key: key, Name: name, Type: typ})
	}
	var problem string
	if err := p.js(ctx, "attach", &problem, list); err != nil {
		return err
	}
	if problem != "" {
		return fmt.Errorf("muse attachment failed: %s", problem)
	}
	if err := p.waitAttachments(ctx, func(n int) bool { return n >= len(files) }, 60*time.Second); err != nil {
		return fmt.Errorf("muse did not accept the attachments: %w", err)
	}
	return nil
}

func (p *page) waitAttachments(ctx context.Context, done func(int) bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var n int
		if err := p.js(ctx, "attachmentCount", &n); err != nil {
			return err
		}
		if done(n) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d remove buttons after %s", n, timeout)
		}
		if err := sleep(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
}

type chatState struct {
	MD    string `json:"md"`
	Path  string `json:"path"`
	Stop  bool   `json:"stop"`
	Stuck bool   `json:"stuck"`
	Limit bool   `json:"limit"`
}

// waitReply follows the reply until it is non-empty, no Stop button shows
// and it has not changed for 2 s (the Stop button flickers mid-stream).
func (p *page) waitReply(ctx context.Context, sentAt time.Time, onText func(string) error) (string, error) {
	var last, trace string
	changed := time.Now()
	for {
		if err := sleep(ctx, 150*time.Millisecond); err != nil {
			return "", fmt.Errorf("waiting for the muse reply: %w", err)
		}
		var st chatState
		if err := p.js(ctx, "chat", &st); err != nil {
			if fatal(ctx, err) {
				return "", err
			}
			continue
		}
		if st.Limit {
			return "", errQuotaExhausted
		}
		if seen := fmt.Sprint(st.Stop, st.Stuck, len(st.MD), st.Path); seen != trace {
			trace = seen
			p.log.Debug("muse reply state", "since_send", time.Since(sentAt).Round(time.Millisecond), "stop", st.Stop, "stuck", st.Stuck, "chars", len(st.MD), "path", st.Path)
		}
		if st.MD != "" && st.MD != last {
			last, changed = st.MD, time.Now()
			if err := onText(last); err != nil {
				return "", err
			}
			continue
		}
		if last == "" {
			if st.Stuck && time.Since(sentAt) > 30*time.Second {
				return "", fmt.Errorf("%w: page still shows the message as sending", errNotDelivered)
			}
			if time.Since(sentAt) > 2*time.Minute {
				return "", fmt.Errorf("%w: no reply text within 2m", errTimeout)
			}
			continue
		}
		if !st.Stop && time.Since(changed) >= 2*time.Second {
			return last, nil
		}
	}
}

type mediaState struct {
	chatState
	Ready *struct {
		Src string `json:"src"`
		W   int    `json:"w"`
		H   int    `json:"h"`
	} `json:"ready"`
	Pending int `json:"pending"`
}

type foundMedia struct {
	Src  string
	W, H int
}

// waitMedia waits for a new result attachment of job.Kind. A zero sentAt
// means the job resumes a thread whose prompt was sent earlier.
func (p *page) waitMedia(ctx context.Context, job mediaJob, baseline []string, sentAt time.Time) (foundMedia, error) {
	if baseline == nil {
		baseline = []string{}
	}
	resumed := sentAt.IsZero()
	reported := resumed
	var lastText, trace string
	started, changed := time.Now(), time.Now()
	for {
		var st mediaState
		if err := p.js(ctx, "media", &st, string(job.Kind), baseline); err != nil {
			if fatal(ctx, err) {
				return foundMedia{}, err
			}
		} else {
			if st.Limit {
				return foundMedia{}, errQuotaExhausted
			}
			if seen := fmt.Sprint(st.Stop, st.Stuck, len(st.MD), st.Pending, st.Ready != nil, st.Path); seen != trace {
				trace = seen
				p.log.Debug("muse media state", "kind", job.Kind, "since_start", time.Since(started).Round(time.Millisecond), "stop", st.Stop, "stuck", st.Stuck, "chars", len(st.MD), "pending", st.Pending, "ready", st.Ready != nil, "path", st.Path)
			}
			if !reported && threadPath.MatchString(st.Path) {
				reported = true
				if job.OnThread != nil {
					if err := job.OnThread(st.Path); err != nil {
						return foundMedia{}, err
					}
				}
			}
			if st.Ready != nil {
				return foundMedia{Src: st.Ready.Src, W: st.Ready.W, H: st.Ready.H}, nil
			}
			if !resumed && st.MD == "" && st.Stuck && time.Since(sentAt) > 30*time.Second {
				return foundMedia{}, fmt.Errorf("%w: page still shows the message as sending", errNotDelivered)
			}
			if st.MD != lastText || st.Stop || st.Pending > 0 {
				lastText, changed = st.MD, time.Now()
			} else if lastText != "" && time.Since(changed) >= 15*time.Second {
				return foundMedia{}, &textOnlyError{Text: truncateRunes(lastText, 300)}
			}
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return foundMedia{}, fmt.Errorf("waiting for the muse %s: %w", job.Kind, err)
		}
	}
}

// download pulls a media URL's bytes through the page (blob: URLs only exist
// there) in chunks: one huge evaluate result stalls the DevTools socket.
func (p *page) download(ctx context.Context, src string) (mediaFile, error) {
	key := randomKey()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = p.js(cleanup, "drop", nil, key)
	}()
	var meta struct {
		Size  int    `json:"size"`
		Type  string `json:"type"`
		Error string `json:"error"`
	}
	if err := p.js(ctx, "grab", &meta, src, key); err != nil {
		return mediaFile{}, err
	}
	if meta.Error != "" || meta.Size <= 0 {
		return mediaFile{}, fmt.Errorf("muse media fetch failed: %s (size %d)", meta.Error, meta.Size)
	}
	data := make([]byte, 0, meta.Size)
	for off := 0; off < meta.Size; off += chunkSize {
		var b64 string
		if err := p.js(ctx, "chunk", &b64, key, off, chunkSize); err != nil {
			return mediaFile{}, err
		}
		part, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return mediaFile{}, fmt.Errorf("muse media chunk: %w", err)
		}
		data = append(data, part...)
	}
	if len(data) != meta.Size {
		return mediaFile{}, fmt.Errorf("muse media fetch: got %d of %d bytes", len(data), meta.Size)
	}
	mime := meta.Type
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	return mediaFile{Data: data, MIME: mime}, nil
}

func randomKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "__muse_" + hex.EncodeToString(b)
}

func extension(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
