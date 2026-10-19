package updatecheck

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	backupSuffix      = ".backup"
	discardSuffix     = ".old"
	downloadTimeout   = 10 * time.Minute
	maxChecksumBytes  = 64 << 10
	maxArchiveBytes   = 256 << 20
	maxExtractedBytes = 512 << 20
	maxArchiveEntries = 20000
)

var (
	ErrBusy     = errors.New("已有更新或回滚操作正在进行")
	ErrNoUpdate = errors.New("当前已是最新版本")
	ErrBlocked  = errors.New("当前安装不支持在线更新")
	ErrNoBackup = errors.New("没有可回滚的备份")
)

// trustedDownloadHosts 是下载安装包及其每一跳重定向允许访问的主机。
var trustedDownloadHosts = map[string]bool{
	"github.com":                           true,
	"api.github.com":                       true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

// InstallResult 是在线更新或回滚的结果；Restarting 为 false 时需管理员手动重启。
type InstallResult struct {
	Version    string `json:"version"`
	Restarting bool   `json:"restarting"`
}

// blockedError 携带不能在线更新的具体原因，并可用 errors.Is 匹配 ErrBlocked。
type blockedError string

func (e blockedError) Error() string { return string(e) }
func (blockedError) Unwrap() error   { return ErrBlocked }

// Apply 下载最新 Release，校验 SHA-256 后替换可执行文件、前端静态目录与 VERSION，并保留 .backup 备份。
func (s *Service) Apply(ctx context.Context) (InstallResult, error) {
	if !s.installing.TryLock() {
		return InstallResult{}, ErrBusy
	}
	defer s.installing.Unlock()
	if reason := s.installBlocker(); reason != "" {
		return InstallResult{}, blockedError(reason)
	}
	snapshot := s.Check(ctx)
	switch {
	case snapshot.Status == StatusCheckFailed:
		return InstallResult{}, fmt.Errorf("检查更新失败：%s", snapshot.Error)
	case !snapshot.UpdateAvailable:
		return InstallResult{}, ErrNoUpdate
	case !snapshot.CanUpdate:
		return InstallResult{}, blockedError(snapshot.UpdateBlockedReason)
	}
	s.mu.RLock()
	release := s.release
	s.mu.RUnlock()
	if err := s.install(ctx, release); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Version: release.Tag, Restarting: s.finish()}, nil
}

// Rollback 用上次更新留下的备份换回可执行文件、前端静态目录与 VERSION。
func (s *Service) Rollback() (InstallResult, error) {
	if !s.installing.TryLock() {
		return InstallResult{}, ErrBusy
	}
	defer s.installing.Unlock()
	if !s.backupsExist() {
		return InstallResult{}, ErrNoBackup
	}
	version, _ := os.ReadFile(s.versionPath + backupSuffix)
	targets := s.targets()
	backups := make([]string, len(targets))
	for index, target := range targets {
		backups[index] = target + backupSuffix
	}
	if err := replace(targets, backups, discardSuffix); err != nil {
		return InstallResult{}, err
	}
	for _, target := range targets {
		_ = os.RemoveAll(target + discardSuffix)
	}
	return InstallResult{Version: strings.TrimSpace(string(version)), Restarting: s.finish()}, nil
}

func (s *Service) install(ctx context.Context, release latestRelease) error {
	var checksums bytes.Buffer
	if err := s.download(ctx, release.ChecksumsURL, &checksums, maxChecksumBytes); err != nil {
		return fmt.Errorf("下载 checksums.txt 失败：%w", err)
	}
	archiveName := releaseArchiveName(release.Tag)
	expected, err := findChecksum(checksums.String(), archiveName)
	if err != nil {
		return err
	}
	workDir, err := os.MkdirTemp(filepath.Dir(s.executable), ".grok2api-update-*")
	if err != nil {
		return fmt.Errorf("创建更新临时目录失败：%w", err)
	}
	defer os.RemoveAll(workDir)
	archivePath := filepath.Join(workDir, "release.tar.gz")
	archive, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	err = s.download(ctx, release.ArchiveURL, io.MultiWriter(archive, hash), maxArchiveBytes)
	if err = errors.Join(err, archive.Close()); err != nil {
		return fmt.Errorf("下载 %s 失败：%w", archiveName, err)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != expected {
		return fmt.Errorf("%s 的 SHA-256 校验失败", archiveName)
	}
	stage := filepath.Join(workDir, "stage")
	if err := extractArchive(archivePath, stage); err != nil {
		return fmt.Errorf("解压 %s 失败：%w", archiveName, err)
	}
	if err := checkStage(stage, release.Tag); err != nil {
		return err
	}
	return replace(s.targets(), []string{
		filepath.Join(stage, "grok2api"),
		filepath.Join(stage, "frontend", "dist"),
		filepath.Join(stage, "VERSION"),
	}, backupSuffix)
}

// finish 记录安装文件已替换（重启前不再允许更新）；受 Docker/systemd 托管时安排一次自重启。
func (s *Service) finish() bool {
	s.mu.Lock()
	s.pending = true
	s.mu.Unlock()
	if !s.canRestart() {
		return false
	}
	time.AfterFunc(s.restartDelay, s.restart)
	return true
}

func (s *Service) targets() []string {
	return []string{s.executable, s.staticPath, s.versionPath}
}

func (s *Service) backupsExist() bool {
	if s.executable == "" || s.staticPath == "" {
		return false
	}
	for _, target := range s.targets() {
		if _, err := os.Lstat(target + backupSuffix); err != nil {
			return false
		}
	}
	return true
}

func (s *Service) blockedReason(updateAvailable bool, release latestRelease, pending bool) string {
	if pending {
		return "安装文件已替换，重启服务后生效"
	}
	if reason := s.installBlocker(); reason != "" {
		return reason
	}
	switch {
	case !updateAvailable:
		return "没有可用的新版本"
	case release.ArchiveURL == "":
		return fmt.Sprintf("最新版本没有 %s/%s 安装包", runtime.GOOS, runtime.GOARCH)
	case release.ChecksumsURL == "":
		return "最新版本缺少 checksums.txt 校验文件"
	}
	return ""
}

// installBlocker 检查当前安装是否为可原地替换的发行包布局。
func (s *Service) installBlocker() string {
	current, ok := parseSemanticVersion(s.current)
	if !ok || s.executable == "" {
		return "源码运行/开发版本不支持在线更新"
	}
	data, err := os.ReadFile(s.versionPath)
	if err != nil {
		return "源码运行/开发版本不支持在线更新"
	}
	if installed, ok := parseSemanticVersion(string(data)); !ok || compareSemanticVersion(installed, current) != 0 {
		return "可执行文件目录中的 VERSION 与当前运行版本不一致"
	}
	if info, err := os.Stat(s.staticPath); err != nil || !info.IsDir() {
		return "未找到前端静态目录"
	}
	for _, dir := range []string{filepath.Dir(s.executable), filepath.Dir(s.staticPath)} {
		if !writable(dir) {
			return "安装目录不可写：" + dir
		}
	}
	return ""
}

func writable(dir string) bool {
	file, err := os.CreateTemp(dir, ".grok2api-write-check-*")
	if err != nil {
		return false
	}
	_ = file.Close()
	return os.Remove(file.Name()) == nil
}

func (s *Service) download(ctx context.Context, rawURL string, dst io.Writer, limit int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	if err := checkDownloadURL(request.URL); err != nil {
		return err
	}
	request.Header.Set("User-Agent", "grok2api/"+s.current)
	client := *s.client
	client.Timeout = downloadTimeout
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("重定向次数过多")
		}
		return checkDownloadURL(next.URL)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return errors.New("文件超过大小上限")
	}
	written, err := io.Copy(dst, io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if written > limit {
		return errors.New("文件超过大小上限")
	}
	return nil
}

func checkDownloadURL(target *url.URL) error {
	if target.Scheme != "https" || target.User != nil || !trustedDownloadHosts[strings.ToLower(target.Host)] {
		return fmt.Errorf("拒绝从不受信任的地址下载：%s://%s", target.Scheme, target.Host)
	}
	return nil
}

// findChecksum 从 sha256sum 格式（"<hex>  <文件名>"）的 checksums.txt 中取出指定文件的校验值。
func findChecksum(checksums, name string) (string, error) {
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt 中没有 %s 的校验值", name)
}

// extractArchive 只解出普通文件和目录，拒绝绝对路径、.. 与链接等条目，并限制总大小与文件数。
func extractArchive(archivePath, dir string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	decompressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer decompressed.Close()
	reader := tar.NewReader(decompressed)
	var total int64
	for entries := 0; ; entries++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if entries >= maxArchiveEntries {
			return errors.New("文件数量超过上限")
		}
		if !filepath.IsLocal(header.Name) || slices.Contains(strings.Split(header.Name, "/"), "..") {
			return fmt.Errorf("不安全的路径 %q", header.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			if total += header.Size; total > maxExtractedBytes {
				return errors.New("解压后大小超过上限")
			}
			err = writeEntry(target, reader)
		case tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("不支持的条目类型 %q", header.Name)
		}
		if err != nil {
			return err
		}
	}
}

func writeEntry(target string, content io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, content)
	return errors.Join(err, file.Close())
}

// checkStage 确认解压结果包含完整安装布局且 VERSION 与 Release 一致。
func checkStage(stage, tag string) error {
	data, err := os.ReadFile(filepath.Join(stage, "VERSION"))
	staged, stagedOK := parseSemanticVersion(string(data))
	expected, expectedOK := parseSemanticVersion(tag)
	if err != nil || !stagedOK || !expectedOK || compareSemanticVersion(staged, expected) != 0 {
		return fmt.Errorf("安装包中的 VERSION 与 %s 不一致", tag)
	}
	for _, name := range []string{"grok2api", "frontend/dist/index.html"} {
		if info, err := os.Lstat(filepath.Join(stage, filepath.FromSlash(name))); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("安装包缺少 %s", name)
		}
	}
	return os.Chmod(filepath.Join(stage, "grok2api"), 0o755)
}

// replace 依次把 sources 移到 targets，原文件移到 target+parkSuffix；任一步失败时撤销已完成的替换。
func replace(targets, sources []string, parkSuffix string) error {
	for _, target := range targets {
		if err := os.RemoveAll(target + parkSuffix); err != nil {
			return fmt.Errorf("清理 %s 失败：%w", target+parkSuffix, err)
		}
	}
	for index, target := range targets {
		err := os.Rename(target, target+parkSuffix)
		if err == nil {
			if err = os.Rename(sources[index], target); err != nil {
				err = errors.Join(err, os.Rename(target+parkSuffix, target))
			}
		}
		if err != nil {
			for undo := index - 1; undo >= 0; undo-- {
				err = errors.Join(err, os.Rename(targets[undo], sources[undo]), os.Rename(targets[undo]+parkSuffix, targets[undo]))
			}
			return fmt.Errorf("替换安装文件失败：%w", err)
		}
	}
	return nil
}

func executablePath() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// underSupervisor 判断进程退出后是否会被 Docker 重启策略或 systemd 拉起。
func underSupervisor() bool {
	if os.Getenv("INVOCATION_ID") != "" {
		return true
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// terminateSelf 向自身发送 SIGTERM，走正常的优雅关闭流程。
func terminateSelf() {
	if process, err := os.FindProcess(os.Getpid()); err == nil {
		_ = process.Signal(syscall.SIGTERM)
	}
}
