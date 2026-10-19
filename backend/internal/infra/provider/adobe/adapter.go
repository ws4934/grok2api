package adobe

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

var scope = egressdomain.Scope(account.ProviderAdobe)

// RemoteFetcher 下载客户端给的 https 输入图或参考视频（需自带 SSRF 防护）。
type RemoteFetcher func(ctx context.Context, rawURL string) ([]byte, error)

// Adapter 提供 Adobe Firefly 的图像与视频生成能力。
type Adapter struct {
	mu          sync.RWMutex
	cfg         Config
	egress      *infraegress.Manager
	cipher      *security.Cipher
	assets      provider.ImageAssetStore
	fetchRemote RemoteFetcher
	sherlock    *sherlockService
	logger      *slog.Logger
}

// NewAdapter 构造 Adobe Provider 适配器。assets 把生成的图像归档为网关可稳定
// 读取的本地资源；fetchRemote 为 nil 时只接受 data URI 输入。
func NewAdapter(cfg Config, egress *infraegress.Manager, cipher *security.Cipher, assets provider.ImageAssetStore, fetchRemote RemoteFetcher) *Adapter {
	a := &Adapter{cfg: cfg, egress: egress, cipher: cipher, assets: assets, fetchRemote: fetchRemote}
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

// requestContextFor 把存储的 token 解密成单次调用的凭据。x-arp-session-id 只在
// 提交前由 useSherlock 取，轮询/下载/续期不碰 mint。
func (a *Adapter) requestContextFor(credential account.Credential) (requestContext, error) {
	rc := requestContext{apiKey: defaultAPIKey, timeout: a.config().requestTimeout()}
	if credential.EncryptedAccessToken != "" {
		token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
		if err != nil {
			return requestContext{}, err
		}
		rc.token = token
	}
	return rc, nil
}

// withClient 以账号的浏览器身份获取出口租约，构建绑定其上的 client，执行 fn，把结果
// 反馈给出口健康度并释放租约。
func (a *Adapter) withClient(ctx context.Context, credential account.Credential, fn func(context.Context, *client) error) error {
	return a.withClientAs(ctx, credential, a.userAgentOf(credential), fn)
}

// withClientAs 用指定的浏览器身份（导入时账号还没有存下来的身份）执行 fn。
func (a *Adapter) withClientAs(ctx context.Context, credential account.Credential, userAgent string, fn func(context.Context, *client) error) error {
	lease, err := a.egress.AcquireCredential(infraegress.WithUserAgent(ctx, userAgent), scope, credential)
	if err != nil {
		return err
	}
	defer lease.Release()
	rc, err := a.requestContextFor(credential)
	if err != nil {
		return err
	}
	rc.userAgent = lease.UserAgent // 出口按这个 UA 选了 TLS 指纹，请求头必须一致
	runErr := fn(ctx, &client{do: lease, rc: rc})
	a.report(ctx, lease, runErr)
	return runErr
}

// userAgentOf 是账号导入时分到的浏览器身份；凭据状态里没有时用池里第一个。
func (a *Adapter) userAgentOf(credential account.Credential) string {
	if credential.EncryptedState != "" {
		if raw, err := a.cipher.Decrypt(credential.EncryptedState); err == nil {
			var state map[string]string
			if json.Unmarshal([]byte(raw), &state) == nil && strings.TrimSpace(state[stateUserAgent]) != "" {
				return strings.TrimSpace(state[stateUserAgent])
			}
		}
	}
	return browserUserAgents[0]
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

// useSherlock 在提交前取全局 x-arp-session-id。取不到时请求还没发出，返回
// 请求级 503（换账号无济于事）。
func (a *Adapter) useSherlock(ctx context.Context, c *client) error {
	token, err := a.sherlock.current(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.log().Warn("adobe_sherlock_unavailable", "error", err)
		return errMintUnavailable
	}
	c.rc.sherlock = token
	return nil
}

// submit 发送一次提交；Adobe 以 408 拒绝过期的 x-arp-session-id 时强制重铸后再发
// 一次。只重试 408：它是明确拒绝，5xx 时任务可能已被接受，重发会重复扣积分。
func (a *Adapter) submit(ctx context.Context, c *client, send func() (submission, error)) (submission, error) {
	sub, err := send()
	if !sessionRejected(err) {
		return sub, err
	}
	fresh, mintErr := a.sherlock.forceMint(ctx)
	if mintErr != nil {
		a.log().Warn("adobe_sherlock_remint_failed", "error", mintErr)
		return sub, errMintUnavailable
	}
	c.rc.sherlock = fresh
	if sub, err = send(); sessionRejected(err) {
		a.log().Warn("adobe_sherlock_rejected_after_remint", "error", err)
		return sub, errSessionRejected
	}
	return sub, err
}

func sessionRejected(err error) bool {
	var ue *upstreamError
	return errors.As(err, &ue) && ue.status == http.StatusRequestTimeout
}

// rejected 判断提交是否被明确拒绝（Adobe 4xx 或本地请求错误）：此时没有生成任务，
// 可以清掉提交标记。
func rejected(err error) bool {
	var ue *upstreamError
	var re *requestError
	return errors.As(err, &re) || (errors.As(err, &ue) && ue.status >= 400 && ue.status < 500)
}
