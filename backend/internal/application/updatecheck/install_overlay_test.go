package updatecheck

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Docker's overlay filesystem refuses to rename a directory from an image
// layer (EXDEV): the frontend directory is copied instead, for the update and
// for the rollback.
func TestApplyAndRollbackCopyDirectoriesTheFilesystemCannotRename(t *testing.T) {
	original := rename
	t.Cleanup(func() { rename = original })
	rename = func(src, dst string) error {
		if info, err := os.Stat(src); err == nil && info.IsDir() {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EXDEV}
		}
		return original(src, dst)
	}
	archive := buildArchive(t, releaseEntries("v3.0.1")...)
	service, dir := newInstall(t, (&fakeGitHub{tag: "v3.0.1", assets: releaseAssets("v3.0.1", archive), redirectHost: "release-assets.githubusercontent.com"}).client(t))
	service.canRestart = func() bool { return false }
	service.restart = func() {}

	if _, err := service.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(dir, "grok2api"), "new-binary")
	assertFile(t, filepath.Join(dir, "frontend", "dist", "index.html"), "new-frontend")
	assertFile(t, filepath.Join(dir, "frontend", "dist", "assets", "app.js"), "new-js")
	assertFile(t, filepath.Join(dir, "frontend", "dist.backup", "index.html"), "old-frontend")
	assertMissing(t, filepath.Join(dir, ".grok2api-*"))

	if _, err := service.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertUntouched(t, dir)
	assertMissing(t, filepath.Join(dir, "frontend", "*"+discardSuffix))
}
