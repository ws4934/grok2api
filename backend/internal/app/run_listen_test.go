package app

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A second process on a taken address exits before startup reconciliation
// and the background loops touch the shared database.
func TestRunExitsBeforeStartupWhenTheListenAddressIsTaken(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	dir := t.TempDir()
	yaml := strings.Join([]string{
		"server:",
		"  listen: \"" + occupied.Addr().String() + "\"",
		"secrets:",
		"  jwtSecret: \"0123456789abcdef0123456789abcdef-probe\"",
		"  credentialEncryptionKey: \"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=\"",
		"bootstrapAdmin:",
		"  username: \"admin\"",
		"  password: \"probe-password-123\"",
		"frontend:",
		"  staticPath: \"./missing\"",
		"database:",
		"  driver: sqlite",
		"  sqlite:",
		"    path: \"./data/backend.db\"",
		"media:",
		"  driver: local",
		"  local:",
		"    path: \"./data/media\"",
		"",
	}, "\n")
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedLogBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	application, err := New(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if err := application.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded on a taken address")
	}
	if strings.Contains(logs.String(), "startup_reconciliation_completed") {
		t.Fatalf("startup reconciliation ran although the listen address was taken")
	}
}
