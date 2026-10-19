package chatgpt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A browser session reuses its page bootstrap until bootstrapTTL passes.
func TestBrowserSessionReusesItsBootstrap(t *testing.T) {
	b := &browserHTTP{}
	if _, _, ok := b.bootstrapped(); ok {
		t.Fatal("a new session reported a bootstrap")
	}
	b.rememberBootstrap([]string{"https://chatgpt.com/sdk.js"}, "build-1")
	if scripts, build, ok := b.bootstrapped(); !ok || build != "build-1" || len(scripts) != 1 {
		t.Fatalf("bootstrap = %v %q %v", scripts, build, ok)
	}
	b.bootAt = time.Now().Add(-bootstrapTTL - time.Second)
	if _, _, ok := b.bootstrapped(); ok {
		t.Fatal("an expired bootstrap was reused")
	}
}

// A conversation that stays missing ends the polling instead of running to
// the image deadline.
func TestImagePollingStopsOnAMissingConversation(t *testing.T) {
	interval := imagePollInterval
	imagePollInterval = time.Millisecond
	t.Cleanup(func() { imagePollInterval = interval })
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		polls++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Conversation not found"}`))
	}))
	t.Cleanup(server.Close)
	a, _ := newTestAdapter(t, server)
	if _, err := a.image.pollConversation(context.Background(), wireAccount{Token: "AT"}, "gone"); err == nil || polls != imagePollMissing {
		t.Fatalf("polls = %d, err = %v", polls, err)
	}
}
