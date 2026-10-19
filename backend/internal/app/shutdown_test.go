package app

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	httpmiddleware "github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
)

// Run closes the audit queue and then storage right after drainServer returns,
// so streams that outlive the grace period (SSE and hijacked WebSockets) must be
// cancelled and finish their deferred finalize before it returns.
func TestDrainServerFinalizesLongStreamsBeforeReturning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gate := httpmiddleware.NewConcurrencyGate(10)
	var started sync.WaitGroup
	var finalized atomic.Int32
	stream := func(hijack bool, finalizeTime time.Duration) gin.HandlerFunc {
		return func(c *gin.Context) {
			defer func() {
				time.Sleep(finalizeTime) // stands in for the audit/billing write
				finalized.Add(1)
			}()
			if hijack {
				conn, _, err := c.Writer.Hijack()
				if err != nil {
					t.Error(err)
					started.Done()
					return
				}
				defer conn.Close()
			} else {
				c.Writer.WriteHeader(http.StatusOK)
				c.Writer.Flush()
			}
			started.Done()
			<-c.Request.Context().Done()
		}
	}
	router := gin.New()
	router.Use(gate.Middleware())
	router.GET("/sse", stream(false, 0))
	router.GET("/ws", stream(true, 300*time.Millisecond))

	server := &http.Server{Handler: router}
	cancelRequests := cancelableRequests(server)
	defer cancelRequests()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()

	started.Add(2)
	for _, path := range []string{"/sse", "/ws"} {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: test\r\n\r\n", path); err != nil {
			t.Fatal(err)
		}
	}
	started.Wait()

	if err := drainServer(server, cancelRequests, gate, 50*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("drainServer() error = %v, want nil once cancelled streams finish", err)
	}
	if got := finalized.Load(); got != 2 {
		t.Fatalf("finalized streams = %d, want 2 before the audit queue closes", got)
	}
}
