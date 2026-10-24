package muse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeChrome mimics the DevTools endpoint: it rejects Host headers that are
// neither an IP nor localhost and advertises a port-less WebSocket URL.
type fakeChrome struct {
	*httptest.Server
	mu    sync.Mutex
	hosts []string
}

func newFakeChrome(t *testing.T) *fakeChrome {
	f := &fakeChrome{}
	allowed := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		f.hosts = append(f.hosts, r.Host)
		f.mu.Unlock()
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "localhost" && net.ParseIP(host) == nil {
			http.Error(w, "Host header is specified and is not an IP address or localhost.", http.StatusInternalServerError)
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		if allowed(w, r) {
			fmt.Fprint(w, `{"Browser":"HeadlessChrome/155","webSocketDebuggerUrl":"ws://localhost/devtools/browser/abc"}`)
		}
	})
	mux.HandleFunc("/devtools/browser/abc", func(w http.ResponseWriter, r *http.Request) {
		if !allowed(w, r) {
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(data, &req)
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"method":"Target.targetInfoChanged","params":{}}`))
			switch req.Method {
			case "Browser.getVersion":
				_ = c.Write(ctx, websocket.MessageText, fmt.Appendf(nil, `{"id":%d,"result":{"product":"HeadlessChrome/155"}}`, req.ID))
			case "Test.hang":
			case "Test.close":
				return
			default:
				_ = c.Write(ctx, websocket.MessageText, fmt.Appendf(nil, `{"id":%d,"error":{"code":-32601,"message":"'%s' wasn't found"}}`, req.ID, req.Method))
			}
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func TestProbeBrowserSendsLocalhostAndRewritesWebSocketURL(t *testing.T) {
	f := newFakeChrome(t)
	if err := probeBrowser(context.Background(), f.URL); err != nil {
		t.Fatalf("probeBrowser: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hosts) != 2 || f.hosts[0] != "localhost" || f.hosts[1] != "localhost" {
		t.Fatalf("Host headers = %q, want localhost for /json/version and the WebSocket", f.hosts)
	}
}

func TestProbeBrowserUnavailable(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for _, endpoint := range []string{closed.URL, "ftp://muse-browser:9222", "muse-browser:9222"} {
		if err := probeBrowser(context.Background(), endpoint); !errors.Is(err, errBrowserUnavailable) {
			t.Errorf("probeBrowser(%q) = %v, want errBrowserUnavailable", endpoint, err)
		}
	}
}

func TestCDPConnLifecycle(t *testing.T) {
	f := newFakeChrome(t)
	dialCtx, cancel := context.WithCancel(context.Background())
	c, err := dialCDP(dialCtx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // the socket must outlive the context that dialed it
	ctx := context.Background()
	if err := c.call(ctx, "", "Browser.getVersion", nil, nil); err != nil {
		t.Fatalf("call after dial ctx cancel: %v", err)
	}
	if err := c.call(ctx, "", "Nope.missing", nil, nil); err == nil || errors.Is(err, errBrowserUnavailable) {
		t.Fatalf("protocol error = %v, want a plain CDP error", err)
	}
	short, cancelShort := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelShort()
	if err := c.call(short, "", "Test.hang", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung call = %v, want deadline exceeded", err)
	}
	if err := c.call(ctx, "", "Browser.getVersion", nil, nil); err != nil {
		t.Fatalf("a timed-out call must not break the socket: %v", err)
	}
	if err := c.call(ctx, "", "Test.close", nil, nil); !errors.Is(err, errBrowserUnavailable) {
		t.Fatalf("call on a dropped socket = %v, want errBrowserUnavailable", err)
	}
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("read loop did not exit")
	}
	if c.alive() {
		t.Fatal("alive after the socket dropped")
	}
	c.mu.Lock()
	pending := len(c.pending)
	c.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d pending calls left", pending)
	}
}

func TestEngineReconnects(t *testing.T) {
	f := newFakeChrome(t)
	e := newBrowserEngine(func() Config { return Config{BrowserURL: f.URL} }, slog.Default)
	c1, err := e.connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := e.connect(context.Background()); again != c1 {
		t.Fatal("a live connection must be reused")
	}
	c1.close()
	<-c1.done
	c2, err := e.connect(context.Background())
	if err != nil || c2 == c1 || !c2.alive() {
		t.Fatalf("reconnect = %v, %v", c2 == c1, err)
	}
	c2.close()
}

func TestAcquireHonorsMaxPages(t *testing.T) {
	e := newBrowserEngine(func() Config { return Config{MaxPages: 1} }, slog.Default)
	if err := e.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := e.acquire(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire = %v, want deadline exceeded", err)
	}
	done := make(chan error, 1)
	go func() { done <- e.acquire(context.Background()) }()
	e.release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCDPCookies(t *testing.T) {
	got := cdpCookies([]cookie{
		{Name: "hatch_sess", Value: "v1", Domain: ".muse.ai", Path: "/", Expires: 1800000000.5, HTTPOnly: true, Secure: true, SameSite: "lax"},
		{Name: "__Host-hatch_one_tap", Value: "v2", Domain: "muse.ai", HostOnly: true, Secure: true, SameSite: "no_restriction"},
		{Name: "theme", Value: "v3", Domain: "muse.ai", HostOnly: true, SameSite: "unspecified"},
		{Name: "bare", Value: "v4", SameSite: "strict"},
	})
	want := []cdpCookie{
		{Name: "hatch_sess", Value: "v1", Domain: ".muse.ai", Path: "/", Expires: 1800000000.5, HTTPOnly: true, Secure: true, SameSite: "Lax"},
		{Name: "__Host-hatch_one_tap", Value: "v2", URL: "https://muse.ai/", Path: "/", Secure: true, SameSite: "None"},
		{Name: "theme", Value: "v3", URL: "https://muse.ai/", Path: "/"},
		{Name: "bare", Value: "v4", URL: "https://muse.ai/", Path: "/", SameSite: "Strict"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d cookies", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cookie %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Session cookies must not send expires: 0, which CDP reads as expired.
	data, _ := json.Marshal(got[1])
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	if _, ok := raw["expires"]; ok {
		t.Errorf("session cookie JSON has expires: %s", data)
	}
	if _, ok := raw["domain"]; ok {
		t.Errorf("host-only cookie JSON has domain: %s", data)
	}
}
