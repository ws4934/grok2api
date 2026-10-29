package relational

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Override only the historical CHECK, keeping the fixture's other columns
// identical to the current model so this test measures the constraint update.
type eightImageMediaJobModel struct {
	mediaJobModel
	InputImageCount int `gorm:"not null;default:0;check:chk_media_jobs_input_image_count,input_image_count BETWEEN 0 AND 8"`
}

func (eightImageMediaJobModel) TableName() string { return "media_jobs" }

type fifteenImageMediaJobModel struct {
	mediaJobModel
	InputImageCount int `gorm:"not null;default:0;check:chk_media_jobs_input_image_count,input_image_count BETWEEN 0 AND 15 AND (kind <> 'image' OR input_image_count <= 8)"`
}

func (fifteenImageMediaJobModel) TableName() string { return "media_jobs" }

func TestSQLiteMediaJobInputConstraintUpgradePreservesData(t *testing.T) {
	for _, limit := range []int{8, 15} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			verifyMediaJobInputConstraintUpgrade(t, openTestDatabase(t), limit)
		})
	}
}

func TestPostgresMediaJobInputConstraintUpgradePreservesData(t *testing.T) {
	for _, limit := range []int{8, 15} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) { verifyPostgresMediaJobInputConstraintUpgrade(t, limit) })
	}
}

func verifyPostgresMediaJobInputConstraintUpgrade(t *testing.T, limit int) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	database, err := OpenPostgres(ctx, dsn, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// Existing integration tests share a database and can already have 15-image
	// jobs. Keep the historical constraint and all fixture data in a rolled-back
	// schema, without imposing the former limit on their tables.
	rollback := errors.New("rollback isolated upgrade fixture")
	err = database.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		name := quotePostgresTestIdentifier("input_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
		if err := tx.Exec("CREATE SCHEMA " + name).Error; err != nil {
			return err
		}
		if err := tx.Exec("SET LOCAL search_path TO " + name).Error; err != nil {
			return err
		}
		scoped := &Database{db: tx, dialect: "postgres"}
		if err := scoped.InitializeSchema(ctx); err != nil {
			return err
		}
		verifyMediaJobInputConstraintUpgrade(t, scoped, limit)
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
}

func verifyMediaJobInputConstraintUpgrade(t *testing.T, database *Database, oldLimit int) {
	t.Helper()
	ctx := context.Background()
	db := database.db.WithContext(ctx)
	installOld := func() error {
		if database.dialect == "postgres" {
			if err := db.Migrator().DropConstraint(&mediaJobModel{}, mediaJobInputImageConstraint); err != nil {
				return err
			}
		}
		var oldModel any = &eightImageMediaJobModel{}
		if oldLimit == 15 {
			oldModel = &fifteenImageMediaJobModel{}
		}
		return db.Migrator().CreateConstraint(oldModel, mediaJobInputImageConstraint)
	}
	if database.dialect == "sqlite" {
		if err := database.withSQLiteForeignKeysDisabled(ctx, installOld); err != nil {
			t.Fatal(err)
		}
	} else if err := installOld(); err != nil {
		t.Fatal(err)
	}
	accountID, keyID, _ := mediaJobFixtureOwners(t, database, "input-upgrade")
	if err := db.Model(&clientKeyModel{}).Where("id = ?", keyID).Update("billing_limit_usd_ticks", 10_000).Error; err != nil {
		t.Fatal(err)
	}
	jobs, keys := NewMediaJobRepository(database), NewClientKeyRepository(database)
	now := time.Now().UTC()
	job := testMediaJob("video_input_upgrade_old", accountID, keyID, media.StatusQueued, now)
	job.InputImageCount, job.InputJSON = 8, `{"image_url":"https://example.com/first.png"}`
	job.IdempotencyKey, job.PayloadHash = "existing-request", strings.Repeat("a", 64)
	if err := jobs.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	before, err := jobs.GetMediaJob(ctx, job.ID, keyID)
	if err != nil {
		t.Fatal(err)
	}
	// A failed write gets its own savepoint on PostgreSQL, retaining the outer
	// fixture transaction for the initialization and preservation checks.
	setCount := func(count int, kind media.Kind) error {
		return db.Transaction(func(tx *gorm.DB) error {
			return tx.Model(&mediaJobModel{}).Where("id = ?", job.ID).Updates(map[string]any{"input_image_count": count, "kind": kind}).Error
		})
	}
	if err := setCount(oldLimit+1, media.KindVideo); err == nil {
		t.Fatal("fixture did not install the former image ceiling")
	}
	if err := NewAuditRepository(database).Create(ctx, audit.Record{
		EventID: "usage_input_upgrade_1", RequestID: job.RequestID, ClientKeyID: keyID, ModelRouteID: 1,
		ModelPublicID: job.Model, Provider: job.Provider, Operation: audit.OperationVideo,
		StatusCode: 200, CostInUSDTicks: 120, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if reserved, err := keys.ReserveBillingUsage(ctx, keyID, "reserve_input_upgrade_1", 400, now.Add(time.Hour)); err != nil || !reserved {
		t.Fatalf("reserve=%v err=%v", reserved, err)
	}
	beforeKey, err := keys.Get(ctx, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeKey.BilledUsageUSDTicks != 120 || beforeKey.ReservedUsageUSDTicks != 400 {
		t.Fatalf("billing fixture=%+v", beforeKey)
	}
	for _, value := range []any{
		&mediaAssetModel{ID: "asset_input_upgrade_1", Kind: "video", StorageKey: "upgrade/video.mp4", MIMEType: "video/mp4", SizeBytes: 16, SHA256: strings.Repeat("a", 64), CreatedAt: now},
		&mediaUploadTicketModel{TokenHash: strings.Repeat("b", 64), AssetID: "asset_input_upgrade_1", JobID: job.ID, MaxBytes: 100, AllowedMIME: "video/mp4", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	counts := map[string]int64{}
	for _, table := range []string{"media_jobs", "media_assets", "media_upload_tickets", "billing_reservations", "request_audits"} {
		counts[table] = tableRowCount(t, database, table)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	identity := inputConstraintIdentity(t, database)
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if inputConstraintIdentity(t, database) != identity {
		t.Fatal("initialization rebuilt an already current constraint/table")
	}
	after, err := jobs.GetMediaJob(ctx, job.ID, keyID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("job changed during upgrade: before=%+v after=%+v err=%v", before, after, err)
	}
	afterKey, err := keys.Get(ctx, keyID)
	if err != nil || afterKey.BilledUsageUSDTicks != beforeKey.BilledUsageUSDTicks || afterKey.ReservedUsageUSDTicks != beforeKey.ReservedUsageUSDTicks {
		t.Fatalf("billing changed: before=%+v after=%+v err=%v", beforeKey, afterKey, err)
	}
	for table, count := range counts {
		if got := tableRowCount(t, database, table); got != count {
			t.Errorf("%s rows=%d want=%d", table, got, count)
		}
	}
	if err := setCount(media.MaxVideoInputImages, media.KindVideo); err != nil {
		t.Fatalf("upgraded video ceiling: %v", err)
	}
	if err := setCount(media.MaxVideoInputImages+1, media.KindVideo); err == nil {
		t.Fatal("video accepted inputs beyond its ceiling")
	}
	if err := setCount(9, media.KindImage); err == nil {
		t.Fatal("image accepted nine inputs")
	}
	if err := setCount(8, media.KindImage); err != nil {
		t.Fatalf("image ceiling changed: %v", err)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		duplicate := job
		duplicate.ID = "video_input_upgrade_duplicate"
		return NewMediaJobRepository(&Database{db: tx, dialect: database.dialect}).CreateMediaJob(ctx, duplicate)
	})
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("idempotency index lost during rebuild: %v", err)
	}
	if database.dialect == "sqlite" {
		var enabled int
		if err := db.Raw("PRAGMA foreign_keys").Scan(&enabled).Error; err != nil || enabled != 1 {
			t.Fatalf("foreign keys=%d err=%v", enabled, err)
		}
	}
}

func inputConstraintIdentity(t *testing.T, database *Database) int64 {
	t.Helper()
	query := "SELECT rootpage FROM sqlite_master WHERE type = 'table' AND name = 'media_jobs'"
	if database.dialect == "postgres" {
		query = "SELECT oid FROM pg_constraint WHERE conrelid = to_regclass('media_jobs') AND conname = 'chk_media_jobs_input_image_count'"
	}
	var identity int64
	if err := database.db.Raw(query).Scan(&identity).Error; err != nil || identity == 0 {
		t.Fatalf("constraint/table identity=%d err=%v", identity, err)
	}
	return identity
}
