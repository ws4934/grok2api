package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type stickyEgressStub struct {
	egressStub
	nodes []egressdomain.Node
}

func (s stickyEgressStub) ListEgressNodes(context.Context, egressdomain.Scope, repository.SortQuery) ([]egressdomain.Node, error) {
	return s.nodes, nil
}

// Imports, refresh-token exchanges and OAuth code exchanges of different
// logins must not share one sticky-proxy account (and with it one exit IP);
// a re-import of a stored account keeps that account's egress.
func TestPendingLoginsUseTheirOwnEgressIdentity(t *testing.T) {
	egressdomain.SetExtensionScopes([]egressdomain.Scope{scope})
	t.Cleanup(func() { egressdomain.SetExtensionScopes(nil) })
	var mu sync.Mutex
	var users []string
	// The proxy answers the token endpoint itself and records the sticky account.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		credentials, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(r.Header.Get("Proxy-Authorization"), "Basic "))
		user, _, _ := strings.Cut(string(credentials), ":")
		mu.Lock()
		users = append(users, user)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-AT","refresh_token":"fresh-RT","expires_in":3600}`))
	}))
	t.Cleanup(proxy.Close)
	a, _ := newTestAdapter(t, proxy)
	proxyURL, err := a.cipher.Encrypt("http://Default.{account}:token@" + strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	a.egress = infraegress.NewManager(stickyEgressStub{nodes: []egressdomain.Node{{ID: 1, Name: "resin", Scope: scope, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL}}}, a.cipher)
	lastUser := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(users) == 0 {
			return ""
		}
		return users[len(users)-1]
	}
	ctx := context.Background()

	var imported []string
	for _, rt := range []string{"RT-a", "RT-b"} {
		seeds, err := a.ParseImportedCredentials([]byte(`{"refresh_token":"` + rt + `","client_profile":"` + oauthProfile + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.PrepareImportedCredential(ctx, seeds[0]); err != nil {
			t.Fatal(err)
		}
		want := "Default.chatgpt_pending_" + security.HashToken(seeds[0].SourceKey)[:24]
		if lastUser() != want {
			t.Fatalf("import %s left as %q, want %q", rt, lastUser(), want)
		}
		imported = append(imported, lastUser())
	}
	if imported[0] == imported[1] {
		t.Fatalf("two imports shared the sticky account %q", imported[0])
	}

	seeds, err := a.ParseImportedCredentials([]byte(`{"refresh_token":"RT-a","client_profile":"` + oauthProfile + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	bound := infraegress.WithExistingAccount(ctx, account.Credential{ID: 42, Provider: account.ProviderChatGPT})
	if _, err := a.PrepareImportedCredential(bound, seeds[0]); err != nil {
		t.Fatal(err)
	}
	if lastUser() != "Default.chatgpt_42" {
		t.Fatalf("re-import left as %q, want the stored account's identity", lastUser())
	}

	challenge, err := a.StartAuthorization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var session oauthSession
	if err := json.Unmarshal([]byte(challenge.State), &session); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompleteAuthorization(ctx, challenge, oauthRedirect+"?state="+session.State+"&code=abc"); err != nil {
		t.Fatal(err)
	}
	if want := "Default.chatgpt_pending_" + security.HashToken(session.State)[:24]; lastUser() != want {
		t.Fatalf("OAuth login left as %q, want %q", lastUser(), want)
	}
}
