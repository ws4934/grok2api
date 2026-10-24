package muse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// cdpHTTP talks to the DevTools endpoint directly: proxy environment
// variables must not reroute a private service address.
var cdpHTTP = &http.Client{Transport: &http.Transport{}}

// cdpConn is one browser-level DevTools WebSocket. Pages are flattened
// sessions on it, addressed by sessionId. Events are read and dropped: the
// engine polls the page instead.
type cdpConn struct {
	endpoint string
	ws       *websocket.Conn
	nextID   atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan cdpMessage
	err     error         // why the connection ended; set before done closes
	done    chan struct{} // closed when the read loop ends
}

type cdpMessage struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

// probeBrowser checks that endpoint serves DevTools the way the engine uses it.
func probeBrowser(ctx context.Context, endpoint string) error {
	c, err := dialCDP(ctx, endpoint)
	if err != nil {
		return err
	}
	defer c.close()
	return c.call(ctx, "", "Browser.getVersion", nil, nil)
}

// dialCDP connects to the browser behind a DevTools HTTP endpoint such as
// http://muse-browser:9222. Chrome refuses any Host header that is neither an
// IP nor localhost, so both requests claim localhost, and the WebSocket URL it
// returns (ws://localhost/devtools/browser/<id>) is pointed back at endpoint.
func dialCDP(ctx context.Context, endpoint string) (*cdpConn, error) {
	base, err := url.Parse(endpoint)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("%w: invalid browser URL %q", errBrowserUnavailable, endpoint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.JoinPath("json", "version").String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBrowserUnavailable, err)
	}
	req.Host = "localhost"
	resp, err := cdpHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBrowserUnavailable, err)
	}
	defer resp.Body.Close()
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GET /json/version: HTTP %d", errBrowserUnavailable, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil || version.WebSocketDebuggerURL == "" {
		return nil, fmt.Errorf("%w: GET /json/version: no webSocketDebuggerUrl", errBrowserUnavailable)
	}
	wsURL, err := url.Parse(version.WebSocketDebuggerURL)
	if err != nil {
		return nil, fmt.Errorf("%w: bad webSocketDebuggerUrl: %v", errBrowserUnavailable, err)
	}
	wsURL.Scheme, wsURL.Host = "ws", base.Host
	if base.Scheme == "https" {
		wsURL.Scheme = "wss"
	}
	ws, _, err := websocket.Dial(ctx, wsURL.String(), &websocket.DialOptions{HTTPClient: cdpHTTP, Host: "localhost"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBrowserUnavailable, err)
	}
	// Media is pulled in 1 MiB chunks (~1.4 MB as base64), far below this.
	ws.SetReadLimit(64 << 20)
	c := &cdpConn{endpoint: endpoint, ws: ws, pending: map[int64]chan cdpMessage{}, done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

func (c *cdpConn) readLoop() {
	var err error
	for {
		var data []byte
		if _, data, err = c.ws.Read(context.Background()); err != nil {
			break
		}
		var msg cdpMessage
		if json.Unmarshal(data, &msg) != nil || msg.ID == 0 {
			continue // an event
		}
		c.mu.Lock()
		ch := c.pending[msg.ID]
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- msg // buffered
		}
	}
	c.ws.CloseNow()
	c.mu.Lock()
	c.err = fmt.Errorf("%w: connection lost: %v", errBrowserUnavailable, err)
	close(c.done)
	c.mu.Unlock()
}

func (c *cdpConn) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *cdpConn) close() { c.ws.CloseNow() }

// call sends one command (to the browser when session is empty) and decodes
// its result into result when non-nil.
func (c *cdpConn) call(ctx context.Context, session, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := c.nextID.Add(1)
	data, err := json.Marshal(struct {
		ID        int64  `json:"id"`
		SessionID string `json:"sessionId,omitempty"`
		Method    string `json:"method"`
		Params    any    `json:"params,omitempty"`
	}{id, session, method, params})
	if err != nil {
		return err
	}
	ch := make(chan cdpMessage, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	// A context that ends during a write closes the whole socket, so writes
	// get their own deadline instead of the caller's context.
	wctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = c.ws.Write(wctx, websocket.MessageText, data)
	cancel()
	if err != nil {
		return fmt.Errorf("%w: %s: %v", errBrowserUnavailable, method, err)
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return fmt.Errorf("cdp %s: %s", method, strings.TrimSpace(msg.Error.Message+" "+msg.Error.Data))
		}
		if result != nil {
			return json.Unmarshal(msg.Result, result)
		}
		return nil
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
