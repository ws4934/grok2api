package socksproxy

import (
	"context"
	"io"
	"net"
	"net/url"
	"testing"
	"time"
)

// A SOCKS4 proxy that accepts TCP and never answers must not keep the dial
// goroutine or the proxy socket once the dial is cancelled or times out.
func TestNewDialContextReleasesStalledSOCKS4Handshake(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout time.Duration
		cancel  time.Duration
	}{
		{name: "cancelled", timeout: time.Minute, cancel: 50 * time.Millisecond},
		// http.Transport dials with a context the request cannot cancel.
		{name: "handshake timeout", timeout: 100 * time.Millisecond, cancel: time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			closed := make(chan struct{}, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = io.Copy(io.Discard, conn) // returns once the client closes
				closed <- struct{}{}
			}()
			dial, err := NewDialContext(&url.URL{Scheme: "socks4", Host: listener.Addr().String()}, &net.Dialer{}, test.timeout)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), test.cancel)
			defer cancel()
			dialed := make(chan error, 1)
			go func() {
				conn, err := dial(ctx, "tcp", "203.0.113.1:80")
				if conn != nil {
					_ = conn.Close()
				}
				dialed <- err
			}()
			select {
			case err := <-dialed:
				if err == nil {
					t.Fatal("dial through a silent proxy succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("dial still blocked in the SOCKS4 handshake")
			}
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("proxy socket still open after the dial returned")
			}
		})
	}
}
