package updatecheck

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeGitHub 模拟 GitHub API、github.com 下载重定向与资产主机；客户端把所有主机都拨到同一个 TLS 测试服务。
type fakeGitHub struct {
	tag          string
	assets       map[string][]byte
	redirectHost string
	hold, held   chan struct{} // hold 非空时，安装包下载阻塞到 hold 关闭
}

func (f *fakeGitHub) client(t *testing.T) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.URL.Path)
		switch r.Host {
		case "api.github.com":
			assets := []map[string]string{}
			for asset := range f.assets {
				assets = append(assets, map[string]string{"name": asset, "browser_download_url": "https://github.com/ws4934/many2api/releases/download/" + f.tag + "/" + asset})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": f.tag, "html_url": releasesPage + "/tag/" + f.tag, "assets": assets})
		case "github.com":
			http.Redirect(w, r, "https://"+f.redirectHost+"/"+name, http.StatusFound)
		default:
			if f.hold != nil && strings.HasSuffix(name, ".tar.gz") {
				f.held <- struct{}{}
				<-f.hold
			}
			if content, ok := f.assets[name]; ok {
				_, _ = w.Write(content)
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	address := server.Listener.Addr().String()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
}

type tarEntry struct {
	header tar.Header
	body   string
}

func regular(name, body string) tarEntry {
	return tarEntry{header: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}, body: body}
}

func directory(name string) tarEntry {
	return tarEntry{header: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}}
}

func buildArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	for _, entry := range entries {
		if err := writer.WriteHeader(&entry.header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(writer.Close(), compressed.Close()); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func releaseEntries(tag string) []tarEntry {
	return []tarEntry{
		regular("grok2api", "new-binary"),
		regular("VERSION", tag+"\n"),
		directory("frontend/"),
		directory("frontend/dist/"),
		regular("frontend/dist/index.html", "new-frontend"),
		regular("frontend/dist/assets/app.js", "new-js"),
	}
}

func releaseAssets(tag string, archive []byte) map[string][]byte {
	name := releaseArchiveName(tag)
	sum := sha256.Sum256(archive)
	return map[string][]byte{
		name:            archive,
		"checksums.txt": []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n"),
	}
}

func newInstall(t *testing.T, client *http.Client) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "grok2api"), "old-binary")
	writeTestFile(t, filepath.Join(dir, "VERSION"), "v3.0.0\n")
	writeTestFile(t, filepath.Join(dir, "frontend", "dist", "index.html"), "old-frontend")
	service := NewService("v3.0.0", filepath.Join(dir, "frontend", "dist"), client)
	service.executable = filepath.Join(dir, "grok2api")
	service.versionPath = filepath.Join(dir, "VERSION")
	service.canRestart = func() bool { return false }
	service.restart = func() {}
	service.restartDelay = 0
	return service, dir
}

func writeTestFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, name, want string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil || string(data) != want {
		t.Fatalf("%s = %q, %v; want %q", name, data, err, want)
	}
}

func assertMissing(t *testing.T, pattern string) {
	t.Helper()
	if matches, _ := filepath.Glob(pattern); len(matches) != 0 {
		t.Fatalf("unexpected leftovers: %v", matches)
	}
}

// assertUntouched 确认安装目录仍是旧版本且没有留下备份或临时目录。
func assertUntouched(t *testing.T, dir string) {
	t.Helper()
	assertFile(t, filepath.Join(dir, "grok2api"), "old-binary")
	assertFile(t, filepath.Join(dir, "VERSION"), "v3.0.0\n")
	assertFile(t, filepath.Join(dir, "frontend", "dist", "index.html"), "old-frontend")
	assertMissing(t, filepath.Join(dir, "*"+backupSuffix))
	assertMissing(t, filepath.Join(dir, "frontend", "*"+backupSuffix))
	assertMissing(t, filepath.Join(dir, ".grok2api-*"))
}

func TestApplyReplacesInstallAndKeepsBackups(t *testing.T) {
	for _, canRestart := range []bool{true, false} {
		t.Run(fmt.Sprint("canRestart=", canRestart), func(t *testing.T) {
			fake := &fakeGitHub{tag: "v3.0.1", assets: releaseAssets("v3.0.1", buildArchive(t, releaseEntries("v3.0.1")...)), redirectHost: "release-assets.githubusercontent.com"}
			service, dir := newInstall(t, fake.client(t))
			restarted := make(chan struct{}, 1)
			service.canRestart = func() bool { return canRestart }
			service.restart = func() { restarted <- struct{}{} }

			if snapshot := service.Check(context.Background()); !snapshot.CanUpdate || snapshot.UpdateBlockedReason != "" || snapshot.CanRestart != canRestart || snapshot.ReleaseURL != releasesPage+"/tag/v3.0.1" {
				t.Fatalf("before apply snapshot = %#v", snapshot)
			}
			result, err := service.Apply(context.Background())
			if err != nil || result.Version != "v3.0.1" || result.Restarting != canRestart {
				t.Fatalf("Apply() = %#v, %v", result, err)
			}
			assertFile(t, filepath.Join(dir, "grok2api"), "new-binary")
			assertFile(t, filepath.Join(dir, "VERSION"), "v3.0.1\n")
			assertFile(t, filepath.Join(dir, "frontend", "dist", "index.html"), "new-frontend")
			assertFile(t, filepath.Join(dir, "frontend", "dist", "assets", "app.js"), "new-js")
			assertFile(t, filepath.Join(dir, "grok2api.backup"), "old-binary")
			assertFile(t, filepath.Join(dir, "VERSION.backup"), "v3.0.0\n")
			assertFile(t, filepath.Join(dir, "frontend", "dist.backup", "index.html"), "old-frontend")
			assertMissing(t, filepath.Join(dir, ".grok2api-*"))
			if info, err := os.Stat(filepath.Join(dir, "grok2api")); err != nil || info.Mode().Perm() != 0o755 {
				t.Fatalf("binary mode = %v, %v", info, err)
			}
			if snapshot := service.Snapshot(); snapshot.CanUpdate || !snapshot.RollbackAvailable {
				t.Fatalf("after apply snapshot = %#v", snapshot)
			}
			select {
			case <-restarted:
				if !canRestart {
					t.Fatal("restart hook called without a supervisor")
				}
			case <-time.After(200 * time.Millisecond):
				if canRestart {
					t.Fatal("restart hook was not called")
				}
			}
		})
	}
}

func TestApplyRejectsChecksumMismatch(t *testing.T) {
	assets := releaseAssets("v3.0.1", buildArchive(t, releaseEntries("v3.0.1")...))
	assets["checksums.txt"] = []byte(strings.Repeat("0", 64) + "  " + releaseArchiveName("v3.0.1") + "\n")
	service, dir := newInstall(t, (&fakeGitHub{tag: "v3.0.1", assets: assets, redirectHost: "objects.githubusercontent.com"}).client(t))
	if _, err := service.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("Apply() error = %v", err)
	}
	assertUntouched(t, dir)
}

func TestCheckReportsMissingPlatformAsset(t *testing.T) {
	assets := releaseAssets("v3.0.1", buildArchive(t, releaseEntries("v3.0.1")...))
	delete(assets, releaseArchiveName("v3.0.1"))
	service, dir := newInstall(t, (&fakeGitHub{tag: "v3.0.1", assets: assets, redirectHost: "objects.githubusercontent.com"}).client(t))
	snapshot := service.Check(context.Background())
	if !snapshot.UpdateAvailable || snapshot.CanUpdate || !strings.Contains(snapshot.UpdateBlockedReason, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if _, err := service.Apply(context.Background()); !errors.Is(err, ErrBlocked) || err.Error() != snapshot.UpdateBlockedReason {
		t.Fatalf("Apply() error = %v", err)
	}
	assertUntouched(t, dir)
}

func TestApplyRejectsUnsafeArchiveEntries(t *testing.T) {
	for _, entry := range []tarEntry{
		regular("../evil", "x"),
		regular("frontend/../../evil", "x"),
		regular("/tmp/evil", "x"),
		{header: tar.Header{Name: "frontend/dist/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
	} {
		t.Run(entry.header.Name, func(t *testing.T) {
			archive := buildArchive(t, append(releaseEntries("v3.0.1"), entry)...)
			service, dir := newInstall(t, (&fakeGitHub{tag: "v3.0.1", assets: releaseAssets("v3.0.1", archive), redirectHost: "objects.githubusercontent.com"}).client(t))
			if _, err := service.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "解压") {
				t.Fatalf("Apply() error = %v", err)
			}
			assertUntouched(t, dir)
			assertMissing(t, filepath.Join(dir, "evil"))
		})
	}
}

func TestApplyRejectsUntrustedRedirect(t *testing.T) {
	archive := buildArchive(t, releaseEntries("v3.0.1")...)
	service, dir := newInstall(t, (&fakeGitHub{tag: "v3.0.1", assets: releaseAssets("v3.0.1", archive), redirectHost: "evil.example.com"}).client(t))
	if _, err := service.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "evil.example.com") {
		t.Fatalf("Apply() error = %v", err)
	}
	assertUntouched(t, dir)
}

func TestRollbackRestoresPreviousInstall(t *testing.T) {
	archive := buildArchive(t, releaseEntries("v3.0.1")...)
	service, dir := newInstall(t, (&fakeGitHub{tag: "v3.0.1", assets: releaseAssets("v3.0.1", archive), redirectHost: "release-assets.githubusercontent.com"}).client(t))
	if _, err := service.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := make(chan struct{}, 1)
	service.canRestart = func() bool { return true }
	service.restart = func() { restarted <- struct{}{} }

	result, err := service.Rollback()
	if err != nil || result.Version != "v3.0.0" || !result.Restarting {
		t.Fatalf("Rollback() = %#v, %v", result, err)
	}
	assertUntouched(t, dir)
	assertMissing(t, filepath.Join(dir, "*"+discardSuffix))
	assertMissing(t, filepath.Join(dir, "frontend", "*"+discardSuffix))
	if _, err := os.Stat(filepath.Join(dir, "frontend", "dist", "assets")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new frontend files survived rollback: %v", err)
	}
	if service.Snapshot().RollbackAvailable {
		t.Fatal("rollback still reported as available")
	}
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("restart hook was not called")
	}
	if _, err := service.Rollback(); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("second Rollback() error = %v", err)
	}
}

func TestConcurrentApplyIsBusy(t *testing.T) {
	archive := buildArchive(t, releaseEntries("v3.0.1")...)
	fake := &fakeGitHub{tag: "v3.0.1", assets: releaseAssets("v3.0.1", archive), redirectHost: "release-assets.githubusercontent.com", hold: make(chan struct{}), held: make(chan struct{}, 1)}
	service, _ := newInstall(t, fake.client(t))
	done := make(chan error, 1)
	go func() {
		_, err := service.Apply(context.Background())
		done <- err
	}()
	select {
	case <-fake.held:
	case <-time.After(5 * time.Second):
		close(fake.hold)
		t.Fatal("first apply never started downloading")
	}
	if _, err := service.Apply(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent Apply() error = %v", err)
	}
	if _, err := service.Rollback(); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent Rollback() error = %v", err)
	}
	close(fake.hold)
	if err := <-done; err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
}

func TestApplyRefusesDevelopmentBuild(t *testing.T) {
	service := NewService("dev", "", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("development build reached the network")
		return nil, nil
	})})
	if _, err := service.Apply(context.Background()); !errors.Is(err, ErrBlocked) || err.Error() != "源码运行/开发版本不支持在线更新" {
		t.Fatalf("Apply() error = %v", err)
	}
}

func TestApplyWithoutUpdateFails(t *testing.T) {
	archive := buildArchive(t, releaseEntries("v3.0.0")...)
	service, dir := newInstall(t, (&fakeGitHub{tag: "v3.0.0", assets: releaseAssets("v3.0.0", archive), redirectHost: "release-assets.githubusercontent.com"}).client(t))
	if _, err := service.Apply(context.Background()); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("Apply() error = %v", err)
	}
	assertUntouched(t, dir)
}
