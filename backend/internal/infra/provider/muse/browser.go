package muse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	chatTimeout  = 5 * time.Minute
	imageTimeout = 4 * time.Minute
	videoTimeout = 10 * time.Minute
	usageTimeout = time.Minute
	readyTimeout = time.Minute
)

// browserEngine drives muse.ai in the shared headless Chrome. Every call gets
// its own browser context (cookies, proxy), disposed when the call ends.
type browserEngine struct {
	cfg    func() Config
	logger func() *slog.Logger

	connMu sync.Mutex
	conn   *cdpConn

	slotMu sync.Mutex
	active int
	freed  chan struct{} // closed and replaced whenever a page slot frees up
}

func newBrowserEngine(cfg func() Config, logger func() *slog.Logger) *browserEngine {
	return &browserEngine{cfg: cfg, logger: logger, freed: make(chan struct{})}
}

func (e *browserEngine) Chat(ctx context.Context, s session, turn chatTurn, onText func(string) error) (string, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()
	var reply string
	err := e.withPage(ctx, s, "/thread/new", func(p *page) error {
		if err := p.attach(ctx, turn.Images); err != nil {
			return err
		}
		sentAt, err := p.send(ctx, turn.Prompt)
		if err != nil {
			return err
		}
		reply, err = p.waitReply(ctx, sentAt, onText)
		return err
	})
	return reply, timedOut(parent, err)
}

func (e *browserEngine) Generate(ctx context.Context, s session, job mediaJob) (mediaFile, error) {
	parent := ctx
	timeout := imageTimeout
	if job.Kind == mediaVideo {
		timeout = videoTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	path := job.Thread
	switch {
	case path == "":
		path = "/thread/new"
	case !threadPath.MatchString(path):
		return mediaFile{}, fmt.Errorf("invalid muse thread path %q", path)
	}
	var file mediaFile
	err := e.withPage(ctx, s, path, func(p *page) error {
		var baseline []string
		var sentAt time.Time
		if job.Thread == "" {
			if err := p.attach(ctx, job.Inputs); err != nil {
				return err
			}
			if err := p.js(ctx, "sources", &baseline); err != nil {
				return err
			}
			var err error
			if sentAt, err = p.send(ctx, job.Prompt); err != nil {
				return err
			}
		}
		found, err := p.waitMedia(ctx, job, baseline, sentAt)
		if err != nil {
			return err
		}
		file, err = p.download(ctx, found.Src)
		file.Width, file.Height = found.W, found.H
		return err
	})
	return file, timedOut(parent, err)
}

func (e *browserEngine) Usage(ctx context.Context, s session) (usage, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	var u usage
	err := e.withPage(ctx, s, "/thread/new", func(p *page) error {
		var err error
		u, err = p.readUsage(ctx)
		return err
	})
	return u, timedOut(parent, err)
}

// timedOut reports the engine's own deadline as errTimeout; the caller's
// cancellation or deadline stays a context error.
func timedOut(parent context.Context, err error) error {
	if err != nil && errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
		return fmt.Errorf("%w: %v", errTimeout, err)
	}
	return err
}

// withPage opens a logged-in muse.ai page at path, runs work and always
// disposes the page's browser context.
func (e *browserEngine) withPage(ctx context.Context, s session, path string, work func(*page) error) error {
	if err := e.acquire(ctx); err != nil {
		return err
	}
	defer e.release()
	conn, err := e.connect(ctx)
	if err != nil {
		return err
	}
	// Created under a detached context so a cancellation racing the reply
	// cannot leave a context behind that nobody disposes.
	setup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	var bc struct {
		BrowserContextID string `json:"browserContextId"`
	}
	params := map[string]any{"disposeOnDetach": true}
	if s.ProxyServer != "" {
		params["proxyServer"] = s.ProxyServer
	}
	err = conn.call(setup, "", "Target.createBrowserContext", params, &bc)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := conn.call(cleanup, "", "Target.disposeBrowserContext", map[string]any{"browserContextId": bc.BrowserContextID}, nil); err != nil {
			e.logger().Warn("muse browser context dispose failed", "error", err)
		}
	}()

	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := conn.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": bc.BrowserContextID}, &target); err != nil {
		return err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := conn.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		return err
	}
	p := &page{conn: conn, session: attached.SessionID, log: e.logger()}
	if err := p.cmd(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{"width": 1440, "height": 1000, "deviceScaleFactor": 1, "mobile": false}, nil); err != nil {
		return err
	}
	if err := p.cmd(ctx, "Network.setCookies", map[string]any{"cookies": cdpCookies(s.Cookies)}, nil); err != nil {
		return err
	}
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if err := p.cmd(ctx, "Page.navigate", map[string]any{"url": "https://muse.ai" + path}, &nav); err != nil {
		return err
	}
	if nav.ErrorText != "" {
		return fmt.Errorf("muse page load failed: %s", nav.ErrorText)
	}
	started := time.Now()
	if err := p.waitReady(ctx); err != nil {
		return err
	}
	e.logger().Debug("muse page ready", "path", path, "elapsed", time.Since(started))
	return work(p)
}

// acquire takes one of cfg().maxPages() page slots; a changed limit applies
// from the next acquire or release.
func (e *browserEngine) acquire(ctx context.Context) error {
	for {
		e.slotMu.Lock()
		if e.active < e.cfg().maxPages() {
			e.active++
			e.slotMu.Unlock()
			return nil
		}
		freed := e.freed
		e.slotMu.Unlock()
		select {
		case <-freed:
		case <-ctx.Done():
			return fmt.Errorf("waiting for a free muse page: %w", ctx.Err())
		}
	}
}

func (e *browserEngine) release() {
	e.slotMu.Lock()
	e.active--
	close(e.freed)
	e.freed = make(chan struct{})
	e.slotMu.Unlock()
}

// connect returns the live browser connection, dialing on first use, after
// the socket died or when the configured endpoint changed (which drops the
// pages still open on the old one).
func (e *browserEngine) connect(ctx context.Context) (*cdpConn, error) {
	endpoint := e.cfg().browserURL()
	e.connMu.Lock()
	defer e.connMu.Unlock()
	if e.conn != nil && e.conn.alive() && e.conn.endpoint == endpoint {
		return e.conn, nil
	}
	if e.conn != nil {
		e.conn.close()
		e.conn = nil
	}
	c, err := dialCDP(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	e.conn = c
	return c, nil
}

// cdpCookie is Network.CookieParam.
type cdpCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	URL      string  `json:"url,omitempty"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
}

// cdpCookies maps Cookie-Editor cookies to CDP. Host-only cookies (including
// __Host- ones) must be set by URL, since any domain makes them domain cookies.
func cdpCookies(jar []cookie) []cdpCookie {
	out := make([]cdpCookie, 0, len(jar))
	for _, c := range jar {
		p := cdpCookie{Name: c.Name, Value: c.Value, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly, Expires: c.Expires}
		if p.Path == "" {
			p.Path = "/"
		}
		host := strings.TrimPrefix(c.Domain, ".")
		switch {
		case host == "":
			p.URL = "https://muse.ai/"
		case c.HostOnly:
			p.URL = "https://" + host + "/"
		default:
			p.Domain = c.Domain
		}
		switch strings.ToLower(c.SameSite) {
		case "lax":
			p.SameSite = "Lax"
		case "strict":
			p.SameSite = "Strict"
		case "no_restriction", "none":
			p.SameSite = "None"
		}
		out = append(out, p)
	}
	return out
}
