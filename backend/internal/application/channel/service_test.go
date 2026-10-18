package channel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	keyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/google/uuid"
)

type fixtureModule struct {
	failValidate bool
	mode         string
	submissions  int
	chatText     string
}

func (*fixtureModule) Descriptor() domain.ChannelDescriptor {
	return domain.ChannelDescriptor{ID: "fixture", Name: "Fixture", Version: 1, Capabilities: domain.Capabilities{Chat: true, Responses: true, ImageGeneration: true}}
}
func (*fixtureModule) ParseImport(b []byte) ([]domain.ImportRecord, error) {
	var v []domain.ImportRecord
	e := json.Unmarshal(b, &v)
	return v, e
}
func (*fixtureModule) Prepare(_ context.Context, c domain.Credentials) (domain.Credentials, error) {
	return c, nil
}
func (m *fixtureModule) Validate(context.Context, domain.Credentials) (domain.Snapshot, error) {
	if m.failValidate {
		return domain.Snapshot{}, errors.New("network")
	}
	return domain.Snapshot{Identity: domain.Identity{UserID: "u"}, Models: []string{"m"}, Quotas: []domain.FeatureQuota{}}, nil
}
func (*fixtureModule) Refresh(_ context.Context, c domain.Credentials) (domain.Credentials, error) {
	c.AccessToken = "rotated"
	c.RefreshToken = "new-RT"
	return c, nil
}
func (m *fixtureModule) Chat(ctx context.Context, _ domain.Credentials, _ domain.ChatRequest, emit func(domain.ChatEvent) error) error {
	if e := domain.ObserveQuotaSend(ctx, "reason"); e != nil {
		return e
	}
	return emit(domain.ChatEvent{Text: m.chatText, Done: true})
}
func (*fixtureModule) ChatQuota(domain.ChatRequest) []domain.QuotaNeed {
	return []domain.QuotaNeed{{Feature: "reason", Amount: 1}}
}
func (*fixtureModule) ImageQuota(domain.ImageRequest) []domain.QuotaNeed {
	return []domain.QuotaNeed{{Feature: "reason", Amount: 1}, {Feature: "image_gen", Amount: 1}}
}
func (*fixtureModule) PrepareImage(_ context.Context, _ domain.Credentials, r domain.ImageRequest, _ func(domain.ImageInput) (domain.Blob, error)) (domain.PreparedImage, error) {
	return domain.PreparedImage{Request: r}, nil
}
func (m *fixtureModule) SubmitImage(ctx context.Context, _ domain.Credentials, _ domain.PreparedImage, save func(domain.Checkpoint) error) (domain.Checkpoint, error) {
	m.submissions++
	cp := domain.Checkpoint{Version: 1, Submitted: true}
	for _, feature := range []string{"reason", "image_gen"} {
		if e := domain.ObserveQuotaSend(ctx, feature); e != nil {
			return cp, e
		}
	}
	if e := save(cp); e != nil {
		return cp, e
	}
	if m.mode == "uncertain" {
		return cp, errors.New("connection lost after write")
	}
	cp.ConversationID = "conversation"
	if e := save(cp); e != nil {
		return cp, e
	}
	if m.mode == "checkpoint" {
		return cp, errors.New("crash after checkpoint")
	}
	return cp, nil
}
func (*fixtureModule) PollImage(_ context.Context, _ domain.Credentials, cp domain.Checkpoint) (domain.Checkpoint, error) {
	cp.FileIDs = []string{"file"}
	return cp, nil
}
func (*fixtureModule) FetchImage(context.Context, domain.Credentials, domain.Checkpoint) ([]domain.Blob, error) {
	b, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=")
	return []domain.Blob{{Data: b, MIME: "image/png"}}, nil
}

type directAudit struct{ repo *relational.AuditRepository }

func (a directAudit) CreateDurable(ctx context.Context, r audit.Record) error {
	return a.repo.Create(ctx, r)
}

type fixture struct {
	s       *Service
	module  *fixtureModule
	repo    *relational.ChannelRepository
	keys    *relational.ClientKeyRepository
	key     clientkey.Key
	route   model.Route
	account domain.Account
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	db, e := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "channel.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	if e := db.InitializeSchema(ctx); e != nil {
		t.Fatal(e)
	}
	cipher, e := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if e != nil {
		t.Fatal(e)
	}
	m := &fixtureModule{chatText: "你好 world"}
	registry, e := domain.NewRegistry(m)
	if e != nil {
		t.Fatal(e)
	}
	repo := relational.NewChannelRepository(db)
	models := relational.NewModelRepository(db)
	keys := relational.NewClientKeyRepository(db)
	s := NewService(registry, repo, cipher, memory.NewLockStore(), models)
	s.SetClientKeys(keys)
	encrypted, _ := cipher.Encrypt("test-secret")
	key, e := keys.Create(ctx, clientkey.Key{Name: "fixture", Prefix: "fixture", SecretHash: strings.Repeat("a", 64), EncryptedSecret: encrypted, Enabled: true, RPMLimit: 60, MaxConcurrent: 4, BillingLimitUSDTicks: 1000000})
	if e != nil {
		t.Fatal(e)
	}
	billing := keyapp.NewService(keys, memory.NewRateLimiter(), memory.NewConcurrencyLimiter(), 60, 4, cipher)
	if e := s.SetExecution(billing, directAudit{relational.NewAuditRepository(db)}); e != nil {
		t.Fatal(e)
	}
	objects, e := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if e != nil {
		t.Fatal(e)
	}
	media := mediaapp.NewService(relational.NewMediaAssetRepository(db), relational.NewMediaJobRepository(db), objects, nil, mediaapp.Config{PublicBaseURL: "https://example.test", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30})
	s.SetImages(repo, media)
	result, e := s.Import(ctx, "fixture", []byte(`[{"name":"account","credentials":{"version":1,"access_token":"AT","refresh_token":"RT","client_profile":"fixture"}}]`), false)
	if e != nil || len(result) != 1 || result[0].Status != "imported" {
		t.Fatalf("import: %+v %v", result, e)
	}
	a, e := s.AccountAction(ctx, "fixture", result[0].AccountID, "validate")
	if e != nil {
		t.Fatal(e)
	}
	cfg := domain.DefaultSettings()
	cfg.Enabled = true
	cfg.ImageConversationModel = "m"
	cfg.OutputTokenLimit = 16
	if e := s.SaveSettings(ctx, "fixture", cfg); e != nil {
		t.Fatal(e)
	}
	for _, op := range []string{"chat", "image"} {
		if _, e := s.SavePrice(ctx, "fixture", domain.Price{Model: "m", Operation: op, InputPerMillionTicks: 1000000, OutputPerMillionTicks: 1000000, ImageTicks: 25}); e != nil {
			t.Fatal(e)
		}
	}
	route, e := s.CreateModel(ctx, "fixture", ModelInput{PublicID: "m", UpstreamModel: "m", Operation: "chat", Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	return fixture{s, m, repo, keys, key, route, a}
}
func TestChannelRegistrationBillingAndLocalLimit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	request := domain.ChatRequest{Messages: []domain.Message{{Role: "user", Content: []domain.ContentPart{{Type: "text", Text: "hello"}}}}}
	if _, e := f.s.Chat(ctx, f.key, f.route, request, false, func(domain.ChatEvent) error { return nil }); e == nil {
		t.Fatal("legacy all granted fixture access")
	}
	if e := f.s.SetGrant(ctx, "fixture", f.key.ID, true); e != nil {
		t.Fatal(e)
	}
	f.module.chatText = strings.Repeat("你好 world ", 100)
	got, e := f.s.Chat(ctx, f.key, f.route, request, true, func(domain.ChatEvent) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	if !got.Truncated || got.OutputTokens > 16 || got.OutputTokens == 0 {
		t.Fatalf("output bound: %+v", got)
	}
	key, e := f.keys.Get(ctx, f.key.ID)
	if e != nil || key.BilledUsageUSDTicks != got.InputTokens+got.OutputTokens || key.ReservedUsageUSDTicks != 0 {
		t.Fatalf("settlement: %+v %v", key, e)
	}
	before := key.BilledUsageUSDTicks
	f.module.chatText = ""
	if _, e := f.s.Chat(ctx, key, f.route, request, false, func(domain.ChatEvent) error { return nil }); e != nil {
		t.Fatal(e)
	}
	key, _ = f.keys.Get(ctx, key.ID)
	if key.BilledUsageUSDTicks != before {
		t.Fatal("empty result charged")
	}
	if e := f.s.SetGrant(ctx, "fixture", key.ID, false); e != nil {
		t.Fatal(e)
	}
	if rows, e := f.s.ClientModels(ctx, key); e != nil || len(rows) != 0 {
		t.Fatal("revoked channel still listed")
	}
}
func TestRefreshPersistsRotatedTokenBeforeVerificationFailure(t *testing.T) {
	f := newFixture(t)
	f.module.failValidate = true
	if _, e := f.s.AccountAction(context.Background(), "fixture", f.account.ID, "refresh"); e == nil {
		t.Fatal("sync error lost")
	}
	secret, e := f.repo.GetSecret(context.Background(), "fixture", f.account.ID)
	if e != nil {
		t.Fatal(e)
	}
	c, e := f.s.decrypt(secret)
	if e != nil || c.AccessToken != "rotated" || c.RefreshToken != "new-RT" {
		t.Fatalf("rotated credentials lost: %v", e)
	}
	if secret.FailureCount != 1 || secret.NextMaintenance == nil {
		t.Fatal("retry backoff missing")
	}
}
func TestImageCrashRecoveryDoesNotResubmitOrDoubleCharge(t *testing.T) {
	for _, mode := range []string{"uncertain", "checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			_ = f.s.SetGrant(ctx, "fixture", f.key.ID, true)
			_, e := f.s.SavePrice(ctx, "fixture", domain.Price{Model: "gpt-image-2", Operation: "image", ImageTicks: 25})
			if e != nil {
				t.Fatal(e)
			}
			route, e := f.s.CreateModel(ctx, "fixture", ModelInput{PublicID: "image", UpstreamModel: "gpt-image-2", Operation: "image", Enabled: true})
			if e != nil {
				t.Fatal(e)
			}
			resolved, matched, e := f.s.Resolve(ctx, "image", "image")
			if e != nil || !matched || resolved.ID != route.ID {
				t.Fatalf("virtual image route unavailable: %v %v", matched, e)
			}
			f.module.mode = mode
			job, e := f.s.SubmitImages(ctx, f.key, route, domain.ImageRequest{Operation: "image", Prompt: "test", N: 1}, "idempotent")
			if e != nil {
				t.Fatal(e)
			}
			_, item, e := f.repo.ClaimImageItem(ctx, account.Provider("fixture"), time.Now(), time.Now().Add(time.Minute), uuid.NewString())
			if e != nil {
				t.Fatal(e)
			}
			f.s.runImageItem(ctx, job, item)
			stored, e := f.repo.GetImageJob(ctx, job.ID, f.key.ID)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "uncertain" {
				if stored.Status != "uncertain" {
					t.Fatalf("uncertain marked %s", stored.Status)
				}
			} else {
				if stored.Status != "running" || stored.Items[0].Checkpoint.ConversationID == "" {
					t.Fatal("checkpoint lost")
				}
				_ = f.repo.ReleaseChannelSlot(ctx, item.ClaimToken)
				resumed, claim, e := f.repo.ClaimImageItem(ctx, "fixture", time.Now().Add(2*time.Minute), time.Now().Add(3*time.Minute), uuid.NewString())
				if e != nil {
					t.Fatal(e)
				}
				f.module.mode = ""
				f.s.runImageItem(ctx, resumed, claim)
				stored, e = f.repo.GetImageJob(ctx, job.ID, f.key.ID)
				if e != nil || stored.Status != "succeeded" {
					t.Fatalf("recovery failed: %+v %v", stored, e)
				}
				f.s.runImageItem(ctx, resumed, claim)
			}
			if f.module.submissions != 1 {
				t.Fatalf("resubmitted %d times", f.module.submissions)
			}
			key, _ := f.keys.Get(ctx, f.key.ID)
			want := int64(0)
			if mode == "checkpoint" {
				want = 25
			}
			if key.BilledUsageUSDTicks != want {
				t.Fatalf("charge = %d, want %d", key.BilledUsageUSDTicks, want)
			}
		})
	}
}

func TestImportIdentityDeduplicationPreservesAccountSettings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	name := "Keep this name"
	enabled := false
	concurrency := 3
	if _, e := f.s.UpdateAccount(ctx, "fixture", f.account.ID, AccountPatch{Name: &name, Enabled: &enabled, MaxConcurrent: &concurrency}); e != nil {
		t.Fatal(e)
	}
	for _, replace := range []bool{false, true} {
		rows, e := f.s.Import(ctx, "fixture", []byte(`[{"name":"incoming","credentials":{"version":1,"access_token":"new-AT","refresh_token":"different-RT","client_profile":"fixture"}}]`), replace)
		if e != nil || len(rows) != 1 {
			t.Fatal(e)
		}
		saved, e := f.s.AccountAction(ctx, "fixture", rows[0].AccountID, "validate")
		if replace && e != nil {
			t.Fatal(e)
		}
		if !replace && !errors.Is(e, domain.ErrConflict) {
			t.Fatalf("duplicate was not skipped: %v", e)
		}
		if saved.ID != f.account.ID || saved.Name != name || saved.Enabled || saved.MaxConcurrent != concurrency {
			t.Fatalf("account settings changed: %+v", saved)
		}
		_, total, e := f.s.Accounts(ctx, "fixture", 0, 50)
		if e != nil || total != 1 {
			t.Fatalf("duplicate account retained: %d %v", total, e)
		}
		secret, _ := f.repo.GetSecret(ctx, "fixture", saved.ID)
		credentials, _ := f.s.decrypt(secret)
		expected := "AT"
		if replace {
			expected = "new-AT"
		}
		if credentials.AccessToken != expected {
			t.Fatal("incorrect credential replacement")
		}
	}
}
