package adobe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/flight"
	"golang.org/x/sync/singleflight"
)

const (
	// sherlockRefresh 之后在后台换新 token（环境新鲜度是风控关键变量）。
	sherlockRefresh = 5 * time.Minute
	// sherlockTTL 是一枚 token 的有效寿命，过期后提交必须等新 token。
	sherlockTTL = 30 * time.Minute
	// mintTimeout 要盖住 sidecar 自己的预算（两次尝试共约 150 秒）。
	mintTimeout = 160 * time.Second
	// mintRetryGap 是两次后台刷新之间的最小间隔，mint 故障时不连续冲击它。
	mintRetryGap = 30 * time.Second
)

var errNoSherlockSource = errors.New("adobe: 未配置 mint 来源")

// sherlockService 维护一枚全局共享、与账号无关的 x-arp-session-id，通过 GET /mint
// 向 mint sidecar 拉取并缓存。
type sherlockService struct {
	cfg    func() Config
	client *http.Client
	flight singleflight.Group

	mu       sync.Mutex
	token    string
	mintedAt time.Time
	// loadedFrom 是当前 token 那次拉取的开始时间；更早开始的拉取不得覆盖它。
	loadedFrom time.Time
	// triedAt 是最近一次后台刷新的开始时间。
	triedAt time.Time
}

func newSherlockService(cfg func() Config) *sherlockService {
	return &sherlockService{cfg: cfg, client: &http.Client{}}
}

// current 返回一枚可用 token。超过刷新周期但仍在有效期内时立即返回缓存并在后台
// 刷新，提交不必等浏览器铸造；没有有效缓存时才同步拉取。
func (s *sherlockService) current(ctx context.Context) (string, error) {
	s.mu.Lock()
	token, age := s.token, time.Since(s.mintedAt)
	refresh := token != "" && age >= sherlockRefresh && age < sherlockTTL && time.Since(s.triedAt) >= mintRetryGap
	if refresh {
		s.triedAt = time.Now()
	}
	s.mu.Unlock()
	if token != "" && age < sherlockTTL {
		if refresh {
			go func() { _, _ = s.mint(context.Background(), false) }()
		}
		return token, nil
	}
	return s.mint(ctx, false)
}

// forceMint 让 sidecar 丢弃缓存立即重新铸造。
func (s *sherlockService) forceMint(ctx context.Context) (string, error) {
	return s.mint(ctx, true)
}

// mint 是共享加载：同类调用合并成一次，调用方离开不会取消它。fresh 用独立的
// key，避免强制刷新拿到正在进行的普通拉取（可能是 sidecar 缓存的旧 token）。
func (s *sherlockService) mint(ctx context.Context, fresh bool) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(s.cfg().SherlockMintAPI), "/")
	if base == "" {
		return "", errNoSherlockSource
	}
	key, endpoint := "mint", base+"/mint"
	if fresh {
		key, endpoint = "fresh", endpoint+"?fresh=1"
	}
	return flight.Do(ctx, &s.flight, key, mintTimeout, func(ctx context.Context) (string, error) {
		started := time.Now()
		value, err := fetchSherlock(ctx, s.client, endpoint)
		if err != nil {
			return "", err
		}
		s.mu.Lock()
		// 普通拉取可能晚于强制重铸返回（拿的是 sidecar 缓存的旧 token），不能覆盖新的。
		if started.After(s.loadedFrom) {
			s.token, s.mintedAt, s.loadedFrom = value, time.Now(), started
		}
		s.mu.Unlock()
		return value, nil
	})
}

// TestMint 按提交时的方式向 base 处的 mint 服务要一枚 token，供管理端测试未保存的
// 地址；不改动缓存的 token。
func (a *Adapter) TestMint(ctx context.Context, base string) error {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return errNoSherlockSource
	}
	ctx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	_, err := fetchSherlock(ctx, a.sherlock.client, base+"/mint")
	return err
}

// fetchSherlock 向 mint 服务拉取一枚 token 并校验结构；非 2xx 时带上服务给出的原因。
func fetchSherlock(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var body struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := "adobe: mint 服务返回 " + resp.Status
		if reason := strings.TrimSpace(body.Error); reason != "" {
			message += ": " + truncate(reason, 200)
		}
		return "", errors.New(message)
	}
	value := strings.TrimSpace(body.Token)
	if !validSherlock(value) {
		return "", errors.New("adobe: mint 服务未返回有效 sherlockToken")
	}
	return value, nil
}

// validSherlock 校验 x-arp-session-id 的结构是否完整。
func validSherlock(value string) bool {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(value); err != nil {
			return false
		}
	}
	var fields struct {
		SID string `json:"sid"`
		ARK string `json:"ark"`
		BFP string `json:"bfp"`
		FTR string `json:"ftr"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	return fields.SID != "" && fields.ARK != "" && fields.BFP != "" && fields.FTR != ""
}
