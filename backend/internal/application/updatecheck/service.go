package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/flight"
	"golang.org/x/sync/singleflight"
)

const (
	githubRepository = "ws4934/many2api"
	latestReleaseAPI = "https://api.github.com/repos/" + githubRepository + "/releases/latest"
	releasesPage     = "https://github.com/" + githubRepository + "/releases"
	maxReleaseBytes  = 1 << 20
	maxNotesRunes    = 4096
)

type Status string

const (
	StatusUnchecked       Status = "unchecked"
	StatusUpToDate        Status = "up_to_date"
	StatusUpdateAvailable Status = "update_available"
	StatusCheckFailed     Status = "check_failed"
)

type Snapshot struct {
	CurrentVersion  string     `json:"currentVersion"`
	LatestVersion   string     `json:"latestVersion"`
	UpdateAvailable bool       `json:"updateAvailable"`
	Status          Status     `json:"status"`
	CheckedAt       *time.Time `json:"checkedAt"`
	ReleaseURL      string     `json:"releaseUrl"`
	ReleaseNotes    string     `json:"releaseNotes"`
	Error           string     `json:"error"`
	// 以下字段在每次读取时按当前安装状态计算。
	CanUpdate           bool   `json:"canUpdate"`
	UpdateBlockedReason string `json:"updateBlockedReason"`
	CanRestart          bool   `json:"canRestart"`
	RollbackAvailable   bool   `json:"rollbackAvailable"`
}

type Service struct {
	current string
	client  *http.Client
	now     func() time.Time

	// 安装布局与重启钩子，测试中直接替换。
	latestURL    string
	executable   string
	staticPath   string
	versionPath  string
	restart      func()
	canRestart   func() bool
	restartDelay time.Duration

	mu         sync.RWMutex
	snapshot   Snapshot
	release    latestRelease
	pending    bool
	checks     singleflight.Group
	installing sync.Mutex
}

// NewService 创建更新服务；staticPath 是前端静态目录，在线更新时会一并替换。
func NewService(currentVersion, staticPath string, client *http.Client) *Service {
	currentVersion = strings.TrimSpace(currentVersion)
	if currentVersion == "" {
		currentVersion = "dev"
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if staticPath = strings.TrimSpace(staticPath); staticPath != "" {
		if absolute, err := filepath.Abs(staticPath); err == nil {
			staticPath = absolute
		}
	}
	executable := executablePath()
	versionPath := ""
	if executable != "" {
		versionPath = filepath.Join(filepath.Dir(executable), "VERSION")
	}
	return &Service{
		current:      currentVersion,
		client:       client,
		now:          time.Now,
		latestURL:    latestReleaseAPI,
		executable:   executable,
		staticPath:   staticPath,
		versionPath:  versionPath,
		restart:      terminateSelf,
		canRestart:   underSupervisor,
		restartDelay: time.Second,
		snapshot: Snapshot{
			CurrentVersion: currentVersion,
			Status:         StatusUnchecked,
		},
	}
}

func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	snapshot, release, pending := cloneSnapshot(s.snapshot), s.release, s.pending
	s.mu.RUnlock()
	snapshot.CanRestart = s.canRestart()
	snapshot.RollbackAvailable = s.backupsExist()
	snapshot.UpdateBlockedReason = s.blockedReason(snapshot.UpdateAvailable, release, pending)
	snapshot.CanUpdate = snapshot.UpdateBlockedReason == ""
	return snapshot
}

func (s *Service) Check(ctx context.Context) Snapshot {
	// The fetch is shared and detached, so a caller that leaves does not fail
	// the others; its own cancellation says nothing about the release.
	result, err := flight.Do(ctx, &s.checks, "latest", time.Minute, s.fetchLatest)
	if err != nil && ctx.Err() != nil {
		return s.Snapshot()
	}
	s.record(result, err)
	return s.Snapshot()
}

func (s *Service) record(release latestRelease, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.snapshot.Status = StatusCheckFailed
		s.snapshot.Error = err.Error()
		return
	}
	checkedAt := s.now().UTC()
	current, currentOK := parseSemanticVersion(s.current)
	latest, latestOK := parseSemanticVersion(release.Tag)
	if !currentOK || !latestOK {
		s.snapshot.LatestVersion = release.Tag
		s.snapshot.ReleaseURL = release.URL
		s.snapshot.ReleaseNotes = release.Notes
		s.snapshot.Status = StatusCheckFailed
		s.snapshot.Error = "当前版本或最新版本不是有效的语义化版本，无法比较"
		return
	}
	available := compareSemanticVersion(latest, current) > 0
	s.snapshot = Snapshot{
		CurrentVersion: s.current, LatestVersion: release.Tag,
		UpdateAvailable: available, CheckedAt: &checkedAt,
		ReleaseURL: release.URL, ReleaseNotes: release.Notes,
		Status: StatusUpToDate,
	}
	s.release = release
	if available {
		s.snapshot.Status = StatusUpdateAvailable
	}
}

type latestRelease struct {
	Tag   string
	URL   string
	Notes string
	// 当前平台安装包与 checksums.txt 的下载地址，缺失时为空。
	ArchiveURL   string
	ChecksumsURL string
}

func (s *Service) fetchLatest(ctx context.Context) (latestRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.latestURL, nil)
	if err != nil {
		return latestRelease{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "grok2api/"+s.current)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := s.client.Do(request)
	if err != nil {
		return latestRelease{}, fmt.Errorf("检查 GitHub Release 失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return latestRelease{}, fmt.Errorf("GitHub Release 检查失败（HTTP %d）", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxReleaseBytes+1))
	if err != nil {
		return latestRelease{}, fmt.Errorf("读取 GitHub Release 响应: %w", err)
	}
	if len(data) > maxReleaseBytes {
		return latestRelease{}, errors.New("GitHub Release 响应超过安全上限")
	}
	var payload struct {
		Tag     string `json:"tag_name"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return latestRelease{}, fmt.Errorf("解析 GitHub Release 响应: %w", err)
	}
	payload.Tag = strings.TrimSpace(payload.Tag)
	if payload.Tag == "" {
		return latestRelease{}, errors.New("GitHub Release 未返回版本号")
	}
	release := latestRelease{
		Tag:   payload.Tag,
		URL:   payload.HTMLURL,
		Notes: truncateRunes(strings.TrimSpace(payload.Body), maxNotesRunes),
	}
	if !strings.HasPrefix(release.URL, releasesPage+"/") {
		release.URL = releasesPage + "/tag/" + url.PathEscape(payload.Tag)
	}
	archive := releaseArchiveName(payload.Tag)
	for _, asset := range payload.Assets {
		switch asset.Name {
		case archive:
			release.ArchiveURL = asset.URL
		case "checksums.txt":
			release.ChecksumsURL = asset.URL
		}
	}
	return release, nil
}

// releaseArchiveName 返回当前平台的发行包文件名，例如 grok2api_v3.2.0_linux_amd64.tar.gz。
func releaseArchiveName(tag string) string {
	return fmt.Sprintf("grok2api_%s_%s_%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
}

type semanticVersion struct {
	major, minor, patch uint64
	prerelease          string
}

func parseSemanticVersion(value string) (semanticVersion, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	if before, _, ok := strings.Cut(value, "+"); ok {
		value = before
	}
	prerelease := ""
	if before, after, ok := strings.Cut(value, "-"); ok {
		value, prerelease = before, after
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semanticVersion{}, false
	}
	numbers := make([]uint64, 3)
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semanticVersion{}, false
		}
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semanticVersion{}, false
		}
		numbers[index] = value
	}
	return semanticVersion{major: numbers[0], minor: numbers[1], patch: numbers[2], prerelease: prerelease}, true
}

func compareSemanticVersion(left, right semanticVersion) int {
	for _, pair := range [][2]uint64{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if left.prerelease == right.prerelease {
		return 0
	}
	leftHotfix, leftHotfixNumber := projectHotfix(left.prerelease)
	rightHotfix, rightHotfixNumber := projectHotfix(right.prerelease)
	if leftHotfix && rightHotfix {
		if leftHotfixNumber < rightHotfixNumber {
			return -1
		}
		if leftHotfixNumber > rightHotfixNumber {
			return 1
		}
		return 0
	}
	if leftHotfix {
		return 1
	}
	if rightHotfix {
		return -1
	}
	if left.prerelease == "" {
		return 1
	}
	if right.prerelease == "" {
		return -1
	}
	return strings.Compare(left.prerelease, right.prerelease)
}

func projectHotfix(value string) (bool, uint64) {
	const prefix = "hotfix."
	if !strings.HasPrefix(value, prefix) {
		return false, 0
	}
	part := strings.TrimPrefix(value, prefix)
	if part == "" || (len(part) > 1 && part[0] == '0') {
		return false, 0
	}
	number, err := strconv.ParseUint(part, 10, 64)
	if err != nil {
		return false, 0
	}
	return true, number
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func cloneSnapshot(value Snapshot) Snapshot {
	if value.CheckedAt != nil {
		checkedAt := *value.CheckedAt
		value.CheckedAt = &checkedAt
	}
	return value
}
