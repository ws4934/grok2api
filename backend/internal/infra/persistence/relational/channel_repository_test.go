package relational

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/google/uuid"
)

func channelDatabases(t *testing.T, run func(*testing.T, *Database)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { run(t, openTestDatabase(t)) })
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		db, err := OpenPostgres(context.Background(), dsn, 8, 8)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.InitializeSchema(context.Background()); err != nil {
			t.Fatal(err)
		}
		run(t, db)
	})
}
func channelFixture(t *testing.T, db *Database) (*ChannelRepository, channel.AccountSecret, uint64) {
	t.Helper()
	ctx := context.Background()
	r := NewChannelRepository(db)
	p := account.Provider("fixture_" + uuid.NewString()[:8])
	a := channel.AccountSecret{Account: channel.Account{Provider: p, Name: "Fixture", Enabled: true, MaxConcurrent: 2, Status: "verified", Models: []string{"model"}, Quotas: []channel.FeatureQuota{}}, EncryptedCredentials: testEncryptedToken, Fingerprint: uuid.NewString()}
	saved, err := r.SaveAccount(ctx, a, 0)
	if err != nil {
		t.Fatal(err)
	}
	a.Account = saved
	key := clientKeyModel{Name: uuid.NewString(), Prefix: uuid.NewString()[:8], SecretHash: strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", ""), EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
	if err := db.db.Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	return r, a, key.ID
}
func TestChannelMigrationCredentialsAndGrants(t *testing.T) {
	channelDatabases(t, func(t *testing.T, db *Database) {
		ctx := context.Background()
		r, a, key := channelFixture(t, db)
		if e := db.InitializeSchema(ctx); e != nil {
			t.Fatal(e)
		}
		cfg, e := r.Settings(ctx, a.Provider)
		if e != nil || cfg.Enabled || cfg.ImageWorkers != 2 {
			t.Fatalf("defaults: %+v %v", cfg, e)
		}
		if on, e := r.Granted(ctx, a.Provider, key); e != nil || on {
			t.Fatalf("legacy key gained permission: %v %v", on, e)
		}
		if e := r.SetGrant(ctx, a.Provider, key, true); e != nil {
			t.Fatal(e)
		}
		if on, e := r.Granted(ctx, a.Provider, key); e != nil || !on {
			t.Fatalf("grant: %v %v", on, e)
		}
		if e := r.SetGrant(ctx, a.Provider, key, false); e != nil {
			t.Fatal(e)
		}
		if on, _ := r.Granted(ctx, a.Provider, key); on {
			t.Fatal("revoked grant remains visible")
		}
		first, e := r.GetSecret(ctx, a.Provider, a.ID)
		if e != nil {
			t.Fatal(e)
		}
		newer := first
		newer.Name = "Edited"
		newer.Enabled = false
		if _, e := r.SaveAccount(ctx, newer, first.CredentialVersion); e != nil {
			t.Fatal(e)
		}
		first.Name = "stale"
		if _, e := r.SaveAccount(ctx, first, first.CredentialVersion); !errors.Is(e, channel.ErrConflict) {
			t.Fatalf("stale credential write: %v", e)
		}
		got, e := r.GetSecret(ctx, a.Provider, a.ID)
		if e != nil || got.Name != "Edited" || got.Enabled || got.CredentialVersion != 2 {
			t.Fatalf("lost update: %+v %v", got.Account, e)
		}
		if _, e := r.GetSecret(ctx, "another_channel", a.ID); !errors.Is(e, channel.ErrNotFound) {
			t.Fatalf("cross-channel secret: %v", e)
		}
	})
}
func TestChannelOAuthAtomicClaim(t *testing.T) {
	channelDatabases(t, func(t *testing.T, db *Database) {
		ctx := context.Background()
		r, a, _ := channelFixture(t, db)
		admin := adminModel{Username: uuid.NewString(), PasswordHash: "hash"}
		if e := db.db.Create(&admin).Error; e != nil {
			t.Fatal(e)
		}
		session := channel.OAuthSession{ID: uuid.NewString(), Provider: a.Provider, AdminID: admin.ID, EncryptedStart: "encrypted", StateHash: "state-hash", ExpiresAt: time.Now().Add(time.Minute)}
		if e := r.CreateOAuth(ctx, session); e != nil {
			t.Fatal(e)
		}
		for _, bad := range []struct {
			admin uint64
			state string
			now   time.Time
		}{{admin.ID + 100, "state-hash", time.Now()}, {admin.ID, "wrong", time.Now()}, {admin.ID, "state-hash", time.Now().Add(2 * time.Minute)}} {
			if _, e := r.ClaimOAuth(ctx, a.Provider, session.ID, bad.admin, bad.state, bad.now); !errors.Is(e, channel.ErrConflict) {
				t.Fatalf("invalid OAuth accepted: %v", e)
			}
		}
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, e := r.ClaimOAuth(ctx, a.Provider, session.ID, admin.ID, "state-hash", time.Now())
				results <- e
			}()
		}
		wg.Wait()
		close(results)
		successes := 0
		for e := range results {
			if e == nil {
				successes++
			} else if !errors.Is(e, channel.ErrConflict) {
				t.Fatal(e)
			}
		}
		if successes != 1 {
			t.Fatalf("OAuth exchanges allowed: %d", successes)
		}
		if e := r.FinishOAuth(ctx, session.ID, "completed"); e != nil {
			t.Fatal(e)
		}
		var row channelOAuthModel
		if e := db.db.First(&row, "id = ?", session.ID).Error; e != nil {
			t.Fatal(e)
		}
		if row.EncryptedStart != "" {
			t.Fatal("PKCE retained after completion")
		}
	})
}
func makeChannelJob(a channel.AccountSecret, key uint64, n int) channel.ImageJob {
	v := channel.ImageJob{ID: uuid.NewString(), ClientKeyID: key, Provider: a.Provider, Request: channel.ImageRequest{Operation: "image", Model: "model", N: n, Prompt: "test"}, Price: channel.Price{Version: 1}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour), PayloadHash: "same-content", IdempotencyKey: uuid.NewString()}
	for i := 0; i < n; i++ {
		v.Items = append(v.Items, channel.ImageItem{ID: uuid.NewString(), JobID: v.ID, Position: i})
	}
	return v
}
func TestChannelImageIdempotencyFencingAndPartialCompletion(t *testing.T) {
	channelDatabases(t, func(t *testing.T, db *Database) {
		ctx := context.Background()
		r, a, key := channelFixture(t, db)
		v := makeChannelJob(a, key, 2)
		stored, created, e := r.CreateImageJob(ctx, v)
		if e != nil || !created {
			t.Fatalf("create: %v %v", created, e)
		}
		duplicate := makeChannelJob(a, key, 2)
		duplicate.IdempotencyKey = v.IdempotencyKey
		again, created, e := r.CreateImageJob(ctx, duplicate)
		if e != nil || created || again.ID != stored.ID {
			t.Fatalf("idempotency: %v %v", created, e)
		}
		duplicate.PayloadHash = "different"
		if _, _, e := r.CreateImageJob(ctx, duplicate); !errors.Is(e, channel.ErrConflict) {
			t.Fatalf("hash conflict: %v", e)
		}
		if _, e := r.GetImageJob(ctx, v.ID, key+12345); !errors.Is(e, channel.ErrNotFound) {
			t.Fatalf("cross-key task: %v", e)
		}
		_, first, e := r.ClaimImageItem(ctx, a.Provider, time.Now(), time.Now().Add(time.Minute), uuid.NewString())
		if e != nil {
			t.Fatal(e)
		}
		if e := r.AssignImageAccount(ctx, first, a.ID); e != nil {
			t.Fatal(e)
		}
		cp := channel.Checkpoint{Version: 1, Submitted: true, ConversationID: "upstream-id"}
		if e := r.SaveImageCheckpoint(ctx, first, cp, "polling"); e != nil {
			t.Fatal(e)
		}
		if e := r.CancelImageJob(ctx, v.ID, key); !errors.Is(e, channel.ErrConflict) {
			t.Fatalf("submitted task canceled: %v", e)
		}
		if e := r.DeleteAccount(ctx, a.Provider, a.ID); !errors.Is(e, channel.ErrConflict) {
			t.Fatalf("active account deleted: %v", e)
		}
		if e := db.db.Model(&channelImageItemModel{}).Where("id = ?", first.ID).Update("lease_until", time.Now().Add(-time.Second)).Error; e != nil {
			t.Fatal(e)
		}
		var resumed channel.ImageItem
		var second channel.ImageItem
		for i := 0; i < 2; i++ {
			_, item, e := r.ClaimImageItem(ctx, a.Provider, time.Now(), time.Now().Add(time.Minute), uuid.NewString())
			if e != nil {
				t.Fatal(e)
			}
			if item.ID == first.ID {
				resumed = item
			} else {
				second = item
			}
		}
		if resumed.Checkpoint.ConversationID != "upstream-id" || !resumed.Submitted {
			t.Fatalf("lost checkpoint: %+v", resumed)
		}
		if e := r.SaveImageCheckpoint(ctx, first, cp, "stale"); !errors.Is(e, channel.ErrConflict) {
			t.Fatalf("stale writer: %v", e)
		}
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for i, item := range []channel.ImageItem{resumed, second} {
			wg.Add(1)
			go func(i int, item channel.ImageItem) {
				defer wg.Done()
				status := "succeeded"
				if i == 1 {
					status = "failed"
				}
				results <- r.CompleteImageItem(ctx, item, nil, status, "", nil)
			}(i, item)
		}
		wg.Wait()
		close(results)
		for e := range results {
			if e != nil {
				t.Fatal(e)
			}
		}
		end, e := r.GetImageJob(ctx, v.ID, key)
		if e != nil || end.Status != "partially_succeeded" {
			t.Fatalf("parent aggregation: %s %v", end.Status, e)
		}
		if e := r.CompleteImageItem(ctx, resumed, nil, "succeeded", "", nil); !errors.Is(e, channel.ErrConflict) {
			t.Fatalf("duplicate completion: %v", e)
		}
	})
}
func TestChannelQuotaSeparatesCapabilitiesAndUnsentUploads(t *testing.T) {
	channelDatabases(t, func(t *testing.T, db *Database) {
		ctx := context.Background()
		r, a, _ := channelFixture(t, db)
		zero, two := int64(0), int64(2)
		now := time.Now().Add(-time.Second)
		a.Quotas = []channel.FeatureQuota{{Feature: "image_gen", Remaining: &zero, ObservedAt: now}, {Feature: "file_upload", Remaining: &two, ObservedAt: now}}
		if _, e := r.SaveAccount(ctx, a, a.CredentialVersion); e != nil {
			t.Fatal(e)
		}
		if e := r.ReserveChannelQuota(ctx, a.ID, uuid.NewString(), []channel.QuotaNeed{{Feature: "image_gen", Amount: 1}}); e == nil {
			t.Fatal("exhausted image quota accepted")
		}
		if e := r.ReserveChannelQuota(ctx, a.ID, uuid.NewString(), []channel.QuotaNeed{{Feature: "reason", Amount: 1}}); e != nil {
			t.Fatalf("image quota disabled chat: %v", e)
		}
		event := uuid.NewString()
		if e := r.ReserveChannelQuota(ctx, a.ID, event, []channel.QuotaNeed{{Feature: "file_upload", Amount: 2}}); e != nil {
			t.Fatal(e)
		}
		if e := r.MarkChannelQuotaSent(ctx, event, "file_upload"); e != nil {
			t.Fatal(e)
		}
		if e := r.FinishChannelQuota(ctx, event, true); e != nil {
			t.Fatal(e)
		}
		if e := r.ReserveChannelQuota(ctx, a.ID, uuid.NewString(), []channel.QuotaNeed{{Feature: "file_upload", Amount: 1}}); e != nil {
			t.Fatalf("unattempted upload not released: %v", e)
		}
		if e := r.ReserveChannelQuota(ctx, a.ID, fmt.Sprint(uuid.NewString()), []channel.QuotaNeed{{Feature: "file_upload", Amount: 1}}); e == nil {
			t.Fatal("sent upload incorrectly refunded")
		}
	})
}

func TestChannelSlotRenewalKeepsLongExecutionsCounted(t *testing.T) {
	channelDatabases(t, func(t *testing.T, db *Database) {
		ctx := context.Background()
		r, a, _ := channelFixture(t, db)
		a.MaxConcurrent = 1
		if _, e := r.SaveAccount(ctx, a, a.CredentialVersion); e != nil {
			t.Fatal(e)
		}
		// An expired lease is reclaimable unless the holder renewed it in time.
		if e := r.AcquireChannelSlot(ctx, a.ID, "long-chat", time.Now().Add(-time.Second)); e != nil {
			t.Fatal(e)
		}
		if e := r.RenewChannelSlot(ctx, "long-chat", time.Now().Add(time.Hour)); e != nil {
			t.Fatal(e)
		}
		if e := r.AcquireChannelSlot(ctx, a.ID, "other", time.Now().Add(time.Minute)); !errors.Is(e, channel.ErrConflict) {
			t.Fatalf("renewed slot was reclaimed: %v", e)
		}
		if e := r.RenewChannelSlot(ctx, "missing", time.Now().Add(time.Minute)); !errors.Is(e, channel.ErrConflict) {
			t.Fatalf("renewing an unknown holder: %v", e)
		}
	})
}
