package adobe

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

var scope = egressdomain.Scope(account.ProviderAdobe)

// Adapter 提供 Adobe Firefly 的图像与视频生成能力。
type Adapter struct {
	mu       sync.RWMutex
	cfg      Config
	egress   *infraegress.Manager
	cipher   *security.Cipher
	assets   provider.ImageAssetStore
	sherlock *sherlockService
	logger   *slog.Logger
}

// NewAdapter 构造 Adobe Provider 适配器。assets 把生成的图像归档为网关可稳定
// 读取的本地资源。
func NewAdapter(cfg Config, egress *infraegress.Manager, cipher *security.Cipher, assets provider.ImageAssetStore) *Adapter {
	a := &Adapter{cfg: cfg, egress: egress, cipher: cipher, assets: assets}
	a.sherlock = newSherlockService(a.config)
	return a
}

func (a *Adapter) Provider() account.Provider { return account.ProviderAdobe }

func (a *Adapter) SetLogger(logger *slog.Logger) { a.logger = logger }

func (a *Adapter) log() *slog.Logger {
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

func (a *Adapter) UpdateConfig(cfg Config) {
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
}

func (a *Adapter) config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// requestContextFor 把存储的 token 解密成单次调用的凭据，并取全局 x-arp-session-id
// （不需要它的调用即使取不到也不受影响）。
func (a *Adapter) requestContextFor(ctx context.Context, credential account.Credential) (requestContext, error) {
	cfg := a.config()
	rc := requestContext{apiKey: cfg.apiKey(), userAgent: cfg.userAgent()}
	if credential.EncryptedAccessToken != "" {
		token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
		if err != nil {
			return requestContext{}, err
		}
		rc.token = token
	}
	if token, err := a.sherlock.current(ctx); err == nil {
		rc.sherlock = token
	}
	return rc, nil
}

// withClient 为该凭据获取出口租约，构建绑定其上的 client，执行 fn，把结果
// 反馈给出口健康度并释放租约。
func (a *Adapter) withClient(ctx context.Context, credential account.Credential, fn func(context.Context, *client) error) error {
	lease, err := a.egress.AcquireCredential(ctx, scope, credential)
	if err != nil {
		return err
	}
	defer lease.Release()
	rc, err := a.requestContextFor(ctx, credential)
	if err != nil {
		return err
	}
	runErr := fn(ctx, &client{do: lease, rc: rc})
	a.report(ctx, lease, runErr)
	return runErr
}

// report 把一次结果反馈给出口健康度：传输失败归咎于出口节点，上游状态码不算。
func (a *Adapter) report(ctx context.Context, lease *infraegress.Lease, err error) {
	ctx = context.WithoutCancel(ctx)
	var transport *transportError
	switch {
	case err == nil:
		a.egress.FeedbackForScope(ctx, scope, lease.NodeID, http.StatusOK, nil)
	case errors.As(err, &transport):
		a.egress.FeedbackForScope(ctx, scope, lease.NodeID, 0, err)
	}
}

// pendingCredential 给没有账号行的导入/登录一个独立的粘滞出口身份；重新导入
// 已有账号时则沿用该账号的出口。
func (a *Adapter) pendingCredential(ctx context.Context, key string) account.Credential {
	if existing, ok := infraegress.ExistingAccount(ctx); ok && existing.Provider == account.ProviderAdobe {
		return existing
	}
	return account.Credential{Provider: account.ProviderAdobe, EgressIdentity: "adobe_pending_" + security.HashToken(key)[:24]}
}

// submitImageRemint 提交图像；若被 408 拒绝则刷新 x-arp-session-id 后重试一次。
func (a *Adapter) submitImageRemint(ctx context.Context, c *client, submitURL string, payloads []map[string]any, prompt string) (submission, error) {
	sub, err := c.submitImage(ctx, submitURL, payloads, prompt)
	if err == nil || !staleSherlock(err) {
		return sub, err
	}
	if fresh, mintErr := a.sherlock.forceMint(ctx); mintErr == nil && fresh != "" {
		c.rc.sherlock = fresh
		return c.submitImage(ctx, submitURL, payloads, prompt)
	}
	return sub, err
}

// submitVideoRemint 同 submitImageRemint，用于视频提交。
func (a *Adapter) submitVideoRemint(ctx context.Context, c *client, submitURL string, payload map[string]any) (submission, error) {
	sub, err := c.submitVideo(ctx, submitURL, payload)
	if err == nil || !staleSherlock(err) {
		return sub, err
	}
	if fresh, mintErr := a.sherlock.forceMint(ctx); mintErr == nil && fresh != "" {
		c.rc.sherlock = fresh
		return c.submitVideo(ctx, submitURL, payload)
	}
	return sub, err
}

// staleSherlock 判断提交失败是否可重试刷新 x-arp-session-id（408/503）。
func staleSherlock(err error) bool {
	var ue *upstreamError
	return errors.As(err, &ue) && (ue.status == 408 || ue.status == 503)
}

var sherlockCookiePattern = regexp.MustCompile(`(?:^|;\s*)sherlockToken=([^;]+)`)

// extractSherlock 从导入 Cookie 中读取 x-arp-session-id（仅用于导入时校验 Cookie 有效）。
func extractSherlock(cookie string) string {
	m := sherlockCookiePattern.FindStringSubmatch(cookie)
	if len(m) != 2 {
		return ""
	}
	value := strings.TrimSpace(m[1])
	if !validSherlock(value) {
		return ""
	}
	return value
}
