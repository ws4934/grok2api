package console

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type assetRoundTripFunc func(*http.Request) (*http.Response, error)

func (f assetRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type assetEgressRepository struct {
	consoleEgressRepositoryStub
	node domain.Node
}

func (r assetEgressRepository) ListEgressNodes(_ context.Context, scope domain.Scope, _ repository.SortQuery) ([]domain.Node, error) {
	if scope == r.node.Scope {
		return []domain.Node{r.node}, nil
	}
	return nil, nil
}

func (r assetEgressRepository) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	return r.node, nil
}

func TestPublicConsoleImagesAndVideosBypassBoundAPIProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	a, credential := newConsoleTestAdapterWithAssets(t, "https://console.x.ai", nil)
	proxyURL, _ := a.cipher.Encrypt(proxy.URL)
	a.egress = infraegress.NewManager(assetEgressRepository{node: domain.Node{ID: 9, Scope: domain.ScopeConsole, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL}}, a.cipher)
	credential.EgressNodeID = 9
	var downloads int
	a.public.Transport = assetRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		downloads++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("DPoP") != "" {
			t.Fatalf("unexpected anonymous download: %s %v", r.Method, r.Header)
		}
		mime := "image/png"
		if r.URL.Hostname() == "vidgen.x.ai" {
			mime = "video/mp4"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(strings.NewReader("media")), ContentLength: 5, Request: r}, nil
	})
	ctx, trace := infraegress.WithTrace(infraegress.WithEgressNode(context.Background(), 9))
	image, err := a.downloadConsoleImage(ctx, credential, "https://console.x.ai", "https://imgen.x.ai/out.png")
	if err != nil || string(image) != "media" {
		t.Fatalf("image = %q, %v", image, err)
	}
	body, mime, size, err := a.DownloadVideo(ctx, credential, "https://vidgen.x.ai/out.mp4")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(raw) != "media" || mime != "video/mp4" || size != 5 || downloads != 2 || proxyCalls.Load() != 0 {
		t.Fatalf("video=%q %s %d err=%v downloads=%d proxy calls=%d", raw, mime, size, err, downloads, proxyCalls.Load())
	}
	if selected, ok := trace.Selection(domain.ScopeConsoleAsset); !ok || selected.NodeID != 0 || selected.Scope != domain.ScopeConsoleAsset {
		t.Fatalf("download trace = %+v", selected)
	}
	// A failed direct download must not secretly fall back to the API proxy.
	a.public.Transport = assetRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("CDN unavailable") })
	if _, _, _, err := a.DownloadVideo(ctx, credential, "https://vidgen.x.ai/out.mp4"); err == nil || proxyCalls.Load() != 0 {
		t.Fatalf("CDN failure = %v, API proxy calls=%d", err, proxyCalls.Load())
	}
}

func TestPublicConsoleDownloadHonorsExplicitResourceProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusProxyAuthRequired) // Stop at the proxy; never reach an external host.
	}))
	defer proxy.Close()
	a, credential := newConsoleTestAdapterWithAssets(t, "https://console.x.ai", nil)
	proxyURL, _ := a.cipher.Encrypt(proxy.URL)
	a.egress = infraegress.NewManager(assetEgressRepository{node: domain.Node{ID: 10, Scope: domain.ScopeConsoleAsset, Enabled: true, Health: 1, EncryptedProxyURL: proxyURL}}, a.cipher)
	a.public.Transport = assetRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("explicit resource proxy was bypassed")
		return nil, errors.New("unexpected direct download")
	})
	ctx, trace := infraegress.WithTrace(context.Background())
	_, _, _, err := a.DownloadVideo(ctx, credential, "https://vidgen.x.ai/out.mp4")
	selection, ok := trace.Selection(domain.ScopeConsoleAsset)
	if err == nil || proxyCalls.Load() == 0 || !ok || selection.NodeID != 10 {
		t.Fatalf("explicit resource route: proxy calls=%d trace=%+v err=%v", proxyCalls.Load(), selection, err)
	}
}
