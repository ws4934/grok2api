package egress

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

func TestToFHTTPRequestPreservesRequestFraming(t *testing.T) {
	payload := []byte(`{"message":"hello"}`)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://grok.com/rest/test", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "grok.com"
	request.Header.Set("Content-Type", "application/json")

	converted, err := toFHTTPRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if converted.ContentLength != int64(len(payload)) || len(converted.TransferEncoding) != 0 {
		t.Fatalf("contentLength=%d transferEncoding=%v", converted.ContentLength, converted.TransferEncoding)
	}
	if converted.Host != request.Host || converted.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("host=%q headers=%v", converted.Host, converted.Header)
	}
	if converted.GetBody == nil {
		t.Fatal("GetBody was not preserved")
	}
	body, err := converted.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("body=%q err=%v", got, err)
	}
}

func TestFromFHTTPResponseNormalizesAutoDecompressedHeaders(t *testing.T) {
	response := fromFHTTPResponse(&fhttp.Response{
		Status: "200 OK", StatusCode: http.StatusOK, Proto: "HTTP/2.0", ProtoMajor: 2,
		Header: fhttp.Header{
			"Content-Encoding": []string{"gzip"},
			"Content-Length":   []string{"128"},
			"Content-Type":     []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(`{"status":"completed"}`)), ContentLength: 128, Uncompressed: true,
	})
	if response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Length") != "" {
		t.Fatalf("decoded response headers = %#v", response.Header)
	}
	if response.ContentLength != -1 || !response.Uncompressed {
		t.Fatalf("contentLength=%d uncompressed=%v", response.ContentLength, response.Uncompressed)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(data, []byte(`{"status":"completed"}`)) {
		t.Fatalf("body=%q err=%v", data, err)
	}
}

func TestFromFHTTPResponsePreservesCompressedHeaders(t *testing.T) {
	response := fromFHTTPResponse(&fhttp.Response{
		Status: "200 OK", StatusCode: http.StatusOK,
		Header: fhttp.Header{"Content-Encoding": []string{"gzip"}, "Content-Length": []string{"128"}},
		Body:   io.NopCloser(bytes.NewReader(nil)), ContentLength: 128,
	})
	if response.Header.Get("Content-Encoding") != "gzip" || response.Header.Get("Content-Length") != "128" || response.ContentLength != 128 {
		t.Fatalf("compressed response = headers=%#v contentLength=%d", response.Header, response.ContentLength)
	}
}

func TestFromFHTTPResponseOwnsHeadersAndPreservesDeferredTrailers(t *testing.T) {
	source := &fhttp.Response{
		Status: "200 OK", StatusCode: http.StatusOK,
		Header:           fhttp.Header{"X-Upstream": []string{"original"}},
		Trailer:          fhttp.Header{"X-Usage": nil},
		TransferEncoding: []string{"chunked"},
		Body:             io.NopCloser(bytes.NewReader(nil)),
	}
	response := fromFHTTPResponse(source)

	source.Header.Set("X-Upstream", "mutated")
	source.TransferEncoding[0] = "identity"
	// fhttp 会在读取 Body 到 EOF 时以这种方式填充已声明的 Trailer。
	source.Trailer.Set("X-Usage", "42")

	if response.Header.Get("X-Upstream") != "original" {
		t.Fatalf("response header aliases fhttp header: %#v", response.Header)
	}
	if len(response.TransferEncoding) != 1 || response.TransferEncoding[0] != "chunked" {
		t.Fatalf("response transfer encoding aliases fhttp value: %#v", response.TransferEncoding)
	}
	if response.Trailer.Get("X-Usage") != "42" {
		t.Fatalf("deferred trailer was lost: %#v", response.Trailer)
	}
}

// A proxy that accepts TCP and never answers must fail a browser request at
// its deadline and must not block a request to another host.
func TestBrowserClientProxyHandshakeEndsWithRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	for _, scheme := range []string{"socks5", "socks4", "http"} {
		t.Run(scheme, func(t *testing.T) {
			client, err := newBrowserClient(scheme+"://"+listener.Addr().String(), DefaultUserAgent)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 2)
			for _, target := range []string{"https://203.0.113.1/", "https://203.0.113.2/"} {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					defer cancel()
					request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
					response, err := client.Do(request)
					if err == nil {
						_ = response.Body.Close()
					}
					done <- err
				}()
			}
			for range 2 {
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("request through a silent proxy succeeded")
					}
				case <-time.After(time.Second):
					t.Fatal("request still blocked in the proxy handshake after its deadline")
				}
			}
		})
	}
}

func TestBrowserClientTunnelsThroughAuthenticatedHTTPProxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() {
			_, _ = io.Copy(upstream, buffered)
			_ = upstream.Close()
		}()
		_, _ = io.Copy(conn, upstream)
	}))
	defer proxy.Close()
	client, err := newBrowserClient("http://user:pass@"+proxy.Listener.Addr().String(), DefaultUserAgent)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	request, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("status=%d body=%q err=%v", response.StatusCode, body, err)
	}
}
