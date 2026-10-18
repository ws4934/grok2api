package relational

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/channel"

	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ channel.JobRepository = (*ChannelRepository)(nil)

func lockImageParent(tx *gorm.DB, id string) error {
	var parent channelImageJobModel
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&parent, "id = ?", id).Error
}

func jobDomain(r channelImageJobModel) (channel.ImageJob, error) {
	v := channel.ImageJob{ID: r.ID, ClientKeyID: r.ClientKeyID, Provider: account.Provider(r.Provider), ModelRouteID: r.ModelRouteID, Status: r.Status, CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, ExpiresAt: r.ExpiresAt, PayloadHash: r.PayloadHash}
	if r.IdempotencyKey != nil {
		v.IdempotencyKey = *r.IdempotencyKey
	}
	if e := json.Unmarshal([]byte(r.RequestJSON), &v.Request); e != nil {
		return v, e
	}
	if e := json.Unmarshal([]byte(r.BindingsJSON), &v.BoundAccountIDs); e != nil {
		return v, e
	}
	e := json.Unmarshal([]byte(r.PriceJSON), &v.Price)
	return v, e
}
func itemDomain(r channelImageItemModel) (channel.ImageItem, error) {
	v := channel.ImageItem{ID: r.ID, JobID: r.JobID, Position: r.Position, Status: r.Status, Stage: r.Stage, Submitted: r.Submitted, Error: r.Error, ClaimToken: r.ClaimToken, Generation: r.Generation, LeaseUntil: r.LeaseUntil, StartedAt: r.StartedAt, Width: r.Width, Height: r.Height, MIME: r.MIME}
	if r.AccountID != nil {
		v.AccountID = *r.AccountID
	}
	if e := json.Unmarshal([]byte(r.CheckpointJSON), &v.Checkpoint); e != nil {
		return v, e
	}
	e := json.Unmarshal([]byte(r.AssetsJSON), &v.AssetIDs)
	return v, e
}
func (r *ChannelRepository) CreateImageJob(ctx context.Context, v channel.ImageJob) (channel.ImageJob, bool, error) {
	if v.Request.N < 1 || v.Request.N > 10 || len(v.Items) != v.Request.N {
		return v, false, errors.New("invalid image child count")
	}
	created := false
	request, e := json.Marshal(v.Request)
	if e != nil {
		return v, false, e
	}
	price, e := json.Marshal(v.Price)
	if e != nil {
		return v, false, e
	}
	bindings, e := json.Marshal(v.BoundAccountIDs)
	if e != nil {
		return v, false, e
	}
	e = r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if v.IdempotencyKey != "" {
			var old channelImageJobModel
			e := tx.First(&old, "client_key_id = ? AND idempotency_key = ?", v.ClientKeyID, v.IdempotencyKey).Error
			if e == nil {
				if old.PayloadHash != v.PayloadHash {
					return channel.ErrConflict
				}
				v.ID = old.ID
				return nil
			}
			if !errors.Is(e, gorm.ErrRecordNotFound) {
				return e
			}
		}
		row := channelImageJobModel{ID: v.ID, ClientKeyID: v.ClientKeyID, Provider: string(v.Provider), ModelRouteID: v.ModelRouteID, BindingsJSON: string(bindings), RequestJSON: string(request), PriceJSON: string(price), Status: "queued", PayloadHash: v.PayloadHash, CreatedAt: v.CreatedAt, ExpiresAt: v.ExpiresAt}
		if v.IdempotencyKey != "" {
			row.IdempotencyKey = &v.IdempotencyKey
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var old channelImageJobModel
			if v.IdempotencyKey == "" {
				return channel.ErrConflict
			}
			if err := tx.First(&old, "client_key_id = ? AND idempotency_key = ?", v.ClientKeyID, v.IdempotencyKey).Error; err != nil {
				return err
			}
			if old.PayloadHash != v.PayloadHash {
				return channel.ErrConflict
			}
			v.ID = old.ID
			return nil
		}
		for i := 0; i < v.Request.N; i++ {
			item := channelImageItemModel{ID: v.Items[i].ID, JobID: v.ID, Position: i, Status: "queued", Stage: "queued", CheckpointJSON: "{}", AssetsJSON: "[]", UpdatedAt: time.Now().UTC()}
			if e := tx.Create(&item).Error; e != nil {
				return e
			}
		}
		for _, input := range v.Request.Inputs {
			if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&channelAssetRefModel{JobID: v.ID, AssetID: input.AssetID}).Error; e != nil {
				return e
			}
		}
		if v.Request.Mask != nil {
			if e := tx.Create(&channelAssetRefModel{JobID: v.ID, AssetID: v.Request.Mask.AssetID}).Error; e != nil {
				return e
			}
		}
		// Inputs must remain readable throughout the queue and execution window.
		protectedUntil := v.ExpiresAt.Add(time.Hour)
		if e := tx.Model(&mediaAssetModel{}).Where("id IN (?) AND expires_at < ?", tx.Model(&channelAssetRefModel{}).Select("asset_id").Where("job_id = ?", v.ID), protectedUntil).Update("expires_at", protectedUntil).Error; e != nil {
			return e
		}
		created = true
		return nil
	})
	if e != nil {
		return v, false, e
	}
	job, e := r.GetImageJob(ctx, v.ID, v.ClientKeyID)
	return job, created, e
}
func (r *ChannelRepository) GetImageJob(ctx context.Context, id string, key uint64) (channel.ImageJob, error) {
	var row channelImageJobModel
	q := r.db.db.WithContext(ctx).Where("id = ?", id)
	if key > 0 {
		q = q.Where("client_key_id = ?", key)
	}
	if e := q.First(&row).Error; e != nil {
		return channel.ImageJob{}, channelError(e)
	}
	v, e := jobDomain(row)
	if e != nil {
		return v, e
	}
	var items []channelImageItemModel
	if e := r.db.db.WithContext(ctx).Where("job_id = ?", id).Order("position ASC").Find(&items).Error; e != nil {
		return v, e
	}
	for _, row := range items {
		item, e := itemDomain(row)
		if e != nil {
			return v, e
		}
		v.Items = append(v.Items, item)
	}
	return v, nil
}
func (r *ChannelRepository) ListImageJobs(ctx context.Context, key uint64, p account.Provider, offset, limit int) ([]channel.ImageJob, int64, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	q := r.db.db.WithContext(ctx).Model(&channelImageJobModel{})
	if key > 0 {
		q = q.Where("client_key_id = ?", key)
	}
	if p != "" {
		q = q.Where("provider = ?", p)
	}
	var total int64
	if e := q.Count(&total).Error; e != nil {
		return nil, 0, e
	}
	var rows []channelImageJobModel
	if e := q.Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&rows).Error; e != nil {
		return nil, 0, e
	}
	out := []channel.ImageJob{}
	for _, row := range rows {
		v, e := r.GetImageJob(ctx, row.ID, key)
		if e != nil {
			return nil, 0, e
		}
		out = append(out, v)
	}
	return out, total, nil
}
func (r *ChannelRepository) CancelImageJob(ctx context.Context, id string, key uint64) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var parent channelImageJobModel
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&parent, "id = ? AND client_key_id = ?", id, key).Error; e != nil {
			return channelError(e)
		}
		var n int64
		if e := tx.Model(&channelImageItemModel{}).Where("job_id = ? AND submitted = ?", id, true).Count(&n).Error; e != nil {
			return e
		}
		if n > 0 {
			return channel.ErrConflict
		}
		if parent.Status != "queued" && parent.Status != "running" {
			return channel.ErrConflict
		}
		if e := tx.Model(&channelImageItemModel{}).Where("job_id = ? AND status IN ?", id, []string{"queued", "running"}).Updates(map[string]any{"status": "canceled", "stage": "canceled", "claim_token": "", "lease_until": nil}).Error; e != nil {
			return e
		}
		return tx.Model(&parent).Update("status", "canceled").Error
	})
}
func (r *ChannelRepository) ClaimImageItem(ctx context.Context, provider account.Provider, now, until time.Time, token string) (channel.ImageJob, channel.ImageItem, error) {
	now, until = now.UTC(), until.UTC()
	var row channelImageItemModel
	e := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Model(&channelImageItemModel{}).Select("channel_image_items.*").Joins("JOIN channel_image_jobs ON channel_image_jobs.id = channel_image_items.job_id").Where("channel_image_items.status = ? OR (channel_image_items.status = ? AND channel_image_items.lease_until < ?)", "queued", "running", now).Order("channel_image_items.updated_at ASC, channel_image_items.id ASC")
		q = q.Where("channel_image_jobs.provider = ?", provider)
		if r.db.dialect == "postgres" {
			// Every item mutation locks the parent first, including claims. This
			// also serializes terminal aggregation for concurrent children.
			q = q.Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "channel_image_jobs"}, Options: "SKIP LOCKED"})
		}
		if e := q.First(&row).Error; e != nil {
			return e
		}
		updates := map[string]any{"status": "running", "claim_token": token, "generation": row.Generation + 1, "lease_until": until, "updated_at": now}
		if row.StartedAt == nil {
			updates["started_at"] = now
		}
		result := tx.Model(&channelImageItemModel{}).Where("id = ? AND generation = ? AND (status = ? OR (status = ? AND lease_until < ?))", row.ID, row.Generation, "queued", "running", now).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return channel.ErrConflict
		}
		row.Generation++
		row.ClaimToken = token
		row.LeaseUntil = &until
		row.Status = "running"
		if row.StartedAt == nil {
			row.StartedAt = &now
		}
		return tx.Model(&channelImageJobModel{}).Where("id = ? AND status = ?", row.JobID, "queued").Updates(map[string]any{"status": "running", "started_at": gorm.Expr("COALESCE(started_at, ?)", now)}).Error
	})
	if e != nil {
		return channel.ImageJob{}, channel.ImageItem{}, channelError(e)
	}
	job, e := r.GetImageJob(ctx, row.JobID, 0)
	if e != nil {
		return job, channel.ImageItem{}, e
	}
	item, e := itemDomain(row)
	return job, item, e
}
func imageFence(tx *gorm.DB, v channel.ImageItem) *gorm.DB {
	return tx.Model(&channelImageItemModel{}).Where("id = ? AND generation = ? AND claim_token = ? AND status = ? AND lease_until > ?", v.ID, v.Generation, v.ClaimToken, "running", time.Now().UTC())
}
func checkedImageUpdate(result *gorm.DB) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return channel.ErrConflict
	}
	return nil
}
func (r *ChannelRepository) AssignImageAccount(ctx context.Context, v channel.ImageItem, id uint64) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := lockImageParent(tx, v.JobID); e != nil {
			return e
		}
		if e := acquireChannelSlot(tx, id, v.ClaimToken, time.Now().UTC().Add(90*time.Second)); e != nil {
			return e
		}
		return checkedImageUpdate(imageFence(tx, v).Update("account_id", id))
	})
}
func (r *ChannelRepository) RenewImageItem(ctx context.Context, v channel.ImageItem, until time.Time) error {
	until = until.UTC()
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := lockImageParent(tx, v.JobID); e != nil {
			return e
		}
		if e := checkedImageUpdate(imageFence(tx, v).Update("lease_until", until)); e != nil {
			return e
		}
		var current channelImageItemModel
		if e := tx.First(&current, "id = ?", v.ID).Error; e != nil {
			return e
		}
		if current.AccountID == nil {
			return nil
		}
		return checkedImageUpdate(tx.Model(&channelExecutionSlotModel{}).Where("holder = ? AND lease_until > ?", v.ClaimToken, time.Now().UTC()).Update("lease_until", until))
	})
}
func (r *ChannelRepository) SaveImageCheckpoint(ctx context.Context, v channel.ImageItem, cp channel.Checkpoint, stage string) error {
	raw, e := json.Marshal(cp)
	if e != nil {
		return e
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var parent channelImageJobModel
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&parent, "id = ?", v.JobID).Error; e != nil {
			return e
		}
		if parent.Status == "canceled" {
			return channel.ErrConflict
		}
		return checkedImageUpdate(imageFence(tx, v).Updates(map[string]any{"submitted": cp.Submitted, "checkpoint_json": string(raw), "stage": stage, "updated_at": time.Now().UTC()}))
	})
}
func (r *ChannelRepository) CompleteImageItem(ctx context.Context, v channel.ImageItem, assets []string, status, message string, record *audit.Record) error {
	b, e := json.Marshal(assets)
	if e != nil {
		return e
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := lockImageParent(tx, v.JobID); e != nil {
			return e
		}
		if e := checkedImageUpdate(imageFence(tx, v).Updates(map[string]any{"status": status, "stage": status, "assets_json": string(b), "width": v.Width, "height": v.Height, "mime": v.MIME, "error": message, "lease_until": nil, "updated_at": time.Now().UTC()})); e != nil {
			return e
		}
		if e := tx.Delete(&channelExecutionSlotModel{}, "holder = ?", v.ClaimToken).Error; e != nil {
			return e
		}
		for _, id := range assets {
			if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&channelAssetRefModel{JobID: v.JobID, AssetID: id}).Error; e != nil {
				return e
			}
		}
		if record != nil {
			prepared, e := prepareAudits([]audit.Record{*record})
			if e != nil {
				return e
			}
			if e := createAuditAndBill(tx, &prepared[0].row, prepared[0].attempts); e != nil {
				return e
			}
		}
		var items []channelImageItemModel
		if e := tx.Where("job_id = ?", v.JobID).Find(&items).Error; e != nil {
			return e
		}
		success, active, uncertain, canceled := 0, 0, 0, 0
		for _, item := range items {
			switch item.Status {
			case "succeeded":
				success++
			case "queued", "running":
				active++
			case "uncertain":
				uncertain++
			case "canceled":
				canceled++
			}
		}
		parentStatus := "failed"
		switch {
		case active > 0:
			parentStatus = "running"
		case success == len(items):
			parentStatus = "succeeded"
		case success > 0:
			parentStatus = "partially_succeeded"
		case uncertain > 0:
			parentStatus = "uncertain"
		case canceled == len(items):
			parentStatus = "canceled"
		}
		return tx.Model(&channelImageJobModel{}).Where("id = ?", v.JobID).Update("status", parentStatus).Error
	})
}
func (r *ChannelRepository) ReserveChannelQuota(ctx context.Context, id uint64, event string, needs []channel.QuotaNeed) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var a accountModel
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&a, "id = ?", id).Error; e != nil {
			return e
		}
		var meta channelAccountModel
		if e := tx.First(&meta, "account_id = ?", id).Error; e != nil {
			return e
		}
		var quotas []channel.FeatureQuota
		if e := json.Unmarshal([]byte(meta.QuotasJSON), &quotas); e != nil {
			return e
		}
		for _, need := range needs {
			if need.Amount <= 0 {
				continue
			}
			var existing channelQuotaReservationModel
			e := tx.First(&existing, "event_id = ? AND feature = ?", event, need.Feature).Error
			if e == nil {
				if existing.AccountID != id || existing.Amount != need.Amount {
					return channel.ErrConflict
				}
				continue
			}
			if !errors.Is(e, gorm.ErrRecordNotFound) {
				return e
			}
			for _, q := range quotas {
				if q.Feature == need.Feature && q.Remaining != nil {
					var spent int64
					if e := tx.Model(&channelQuotaReservationModel{}).Select("COALESCE(SUM(amount),0)").Where("account_id = ? AND feature = ? AND state <> ? AND (finished_at IS NULL OR finished_at > ?)", id, need.Feature, "released", q.ObservedAt).Scan(&spent).Error; e != nil {
						return e
					}
					if *q.Remaining-spent < need.Amount {
						return channel.ErrQuotaExhausted
					}
				}
			}
			row := channelQuotaReservationModel{EventID: event, Feature: need.Feature, AccountID: id, Amount: need.Amount, State: "reserved", CreatedAt: time.Now().UTC()}
			if e := tx.Create(&row).Error; e != nil {
				return e
			}
		}
		return nil
	})
}
func (r *ChannelRepository) FinishChannelQuota(ctx context.Context, event string, unsent bool) error {
	// Only requests recorded before the send can consume upstream capacity. A
	// partially completed upload releases the unattempted portion, not the whole
	// operation. The unsent hint never overrides durable send evidence.
	return r.db.db.WithContext(ctx).Model(&channelQuotaReservationModel{}).Where("event_id = ? AND finished_at IS NULL", event).Updates(map[string]any{"state": gorm.Expr("CASE WHEN sent_amount = 0 THEN 'released' ELSE 'spent' END"), "amount": gorm.Expr("sent_amount"), "finished_at": time.Now().UTC()}).Error
}

func (r *ChannelRepository) MarkChannelQuotaSent(ctx context.Context, event, feature string) error {
	return checkedImageUpdate(r.db.db.WithContext(ctx).Model(&channelQuotaReservationModel{}).Where("event_id = ? AND feature = ? AND finished_at IS NULL AND sent_amount < amount", event, feature).Updates(map[string]any{"sent_amount": gorm.Expr("sent_amount + 1"), "state": "sent"}))
}

func (r *ChannelRepository) CleanupChannelData(ctx context.Context, now time.Time) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := tx.Where("expires_at < ?", now).Delete(&channelOAuthModel{}).Error; e != nil {
			return e
		}
		if e := tx.Where("lease_until < ?", now).Delete(&channelExecutionSlotModel{}).Error; e != nil {
			return e
		}
		cutoff := now.Add(-30 * 24 * time.Hour)
		if e := tx.Where("created_at < ? AND status NOT IN ?", cutoff, []string{"queued", "running"}).Delete(&channelImageJobModel{}).Error; e != nil {
			return e
		}
		return tx.Where("finished_at < ?", cutoff).Delete(&channelQuotaReservationModel{}).Error
	})
}

func (r *ChannelRepository) FindImageJob(ctx context.Context, key uint64, idempotency string) (channel.ImageJob, bool, error) {
	var row channelImageJobModel
	e := r.db.db.WithContext(ctx).First(&row, "client_key_id = ? AND idempotency_key = ?", key, idempotency).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return channel.ImageJob{}, false, nil
	}
	if e != nil {
		return channel.ImageJob{}, false, e
	}
	job, e := r.GetImageJob(ctx, row.ID, key)
	return job, true, e
}
