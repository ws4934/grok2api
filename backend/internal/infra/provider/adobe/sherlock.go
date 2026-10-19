package adobe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// sherlockTTL 是一枚 token 的有效寿命。
const sherlockTTL = 30 * time.Minute

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
}

func newSherlockService(cfg func() Config) *sherlockService {
	return &sherlockService{cfg: cfg, client: &http.Client{Timeout: 45 * time.Second}}
}

// current 返回一枚可用 token：超过刷新周期时向 mint 拉取；拉取失败而缓存仍有效则沿用缓存。
func (s *sherlockService) current(ctx context.Context) (string, error) {
	cfg := s.cfg()
	s.mu.Lock()
	token, age := s.token, time.Since(s.mintedAt)
	s.mu.Unlock()
	if token != "" && age < cfg.sherlockRefresh() {
		return token, nil
	}
	minted, err := s.mint(ctx, false)
	if err != nil {
		if token != "" && age < sherlockTTL {
			return token, nil // 铸造失败，回退到仍有效的缓存
		}
		return "", err
	}
	return minted, nil
}

// forceMint 丢弃缓存立即重新拉取。
func (s *sherlockService) forceMint(ctx context.Context) (string, error) {
	return s.mint(ctx, true)
}

func (s *sherlockService) mint(ctx context.Context, fresh bool) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(s.cfg().SherlockMintAPI), "/")
	if base == "" {
		return "", errNoSherlockSource
	}
	token, err, _ := s.flight.Do("mint", func() (any, error) {
		url := base + "/mint"
		if fresh {
			url += "?fresh=1"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := s.client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", errors.New("adobe: mint 服务返回 " + resp.Status)
		}
		var body struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(raw, &body) != nil || !validSherlock(strings.TrimSpace(body.Token)) {
			return "", errors.New("adobe: mint 服务未返回有效 sherlockToken")
		}
		value := strings.TrimSpace(body.Token)
		s.mu.Lock()
		s.token, s.mintedAt = value, time.Now()
		s.mu.Unlock()
		return value, nil
	})
	if err != nil {
		return "", err
	}
	return token.(string), nil
}
