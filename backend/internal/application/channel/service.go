// Package channel coordinates channel modules with shared gateway storage.
package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/google/uuid"
)

type Cipher interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}
type Service struct {
	clientKeys  repository.ClientKeyRepository
	environment func(context.Context, account.Provider, uint64) (context.Context, func(), error)
	images      *ImageExecution
	execution   *Execution
	registry    *domain.Registry
	repo        domain.Repository
	cipher      Cipher
	locks       repository.DistributedLock
	models      repository.ModelRepository

	requestTimeout time.Duration
	settingsHooks  []func(domain.ChannelDescriptor, domain.Settings)
}

// OnSettings registers a hook that receives each channel's settings when they
// are saved and on every maintenance pass, so peers converge within a minute.
func (s *Service) OnSettings(fn func(domain.ChannelDescriptor, domain.Settings)) {
	s.settingsHooks = append(s.settingsHooks, fn)
}

// ApplySettings pushes the stored settings of every channel to the hooks.
func (s *Service) ApplySettings(ctx context.Context) error {
	for _, d := range s.registry.Descriptors() {
		cfg, e := s.repo.Settings(ctx, d.ID)
		if e != nil {
			return e
		}
		for _, hook := range s.settingsHooks {
			hook(d, cfg)
		}
	}
	return nil
}

func (s *Service) SetClientKeys(keys repository.ClientKeyRepository) { s.clientKeys = keys }

func NewService(registry *domain.Registry, repo domain.Repository, cipher Cipher, locks repository.DistributedLock, models repository.ModelRepository) *Service {
	return &Service{registry: registry, repo: repo, cipher: cipher, locks: locks, models: models}
}

type View struct {
	Descriptor domain.ChannelDescriptor `json:"descriptor"`
	Settings   domain.Settings          `json:"settings"`
}

func (s *Service) module(p account.Provider) (domain.Module, error) {
	m, ok := s.registry.Get(p)
	if !ok {
		return nil, domain.ErrNotFound
	}
	return m, nil
}
func (s *Service) List(ctx context.Context) ([]View, error) {
	out := []View{}
	for _, d := range s.registry.Descriptors() {
		cfg, e := s.repo.Settings(ctx, d.ID)
		if e != nil {
			return nil, e
		}
		out = append(out, View{d, cfg})
	}
	return out, nil
}
func (s *Service) Settings(ctx context.Context, p account.Provider) (domain.Settings, error) {
	if _, e := s.module(p); e != nil {
		return domain.Settings{}, e
	}
	return s.repo.Settings(ctx, p)
}
func (s *Service) SaveSettings(ctx context.Context, p account.Provider, v domain.Settings) error {
	m, e := s.module(p)
	if e != nil {
		return e
	}
	if v.CloudflareClearance && m.Descriptor().ClearanceURL == "" {
		return domain.ErrUnsupported
	}
	if e := s.repo.SaveSettings(ctx, p, v); e != nil {
		return e
	}
	for _, hook := range s.settingsHooks {
		hook(m.Descriptor(), v)
	}
	return nil
}
func (s *Service) Accounts(ctx context.Context, p account.Provider, offset, limit int) ([]domain.Account, int64, error) {
	if _, e := s.module(p); e != nil {
		return nil, 0, e
	}
	return s.repo.ListAccounts(ctx, p, offset, limit)
}
func (s *Service) encrypt(c domain.Credentials) (string, error) {
	b, e := json.Marshal(c)
	if e != nil {
		return "", e
	}
	return s.cipher.Encrypt(string(b))
}
func (s *Service) decrypt(v domain.AccountSecret) (domain.Credentials, error) {
	raw, e := s.cipher.Decrypt(v.EncryptedCredentials)
	if e != nil {
		return domain.Credentials{}, errors.New("cannot decrypt channel credentials")
	}
	var c domain.Credentials
	e = json.Unmarshal([]byte(raw), &c)
	return c, e
}
func fingerprint(c domain.Credentials) string {
	value := c.AccessToken
	if c.RefreshToken != "" {
		value = c.RefreshToken
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func (s *Service) saveCredentials(ctx context.Context, v domain.AccountSecret, c domain.Credentials) (domain.Account, error) {
	encrypted, e := s.encrypt(c)
	if e != nil {
		return domain.Account{}, e
	}
	v.EncryptedCredentials = encrypted
	v.Fingerprint = fingerprint(c)
	v.Refreshable = c.RefreshToken != "" && c.ClientProfile != ""
	v.ExpiresAt = c.ExpiresAt
	return s.repo.SaveAccount(ctx, v, v.CredentialVersion)
}

type ImportResult struct {
	Index     int    `json:"index"`
	Status    string `json:"status"`
	AccountID uint64 `json:"account_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (s *Service) Import(ctx context.Context, p account.Provider, data []byte, replace bool) ([]ImportResult, error) {
	m, e := s.module(p)
	if e != nil {
		return nil, e
	}
	records, e := m.(domain.AccountLifecycle).ParseImport(data)
	if e != nil {
		return nil, e
	}
	out := make([]ImportResult, 0, len(records))
	cfg, e := s.repo.Settings(ctx, p)
	if e != nil {
		return nil, e
	}
	for i, record := range records {
		result := ImportResult{Index: i + 1}
		if record.Error != "" {
			result.Status = "failed"
			result.Reason = record.Error
			out = append(out, result)
			continue
		}
		v, e := s.repo.FindAccount(ctx, p, fingerprint(record.Credentials), domain.Identity{})
		if e == nil && !replace {
			result.Status = "duplicate"
			result.AccountID = v.ID
			out = append(out, result)
			continue
		}
		if e != nil && !errors.Is(e, domain.ErrNotFound) {
			return out, e
		}
		secret := domain.AccountSecret{Account: domain.Account{Provider: p, Name: record.Name, Enabled: true, MaxConcurrent: cfg.AccountConcurrency, Status: "pending", Models: []string{}, Quotas: []domain.FeatureQuota{}}}
		if secret.Name == "" {
			secret.Name = m.Descriptor().Name + " account"
		}
		if e == nil {
			secret.Account = v
			secret.Status = "pending"
			secret.Failure = ""
		}
		secret.ReplaceDuplicate = replace
		saved, e := s.saveCredentials(ctx, secret, record.Credentials)
		if e != nil {
			result.Status = "failed"
			result.Reason = "account could not be stored"
		} else {
			result.Status = "imported"
			result.AccountID = saved.ID
		}
		out = append(out, result)
	}
	return out, nil
}
func (s *Service) Export(ctx context.Context, p account.Provider, ids []uint64) ([]domain.ImportRecord, error) {
	if _, e := s.module(p); e != nil {
		return nil, e
	}
	if len(ids) == 0 {
		for offset := 0; ; offset += 200 {
			rows, total, e := s.repo.ListAccounts(ctx, p, offset, 200)
			if e != nil {
				return nil, e
			}
			for _, a := range rows {
				ids = append(ids, a.ID)
			}
			if offset+len(rows) >= int(total) {
				break
			}
		}
	}
	out := []domain.ImportRecord{}
	for _, id := range ids {
		v, e := s.repo.GetSecret(ctx, p, id)
		if e != nil {
			return nil, e
		}
		c, e := s.decrypt(v)
		if e != nil {
			return nil, e
		}
		out = append(out, domain.ImportRecord{Name: v.Name, Credentials: c})
	}
	return out, nil
}

type AccountPatch struct {
	Name          *string             `json:"name"`
	Enabled       *bool               `json:"enabled"`
	MaxConcurrent *int                `json:"max_concurrent"`
	Credentials   *domain.Credentials `json:"credentials"`
}

func (s *Service) UpdateAccount(ctx context.Context, p account.Provider, id uint64, patch AccountPatch) (domain.Account, error) {
	if _, e := s.module(p); e != nil {
		return domain.Account{}, e
	}
	release, ok, e := s.locks.Acquire(ctx, fmt.Sprintf("channel:account:%s:%d", p, id), 2*time.Minute)
	if e != nil {
		return domain.Account{}, e
	}
	if !ok {
		return domain.Account{}, domain.ErrConflict
	}
	defer release()
	v, e := s.repo.GetSecret(ctx, p, id)
	if e != nil {
		return domain.Account{}, e
	}
	if patch.Name != nil {
		v.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.Enabled != nil {
		v.Enabled = *patch.Enabled
	}
	if patch.MaxConcurrent != nil {
		v.MaxConcurrent = *patch.MaxConcurrent
	}
	if patch.Credentials != nil {
		c := *patch.Credentials
		if c.AccessToken == "" && c.RefreshToken == "" {
			return domain.Account{}, errors.New("replacement credentials are empty")
		}
		c.Version = 1
		v.Status = "pending"
		v.Failure = ""
		return s.saveCredentials(ctx, v, c)
	}
	return s.repo.SaveAccount(ctx, v, v.CredentialVersion)
}
func (s *Service) DeleteAccount(ctx context.Context, p account.Provider, id uint64) error {
	if _, e := s.module(p); e != nil {
		return e
	}
	return s.repo.DeleteAccount(ctx, p, id)
}
func (s *Service) AccountAction(ctx context.Context, p account.Provider, id uint64, action string) (domain.Account, error) {
	m, e := s.module(p)
	if e != nil {
		return domain.Account{}, e
	}
	if action != "validate" && action != "refresh" && action != "sync" {
		return domain.Account{}, errors.New("unsupported account action")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	release, ok, e := s.locks.Acquire(ctx, fmt.Sprintf("channel:account:%s:%d", p, id), 2*time.Minute)
	if e != nil {
		return domain.Account{}, e
	}
	if !ok {
		return domain.Account{}, domain.ErrConflict
	}
	defer release()
	v, e := s.repo.GetSecret(ctx, p, id)
	if e != nil {
		return domain.Account{}, e
	}
	c, e := s.decrypt(v)
	if e != nil {
		return domain.Account{}, e
	}
	ctx, releaseEnvironment, e := s.withEnvironment(ctx, p, id)
	if e != nil {
		return domain.Account{}, e
	}
	defer releaseEnvironment()
	life := m.(domain.AccountLifecycle)
	if action == "refresh" {
		c, e = life.Refresh(ctx, c)
	} else {
		c, e = life.Prepare(ctx, c)
	}
	if e == nil {
		persist, end := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		saved, saveErr := s.saveCredentials(persist, v, c)
		end()
		if saveErr != nil {
			return domain.Account{}, saveErr
		}
		v.Account = saved
		v.EncryptedCredentials, _ = s.encrypt(c)
		v.Fingerprint = fingerprint(c)
	}
	if e != nil {
		return s.recordFailure(ctx, m, v, e)
	}
	observed := time.Now().UTC()
	snap, e := life.Validate(ctx, c)
	if e != nil {
		return s.recordFailure(ctx, m, v, e)
	}
	if existing, err := s.repo.FindAccount(ctx, p, "", snap.Identity); err == nil && existing.ID != id {
		if v.Identity.UserID == "" {
			if v.ReplaceDuplicate {
				releaseExisting, acquired, err := s.locks.Acquire(ctx, fmt.Sprintf("channel:account:%s:%d", p, existing.ID), 2*time.Minute)
				if err != nil {
					return domain.Account{}, err
				}
				if !acquired {
					return domain.Account{}, domain.ErrConflict
				}
				defer releaseExisting()
				target, err := s.repo.GetSecret(ctx, p, existing.ID)
				if err != nil {
					return domain.Account{}, err
				}
				target.Identity, target.Models, target.Quotas = snap.Identity, snap.Models, snap.Quotas
				for i := range target.Quotas {
					target.Quotas[i].ObservedAt = observed
				}
				target.Status, target.Failure, target.ReplaceDuplicate = "verified", "", false
				target.SyncedAt = &observed
				existing, err = s.saveCredentials(ctx, target, c)
				if err != nil {
					return domain.Account{}, err
				}
				if s.models != nil {
					if err := s.models.ReplaceAccountCapabilities(ctx, existing.ID, snap.Models, observed); err != nil {
						return existing, err
					}
				}
			}
			if err := s.repo.DeleteAccount(ctx, p, id); err != nil {
				return existing, err
			}
			if v.ReplaceDuplicate {
				return existing, nil
			}
			return existing, domain.ErrConflict
		}
		v.Status = "duplicate"
		v.Failure = "upstream identity already belongs to another account"
		return s.repo.SaveAccount(ctx, v, v.CredentialVersion)
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.Account{}, err
	}
	now := time.Now().UTC()
	v.Identity = snap.Identity
	v.Models = snap.Models
	for i := range snap.Quotas {
		snap.Quotas[i].ObservedAt = observed
	}
	v.Quotas = snap.Quotas
	v.SyncedAt = &now
	v.Status = "verified"
	v.ReplaceDuplicate = false
	v.Failure = ""
	v.FailureCount = 0
	next := now.Add(15 * time.Minute)
	if v.Refreshable && v.ExpiresAt != nil && v.ExpiresAt.Add(-5*time.Minute).Before(next) {
		next = v.ExpiresAt.Add(-5 * time.Minute)
		if next.Before(now) {
			next = now.Add(time.Minute)
		}
	}
	v.NextMaintenance = &next
	saved, e := s.repo.SaveAccount(ctx, v, v.CredentialVersion)
	if e != nil {
		return saved, e
	}
	if s.models != nil {
		e = s.models.ReplaceAccountCapabilities(ctx, id, snap.Models, now)
	}
	return saved, e
}
func (s *Service) recordFailure(ctx context.Context, m domain.Module, v domain.AccountSecret, err error) (domain.Account, error) {
	v.FailureCount++
	next := time.Now().UTC().Add(time.Minute * time.Duration(1<<min(v.FailureCount-1, 6)))
	v.NextMaintenance = &next
	v.Failure = "upstream synchronization failed"
	if classifier, ok := m.(domain.FailureClassifier); ok {
		f := classifier.Classify(err)
		v.Failure = f.Message
		if f.Kind == domain.FailureAuth {
			v.Status = "reauth_required"
		}
	}
	if v.Status != "verified" && v.Status != "reauth_required" {
		v.Status = "pending"
	}
	persist, end := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer end()
	saved, e := s.repo.SaveAccount(persist, v, v.CredentialVersion)
	if e != nil {
		return saved, e
	}
	return saved, errors.New(v.Failure)
}

type OAuthView struct {
	ID        string    `json:"id"`
	URL       string    `json:"authorize_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Service) StartOAuth(ctx context.Context, p account.Provider, adminID uint64) (OAuthView, error) {
	m, e := s.module(p)
	if e != nil {
		return OAuthView{}, e
	}
	o, ok := m.(domain.OAuthAdapter)
	if !ok {
		return OAuthView{}, domain.ErrUnsupported
	}
	start, e := o.StartOAuth(ctx)
	if e != nil {
		return OAuthView{}, e
	}
	b, e := json.Marshal(start)
	if e != nil {
		return OAuthView{}, e
	}
	encrypted, e := s.cipher.Encrypt(string(b))
	if e != nil {
		return OAuthView{}, e
	}
	v := OAuthView{ID: uuid.NewString(), URL: start.URL, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}
	hash := sha256.Sum256([]byte(start.State))
	e = s.repo.CreateOAuth(ctx, domain.OAuthSession{ID: v.ID, Provider: p, AdminID: adminID, EncryptedStart: encrypted, StateHash: hex.EncodeToString(hash[:]), ExpiresAt: v.ExpiresAt})
	return v, e
}
func (s *Service) CompleteOAuth(ctx context.Context, p account.Provider, id string, adminID uint64, callback string) (domain.Account, error) {
	m, e := s.module(p)
	if e != nil {
		return domain.Account{}, e
	}
	o, ok := m.(domain.OAuthAdapter)
	if !ok {
		return domain.Account{}, domain.ErrUnsupported
	}
	u, e := url.Parse(callback)
	if e != nil || len(callback) > 16384 {
		return domain.Account{}, errors.New("invalid callback URL")
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil || len(q["state"]) != 1 || q.Get("state") == "" {
		return domain.Account{}, errors.New("complete callback URL with state is required")
	}
	hash := sha256.Sum256([]byte(q.Get("state")))
	session, e := s.repo.ClaimOAuth(ctx, p, id, adminID, hex.EncodeToString(hash[:]), time.Now().UTC())
	if e != nil {
		return domain.Account{}, e
	}
	status := "failed"
	defer func() {
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.repo.FinishOAuth(finish, id, status)
	}()
	raw, e := s.cipher.Decrypt(session.EncryptedStart)
	if e != nil {
		return domain.Account{}, e
	}
	var start domain.OAuthStart
	if e = json.Unmarshal([]byte(raw), &start); e != nil {
		return domain.Account{}, e
	}
	c, e := o.ExchangeOAuth(ctx, start, callback)
	if e != nil {
		return domain.Account{}, e
	}
	// Persist exchanged tokens even if the HTTP caller disconnects during validation.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	v, e := s.saveCredentials(saveCtx, domain.AccountSecret{Account: domain.Account{Provider: p, Name: "ChatGPT OAuth", Enabled: true, MaxConcurrent: 1, Status: "pending", Models: []string{}, Quotas: []domain.FeatureQuota{}}}, c)
	if e != nil {
		return domain.Account{}, e
	}
	status = "completed"
	// Validation is separate from exchange durability: a failed Web check must
	// leave the freshly issued token pair available for an administrator.
	validated, verifyErr := s.AccountAction(context.WithoutCancel(ctx), p, v.ID, "validate")
	if validated.ID != 0 {
		return validated, nil
	}
	if verifyErr != nil {
		return v, nil
	}
	return validated, nil
}
func (s *Service) Grants(ctx context.Context, p account.Provider) ([]uint64, error) {
	if _, e := s.module(p); e != nil {
		return nil, e
	}
	return s.repo.Grants(ctx, p)
}
func (s *Service) SetGrant(ctx context.Context, p account.Provider, id uint64, enabled bool) error {
	if _, e := s.module(p); e != nil {
		return e
	}
	if id == 0 {
		return errors.New("client key is required")
	}
	return s.repo.SetGrant(ctx, p, id, enabled)
}
func (s *Service) Prices(ctx context.Context, p account.Provider) ([]domain.Price, error) {
	if _, e := s.module(p); e != nil {
		return nil, e
	}
	return s.repo.Prices(ctx, p)
}
func (s *Service) SavePrice(ctx context.Context, p account.Provider, v domain.Price) (domain.Price, error) {
	if _, e := s.module(p); e != nil {
		return v, e
	}
	v.Provider = p
	if v.Model == "" || len(v.Model) > 255 || v.InputPerMillionTicks < 0 || v.OutputPerMillionTicks < 0 || v.ImageTicks < 0 || v.InputPerMillionTicks > 1000000000000 || v.OutputPerMillionTicks > 1000000000000 || v.ImageTicks > 1000000000000 {
		return v, errors.New("invalid price")
	}
	switch v.Operation {
	case "chat", "image", "image_edit":
	default:
		return v, errors.New("invalid priced operation")
	}
	return s.repo.SavePrice(ctx, v)
}

// RunMaintenance is independent of gateway readiness and stops on application shutdown.
func (s *Service) RunMaintenance(ctx context.Context) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if s.images != nil {
				_ = s.images.jobs.CleanupChannelData(ctx, time.Now().UTC())
			}
			_ = s.ApplySettings(ctx)
			for _, d := range s.registry.Descriptors() {
				cfg, e := s.repo.Settings(ctx, d.ID)
				if e != nil || !cfg.Enabled {
					continue
				}
				for offset := 0; ; offset += 200 {
					rows, total, e := s.repo.ListAccounts(ctx, d.ID, offset, 200)
					if e != nil {
						break
					}
					for _, a := range rows {
						now := time.Now().UTC()
						if !a.Enabled || a.Status == "reauth_required" || a.Status == "duplicate" || (a.NextMaintenance != nil && a.NextMaintenance.After(now)) {
							continue
						}
						action := "sync"
						if a.Status == "pending" {
							action = "validate"
						}
						if a.Refreshable && a.ExpiresAt != nil && a.ExpiresAt.Before(now.Add(5*time.Minute)) {
							action = "refresh"
						}
						_, _ = s.AccountAction(ctx, d.ID, a.ID, action)
					}
					if offset+len(rows) >= int(total) {
						break
					}
				}
			}
		}
	}
}

func (s *Service) SetEnvironment(fn func(context.Context, account.Provider, uint64) (context.Context, func(), error)) {
	s.environment = fn
}
func (s *Service) withEnvironment(ctx context.Context, p account.Provider, id uint64) (context.Context, func(), error) {
	if s.environment == nil {
		return ctx, func() {}, nil
	}
	return s.environment(ctx, p, id)
}
