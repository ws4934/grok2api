package chatgpt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/chatgpt/browserprofile"
)

const solverUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"

func TestClearanceSwitchesTheWholeBrowserIdentity(t *testing.T) {
	seen := make(chan http.Header, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	a, _ := newTestAdapter(t, server)
	login := credentials{AccessToken: "AT"}
	me := func(ctx context.Context) {
		w, err := wire(ctx, login)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = a.getJSON(ctx, w, http.MethodGet, "/backend-api/me", nil)
	}

	me(context.Background())
	if h := <-seen; !strings.Contains(h.Get("User-Agent"), "Macintosh") || h.Get("Sec-CH-UA-Platform") != `"macOS"` {
		t.Fatalf("account profile not used without clearance: %v", h)
	}
	ctx := withEnvironment(context.Background(), environment{UserAgent: solverUA, CFCookies: "cf_clearance=solved; __cf_bm=bm"})
	me(ctx)
	if h := <-seen; h.Get("User-Agent") != solverUA || h.Get("Sec-CH-UA-Platform") != `"Linux"` || !strings.Contains(h.Get("Sec-CH-UA"), `v="144"`) {
		t.Fatalf("clearance identity not applied: %v", h)
	}

	w, err := wire(ctx, login)
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := accountBrowserProfile(w.Fields)
	if profile.TLSProfile != "chrome_144" {
		t.Fatalf("TLS profile %s does not follow the solver Chrome version", profile.TLSProfile)
	}
	browser, err := accountBrowser(map[string]*browserHTTP{}, "https://chatgpt.com", "", "AT", w.Fields, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://chatgpt.com/backend-api/me")
	jar := map[string]string{}
	for _, c := range browser.client.GetCookies(origin) {
		jar[c.Name] = c.Value
	}
	if jar["cf_clearance"] != "solved" || jar["__cf_bm"] != "bm" {
		t.Fatalf("clearance cookies missing from the jar: %v", jar)
	}
	plain, _ := wire(context.Background(), login)
	if accountBrowserKey("https://chatgpt.com", "", "AT", profile, w.Fields) == accountBrowserKey("https://chatgpt.com", "", "AT", browserProfileOf(plain), plain.Fields) {
		t.Fatal("a clearance must not reuse the cached account browser")
	}
}

func browserProfileOf(w wireAccount) browserprofile.Profile {
	p, _ := accountBrowserProfile(w.Fields)
	return p
}
