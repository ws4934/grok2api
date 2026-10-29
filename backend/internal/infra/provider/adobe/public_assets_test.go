package adobe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type assetTransport func(*http.Request) (*http.Response, error)

func (f assetTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (f assetTransport) Do(r *http.Request) (*http.Response, error)        { return f(r) }

func TestPresignedImagesAndVideosUseAnonymousDirectTransport(t *testing.T) {
	var downloads int
	transport := assetTransport(func(r *http.Request) (*http.Response, error) {
		downloads++
		for _, key := range []string{"Authorization", "Cookie", "x-api-key", "x-arp-session-id", "DPoP"} {
			if r.Header.Get(key) != "" {
				t.Fatalf("anonymous CDN request sent %s", key)
			}
		}
		if r.Method != http.MethodGet || r.URL.RawQuery != "signature=kept" {
			t.Fatalf("download = %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: io.NopCloser(strings.NewReader("media")), ContentLength: 5}, nil
	})
	c := &client{
		do: assetTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("CDN download used the API proxy")
			return nil, errors.New("unexpected API call")
		}),
		assets: transport,
		rc:     requestContext{token: "access-token", sherlock: "session", apiKey: "api-key"},
	}
	raw, err := c.download(context.Background(), "https://cdn.example.test/image.png?signature=kept")
	if err != nil || string(raw) != "media" {
		t.Fatalf("image=%q err=%v", raw, err)
	}
	// No manager is needed: the public video must not acquire an API lease.
	a := NewAdapter(Config{}, nil, nil, nil, nil)
	a.public.Transport = transport
	image, err := a.FetchImage(context.Background(), provider.ImageJobRequest{}, provider.ImageCheckpoint{Outputs: []string{"https://cdn.example.test/image.png?signature=kept"}})
	if err != nil || string(image) != "media" {
		t.Fatalf("queued image=%q err=%v", image, err)
	}
	body, mime, size, err := a.DownloadVideo(context.Background(), account.Credential{}, "https://cdn.example.test/video.mp4?signature=kept")
	if err != nil {
		t.Fatal(err)
	}
	raw, err = io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(raw) != "media" || mime != "video/mp4" || size != 5 || downloads != 3 {
		t.Fatalf("video=%q mime=%s size=%d downloads=%d err=%v", raw, mime, size, downloads, err)
	}
}

func TestPublicImagePostprocessingFailureDoesNotPenalizeAPIProxy(t *testing.T) {
	a := &Adapter{} // A mistaken feedback call would dereference the missing manager.
	err := provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, &transportError{err: errors.New("CDN unavailable")})
	a.report(context.Background(), &infraegress.Lease{NodeID: 9}, err)
}
