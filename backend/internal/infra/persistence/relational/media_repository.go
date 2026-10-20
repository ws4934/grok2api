package relational

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MediaJobRepository struct{ db *Database }

type MediaAssetRepository struct{ db *Database }

func NewMediaJobRepository(db *Database) *MediaJobRepository { return &MediaJobRepository{db: db} }

func NewMediaAssetRepository(db *Database) *MediaAssetRepository {
	return &MediaAssetRepository{db: db}
}

// mediaAssetPromptRunes bounds the stored prompt excerpt.
const mediaAssetPromptRunes = 1000

func (r *MediaAssetRepository) CreateMediaAsset(ctx context.Context, value media.Asset) error {
	origin := value.Origin
	// Origin is display metadata: clip it to the column sizes instead of
	// failing the save on PostgreSQL.
	row := mediaAssetModel{
		ID: value.ID, Kind: value.Kind, StorageKey: value.StorageKey, MIMEType: value.MIMEType,
		SizeBytes: value.SizeBytes, SHA256: value.SHA256, ExpiresAt: value.ExpiresAt, CreatedAt: value.CreatedAt,
		Provider: truncate(origin.Provider, 32), Model: truncate(origin.Model, 255), UpstreamModel: truncate(origin.UpstreamModel, 255),
		ClientKeyID: origin.ClientKeyID, ClientKeyName: truncate(origin.ClientKeyName, 160),
		AccountID: origin.AccountID, AccountName: truncate(origin.AccountName, 160),
		Operation: truncate(origin.Operation, 32), JobID: truncate(origin.JobID, 64), RequestID: truncate(origin.RequestID, 64),
		Prompt: truncate(origin.Prompt, mediaAssetPromptRunes),
	}
	return r.db.db.WithContext(ctx).Create(&row).Error
}

func (r *MediaAssetRepository) GetMediaAsset(ctx context.Context, id string) (media.Asset, error) {
	var row mediaAssetModel
	if err := r.db.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		return media.Asset{}, mapError(err)
	}
	return mediaAssetToDomain(row), nil
}

// ListMediaAssets 通过字段投影返回符合筛选条件的稳定分页结果。
// 仅列出持久资产；未指定类型时只列图片，保持图库默认行为。
func (r *MediaAssetRepository) ListMediaAssets(ctx context.Context, input repository.MediaAssetListQuery) ([]media.Asset, int64, error) {
	filter := input.Filter
	kind := filter.Kind
	if kind == "" {
		kind = "image"
	}
	query := r.db.db.WithContext(ctx).Model(&mediaAssetModel{}).Where("kind = ? AND expires_at IS NULL", kind)
	if filter.Provider != "" {
		query = query.Where("provider = ?", filter.Provider)
	}
	if filter.Model != "" {
		query = query.Where("model = ?", filter.Model)
	}
	if filter.Operation != "" {
		query = query.Where("operation = ?", filter.Operation)
	}
	if filter.ClientKeyID != 0 {
		query = query.Where("client_key_id = ?", filter.ClientKeyID)
	}
	if filter.AccountID != 0 {
		query = query.Where("account_id = ?", filter.AccountID)
	}
	if filter.JobID != "" {
		query = query.Where("job_id = ?", filter.JobID)
	}
	if !filter.CreatedFrom.IsZero() {
		query = query.Where("created_at >= ?", filter.CreatedFrom.UTC())
	}
	if !filter.CreatedTo.IsZero() {
		query = query.Where("created_at < ?", filter.CreatedTo.UTC())
	}
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(id) LIKE ? OR LOWER(kind) LIKE ? OR LOWER(mime_type) LIKE ? OR LOWER(sha256) LIKE ? OR LOWER(prompt) LIKE ?", pattern, pattern, pattern, pattern, pattern)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []mediaAssetModel
	if err := query.Omit("storage_key", "expires_at").Order("created_at DESC, id DESC").Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	values := make([]media.Asset, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaAssetToDomain(row))
	}
	return values, total, nil
}

func mediaAssetToDomain(row mediaAssetModel) media.Asset {
	return media.Asset{
		ID: row.ID, Kind: row.Kind, StorageKey: row.StorageKey, MIMEType: row.MIMEType,
		SizeBytes: row.SizeBytes, SHA256: row.SHA256, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
		Origin: media.Origin{
			Provider: row.Provider, Model: row.Model, UpstreamModel: row.UpstreamModel,
			ClientKeyID: row.ClientKeyID, ClientKeyName: row.ClientKeyName, AccountID: row.AccountID, AccountName: row.AccountName,
			Operation: row.Operation, JobID: row.JobID, RequestID: row.RequestID, Prompt: row.Prompt,
		},
	}
}

// SummarizeMediaAssets 通过单次聚合查询返回图片数量和全部媒体（含视频）存储占用。
func (r *MediaAssetRepository) SummarizeMediaAssets(ctx context.Context) (repository.MediaAssetStats, error) {
	var stats repository.MediaAssetStats
	err := r.db.db.WithContext(ctx).Model(&mediaAssetModel{}).
		Select("COALESCE(SUM(CASE WHEN kind = 'image' AND expires_at IS NULL THEN 1 ELSE 0 END), 0) AS total_images, COALESCE(SUM(size_bytes), 0) AS total_bytes").
		Scan(&stats).Error
	return stats, err
}

func (r *MediaAssetRepository) TotalMediaAssetBytes(ctx context.Context) (int64, error) {
	var total int64
	err := r.db.db.WithContext(ctx).Model(&mediaAssetModel{}).Select("COALESCE(SUM(size_bytes), 0)").Scan(&total).Error
	return total, err
}

func (r *MediaAssetRepository) ListOldestMediaAssets(ctx context.Context, offset, limit int) ([]media.Asset, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	var rows []mediaAssetModel
	if err := r.db.db.WithContext(ctx).Order("created_at ASC, id ASC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]media.Asset, 0, len(rows))
	for _, row := range rows {
		values = append(values, media.Asset{
			ID: row.ID, Kind: row.Kind, StorageKey: row.StorageKey, MIMEType: row.MIMEType,
			SizeBytes: row.SizeBytes, SHA256: row.SHA256, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
		})
	}
	return values, nil
}

func (r *MediaAssetRepository) ListExpiredMediaAssets(ctx context.Context, before time.Time, offset, limit int) ([]media.Asset, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	var rows []mediaAssetModel
	if err := r.db.db.WithContext(ctx).
		Where("expires_at IS NOT NULL AND expires_at <= ?", before).
		Order("expires_at ASC, id ASC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]media.Asset, 0, len(rows))
	for _, row := range rows {
		values = append(values, media.Asset{
			ID: row.ID, Kind: row.Kind, StorageKey: row.StorageKey, MIMEType: row.MIMEType,
			SizeBytes: row.SizeBytes, SHA256: row.SHA256, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
		})
	}
	return values, nil
}

func (r *MediaAssetRepository) ExpireMediaInputIfUnreferenced(ctx context.Context, id string, expiresAt time.Time) (bool, error) {
	referencePattern := "%" + media.InputReference(id) + "%"
	activeReference := r.db.db.Model(&mediaJobModel{}).Select("1").
		Where("kind = ? AND status IN ? AND input_json LIKE ?", media.KindVideo, []string{string(media.StatusQueued), string(media.StatusInProgress)}, referencePattern)
	result := r.db.db.WithContext(ctx).Model(&mediaAssetModel{}).
		Where("id = ? AND kind IN ? AND expires_at IS NOT NULL", id, []string{"image", "video"}).
		Where("NOT EXISTS (?)", activeReference).
		Update("expires_at", expiresAt)
	return result.RowsAffected > 0, result.Error
}

func (r *MediaAssetRepository) DeleteMediaAsset(ctx context.Context, id string) error {
	result := r.db.db.WithContext(ctx).Where("id = ?", id).Delete(&mediaAssetModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// ListProtectedMediaAssetIDs 返回不可清理的资产：活动任务的结果与未消费票据。
func (r *MediaAssetRepository) ListProtectedMediaAssetIDs(ctx context.Context) (map[string]struct{}, error) {
	protected := make(map[string]struct{})
	var jobAssetIDs []string
	if err := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("result_asset_id <> '' AND status IN ?", []string{string(media.StatusQueued), string(media.StatusInProgress)}).
		Pluck("result_asset_id", &jobAssetIDs).Error; err != nil {
		return nil, err
	}
	for _, id := range jobAssetIDs {
		if id != "" {
			protected[id] = struct{}{}
		}
	}
	// Images already delivered by a running image job stay until it ends.
	var imageResults []string
	if err := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("kind = ? AND result_asset_ids <> '[]' AND status IN ?", media.KindImage, []string{string(media.StatusQueued), string(media.StatusInProgress)}).
		Pluck("result_asset_ids", &imageResults).Error; err != nil {
		return nil, err
	}
	for _, raw := range imageResults {
		var ids []string
		_ = json.Unmarshal([]byte(raw), &ids)
		for _, id := range ids {
			protected[id] = struct{}{}
		}
	}
	var ticketAssetIDs []string
	now := time.Now().UTC()
	if err := r.db.db.WithContext(ctx).Model(&mediaUploadTicketModel{}).
		Where("consumed_at IS NULL AND expires_at > ?", now).
		Pluck("asset_id", &ticketAssetIDs).Error; err != nil {
		return nil, err
	}
	for _, id := range ticketAssetIDs {
		if id != "" {
			protected[id] = struct{}{}
		}
	}
	return protected, nil
}

type MediaUploadTicketRepository struct{ db *Database }

func NewMediaUploadTicketRepository(db *Database) *MediaUploadTicketRepository {
	return &MediaUploadTicketRepository{db: db}
}

func (r *MediaUploadTicketRepository) CreateUploadTicket(ctx context.Context, ticket repository.MediaUploadTicket) error {
	row := mediaUploadTicketModel{
		TokenHash: ticket.TokenHash, AssetID: ticket.AssetID, JobID: ticket.JobID,
		MaxBytes: ticket.MaxBytes, AllowedMIME: ticket.AllowedMIME,
		ExpiresAt: ticket.ExpiresAt, ConsumedAt: ticket.ConsumedAt, CreatedAt: ticket.CreatedAt,
	}
	return r.db.db.WithContext(ctx).Create(&row).Error
}

func (r *MediaUploadTicketRepository) GetUploadTicketByHash(ctx context.Context, tokenHash string) (repository.MediaUploadTicket, error) {
	var row mediaUploadTicketModel
	if err := r.db.db.WithContext(ctx).Where("token_hash = ?", tokenHash).First(&row).Error; err != nil {
		return repository.MediaUploadTicket{}, mapError(err)
	}
	return ticketToDomain(row), nil
}

func (r *MediaUploadTicketRepository) ConsumeUploadTicket(ctx context.Context, tokenHash string, now time.Time) (repository.MediaUploadTicket, bool, error) {
	var ticket repository.MediaUploadTicket
	consumed := false
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&mediaUploadTicketModel{}).
			Where("token_hash = ? AND consumed_at IS NULL AND expires_at > ?", tokenHash, now).
			Update("consumed_at", now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		var row mediaUploadTicketModel
		if err := tx.Where("token_hash = ?", tokenHash).First(&row).Error; err != nil {
			return err
		}
		ticket = ticketToDomain(row)
		consumed = true
		return nil
	})
	if err != nil {
		return repository.MediaUploadTicket{}, false, err
	}
	return ticket, consumed, nil
}

// ReleaseUploadTicket 撤销一次尚未落资产的消费，使同一 token 可再次 PUT。
func (r *MediaUploadTicketRepository) ReleaseUploadTicket(ctx context.Context, tokenHash string) (bool, error) {
	// 使用 map 写入 NULL：GORM Update(column, nil) 可能被忽略。
	result := r.db.db.WithContext(ctx).Model(&mediaUploadTicketModel{}).
		Where("token_hash = ? AND consumed_at IS NOT NULL", tokenHash).
		Updates(map[string]any{"consumed_at": nil})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// DeleteUploadTicketByHash 按 token_hash 精确删除票据；目标不存在时幂等返回 nil。
func (r *MediaUploadTicketRepository) DeleteUploadTicketByHash(ctx context.Context, tokenHash string) error {
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return nil
	}
	return r.db.db.WithContext(ctx).Where("token_hash = ?", tokenHash).Delete(&mediaUploadTicketModel{}).Error
}

// DeleteUploadTicketsByJobID 删除任务关联的上传票据，终止后续延迟上传。
func (r *MediaUploadTicketRepository) DeleteUploadTicketsByJobID(ctx context.Context, jobID string) error {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil
	}
	return r.db.db.WithContext(ctx).Where("job_id = ?", jobID).Delete(&mediaUploadTicketModel{}).Error
}

// DeleteExpiredUploadTickets 删除已过期票据行（已消费或未消费均可）；未过期票据与媒体资产不受影响。
func (r *MediaUploadTicketRepository) DeleteExpiredUploadTickets(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var hashes []string
	if err := r.db.db.WithContext(ctx).Model(&mediaUploadTicketModel{}).
		Where("expires_at < ?", before).
		Order("expires_at ASC").Limit(limit).Pluck("token_hash", &hashes).Error; err != nil {
		return 0, err
	}
	if len(hashes) == 0 {
		return 0, nil
	}
	result := r.db.db.WithContext(ctx).Where("token_hash IN ?", hashes).Delete(&mediaUploadTicketModel{})
	return result.RowsAffected, result.Error
}

func (r *MediaUploadTicketRepository) BindJobResultAsset(ctx context.Context, jobID, assetID string) error {
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("id = ?", jobID).
		Updates(map[string]any{"result_asset_id": assetID, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func ticketToDomain(row mediaUploadTicketModel) repository.MediaUploadTicket {
	return repository.MediaUploadTicket{
		TokenHash: row.TokenHash, AssetID: row.AssetID, JobID: row.JobID,
		MaxBytes: row.MaxBytes, AllowedMIME: row.AllowedMIME,
		ExpiresAt: row.ExpiresAt, ConsumedAt: row.ConsumedAt, CreatedAt: row.CreatedAt,
	}
}

// CreateMediaJob inserts a job. ErrConflict means another job already holds
// its idempotency key for the client key.
func (r *MediaJobRepository) CreateMediaJob(ctx context.Context, value media.Job) error {
	providerValue, scope := account.Provider(value.Provider), account.Provider(value.EgressScope)
	if value.Kind == media.KindImage {
		// Image providers are admitted by the gateway's registry check.
		if !providerValue.IsValid() || (value.EgressScope != "" && scope != providerValue) {
			return errors.New("unsupported image provider or primary egress scope")
		}
	} else if (value.Provider != "" && !providerValue.IsValid()) || (value.EgressScope != "" && !scope.IsLegacy() && scope != providerValue) {
		// Video providers are admitted by the gateway's registry check too; a
		// Grok job may run on another Grok provider's egress scope.
		return errors.New("unsupported video provider or primary egress scope")
	}
	result := r.db.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(mediaJobFromDomain(value))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrConflict
	}
	return nil
}

func (r *MediaJobRepository) GetMediaJob(ctx context.Context, id string, clientKeyID uint64) (media.Job, error) {
	var row mediaJobModel
	// Public polling and content download do not consume the potentially large,
	// immutable reference payload. Keep it off this hot path.
	if err := r.db.db.WithContext(ctx).Omit("input_json").Where("id = ? AND client_key_id = ?", id, clientKeyID).First(&row).Error; err != nil {
		return media.Job{}, mapError(err)
	}
	return mediaJobToDomain(row), nil
}

func (r *MediaJobRepository) GetMediaJobByIdempotencyKey(ctx context.Context, clientKeyID uint64, key string) (media.Job, error) {
	if key == "" {
		return media.Job{}, repository.ErrNotFound
	}
	var row mediaJobModel
	if err := r.db.db.WithContext(ctx).Omit("input_json").Where("client_key_id = ? AND idempotency_key = ? AND idempotency_key <> ''", clientKeyID, key).First(&row).Error; err != nil {
		return media.Job{}, mapError(err)
	}
	return mediaJobToDomain(row), nil
}

func (r *MediaJobRepository) GetMediaJobInput(ctx context.Context, id string) (string, error) {
	var row mediaJobModel
	if err := r.db.db.WithContext(ctx).Select("input_json").Where("id = ?", id).First(&row).Error; err != nil {
		return "", mapError(err)
	}
	return row.InputJSON, nil
}

// GetMediaJobsByIDs 返回管理端明确选择的完整媒体任务。
func (r *MediaJobRepository) GetMediaJobsByIDs(ctx context.Context, ids []string) ([]media.Job, error) {
	if len(ids) == 0 {
		return []media.Job{}, nil
	}
	var rows []mediaJobModel
	if err := r.db.db.WithContext(ctx).Omit("input_json").Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]media.Job, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaJobToDomain(row))
	}
	return values, nil
}

// UpdateMediaJob writes the mutable job state. A claimed job is fenced by its
// claim token and generation, so a worker whose claim was recovered or
// canceled can no longer write (ErrNotFound).
func (r *MediaJobRepository) UpdateMediaJob(ctx context.Context, value media.Job) error {
	updates := mediaJobFromDomain(value)
	query := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).Where("id = ?", value.ID)
	if value.ClaimToken != "" {
		query = query.Where("claim_token = ? AND generation = ?", value.ClaimToken, value.Generation)
	}
	// InputJSON and InputImageCount are immutable creation metadata. Progress and
	// terminal updates must not resend a multi-megabyte Base64 payload.
	result := query.Select("request_id", "client_key_name", "account_id", "account_name", "egress_node_id", "egress_node_name", "egress_scope", "egress_mode", "provider", "model", "model_route_id", "upstream_model", "prompt", "seconds", "size", "quality", "status", "progress", "upstream_url", "result_asset_id", "result_asset_ids", "checkpoint_json", "submitted", "content_type", "error_code", "error_message", "lease_until", "claim_token", "updated_at", "completed_at", "usage_recorded_at", "price_id", "pricing_model", "price_ticks").Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *MediaJobRepository) RenewMediaJobLease(ctx context.Context, value media.Job, leaseUntil time.Time) error {
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("id = ? AND claim_token = ? AND generation = ? AND status = ?", value.ID, value.ClaimToken, value.Generation, media.StatusInProgress).
		Update("lease_until", leaseUntil)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func (r *MediaJobRepository) CancelMediaJob(ctx context.Context, id string, clientKeyID uint64, now time.Time) (media.Job, error) {
	scoped := func() *gorm.DB {
		query := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).Where("id = ?", id)
		if clientKeyID > 0 {
			query = query.Where("client_key_id = ?", clientKeyID)
		}
		return query
	}
	// Clearing the claim fences out the worker; an image job is cancellable
	// until its item in flight is submitted.
	result := scoped().Where("(status = ? OR (status = ? AND kind = ? AND submitted = ?))", media.StatusQueued, media.StatusInProgress, media.KindImage, false).
		Updates(map[string]any{"status": media.StatusCanceled, "claim_token": "", "lease_until": nil, "completed_at": now, "updated_at": now, "error_code": "canceled", "error_message": ""})
	if result.Error != nil {
		return media.Job{}, result.Error
	}
	var row mediaJobModel
	if err := scoped().Omit("input_json").First(&row).Error; err != nil {
		return media.Job{}, mapError(err)
	}
	job := mediaJobToDomain(row)
	if result.RowsAffected == 0 && !job.Status.Terminal() {
		return job, repository.ErrConflict
	}
	return job, nil
}

// ResumeMediaJob compares and sets failed → in_progress with no lease, so the
// recovery claim (new generation and token) runs it once; the checkpoint and
// delivered images stay, and usage is recorded again when it ends.
func (r *MediaJobRepository) ResumeMediaJob(ctx context.Context, id string, generation, item int, now time.Time) (media.Job, error) {
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("id = ? AND kind = ? AND status = ? AND submitted = ? AND generation = ?", id, media.KindImage, media.StatusFailed, true, generation).
		Updates(map[string]any{
			"status": media.StatusInProgress, "error_code": "", "error_message": "", "completed_at": nil, "usage_recorded_at": nil,
			"lease_until": nil, "claim_token": "", "resume_item": item, "resume_generation": generation, "updated_at": now,
		})
	if result.Error != nil {
		return media.Job{}, result.Error
	}
	var row mediaJobModel
	if err := r.db.db.WithContext(ctx).Omit("input_json").Where("id = ?", id).First(&row).Error; err != nil {
		return media.Job{}, mapError(err)
	}
	if result.RowsAffected == 0 {
		return mediaJobToDomain(row), repository.ErrConflict
	}
	return mediaJobToDomain(row), nil
}

// DeleteMediaJob 仅删除终态任务，避免管理端与媒体 Worker 并发修改同一任务。
func (r *MediaJobRepository) DeleteMediaJob(ctx context.Context, id string) error {
	result := r.db.db.WithContext(ctx).
		Where("id = ? AND status IN ?", id, []media.Status{media.StatusCompleted, media.StatusFailed, media.StatusCanceled, media.StatusUncertain}).
		Delete(&mediaJobModel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// ListMediaJobs 通过固定搜索字段和排序白名单返回稳定分页结果。
func (r *MediaJobRepository) ListMediaJobs(ctx context.Context, input repository.MediaJobListQuery) ([]media.Job, int64, error) {
	query := r.db.db.WithContext(ctx).Model(&mediaJobModel{})
	if input.Filter.Kind != "" {
		query = query.Where("kind = ?", input.Filter.Kind)
	}
	if input.Filter.Status != "" {
		query = query.Where("status = ?", input.Filter.Status)
	}
	if input.Filter.Provider != "" {
		query = query.Where("provider = ?", input.Filter.Provider)
	}
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(id) LIKE ? OR LOWER(prompt) LIKE ? OR LOWER(model) LIKE ? OR LOWER(account_name) LIKE ? OR LOWER(client_key_name) LIKE ?", pattern, pattern, pattern, pattern, pattern)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []mediaJobModel
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"prompt":      {expression: "LOWER(prompt)"},
		"model":       {expression: "LOWER(model)"},
		"status":      {expression: "status"},
		"progress":    {expression: "progress", defaultDirection: repository.SortDescending},
		"spec":        {expression: "LOWER(size) || ' ' || LOWER(quality)"},
		"account":     {expression: "LOWER(account_name)"},
		"createdAt":   {expression: "created_at", defaultDirection: repository.SortDescending},
		"completedAt": {expression: "completed_at", nullsLast: true, defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "created_at", defaultDirection: repository.SortDescending}, "id")
	// account_id and checkpoint_json tell whether a failed image job can resume.
	if err := query.Select(
		"id", "kind", "provider", "client_key_name", "account_id", "account_name", "model", "operation", "prompt", "seconds", "size", "quality",
		"status", "progress", "result_asset_id", "result_asset_ids", "item_count", "checkpoint_json", "submitted", "error_code", "error_message", "created_at", "completed_at",
	).Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	values := make([]media.Job, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaJobToDomain(row))
	}
	return values, total, nil
}

// SummarizeMediaJobs 通过单次条件聚合查询统计任务状态。
func (r *MediaJobRepository) SummarizeMediaJobs(ctx context.Context, kind string) (repository.MediaJobStats, error) {
	var stats repository.MediaJobStats
	query := r.db.db.WithContext(ctx).Model(&mediaJobModel{})
	if kind != "" {
		query = query.Where("kind = ?", kind)
	}
	err := query.Select(`
		COUNT(*) AS total_jobs,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS completed,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS failed,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS in_progress,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS queued,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS canceled,
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS uncertain`,
		media.StatusCompleted, media.StatusFailed, media.StatusInProgress, media.StatusQueued, media.StatusCanceled, media.StatusUncertain,
	).Scan(&stats).Error
	return stats, err
}

// auditedStatuses end with an audit record. A canceled image job is audited
// too, for the images it delivered before the cancel; a canceled video never
// reached a billable outcome.
var auditedStatuses = []media.Status{media.StatusCompleted, media.StatusFailed, media.StatusUncertain}

func usageRecordable(query *gorm.DB) *gorm.DB {
	return query.Where("(status IN ? OR (status = ? AND kind = ?))", auditedStatuses, media.StatusCanceled, media.KindImage)
}

// ListUnrecordedTerminalMediaJobs 返回尚未完成审计写入的终态任务。
func (r *MediaJobRepository) ListUnrecordedTerminalMediaJobs(ctx context.Context, limit int) ([]media.Job, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var rows []mediaJobModel
	if err := usageRecordable(r.db.db.WithContext(ctx).Omit("input_json")).Where("usage_recorded_at IS NULL").Order("completed_at ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]media.Job, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaJobToDomain(row))
	}
	return values, nil
}

func (r *MediaJobRepository) MarkMediaJobUsageRecorded(ctx context.Context, id string, recordedAt time.Time) error {
	// Once the durable audit owns the input-image count, terminal jobs no longer
	// need raw Base64 references. Compact them in the same idempotent update;
	// uncertain jobs keep them for an administrator retry.
	result := usageRecordable(r.db.db.WithContext(ctx).Model(&mediaJobModel{})).
		Where("id = ? AND usage_recorded_at IS NULL", id).
		Updates(map[string]any{"usage_recorded_at": recordedAt, "input_json": gorm.Expr("CASE WHEN status = ? THEN input_json ELSE ? END", media.StatusUncertain, "{}")})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := usageRecordable(r.db.db.WithContext(ctx).Model(&mediaJobModel{})).Where("id = ? AND usage_recorded_at IS NOT NULL", id).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return repository.ErrNotFound
		}
	}
	return nil
}

func (r *MediaJobRepository) ListRecoverableMediaJobs(ctx context.Context, limit int) ([]media.Job, error) {
	var rows []mediaJobModel
	now := time.Now().UTC()
	// Recovery only enqueues IDs. The bounded worker reads one complete payload
	// after it atomically claims the job.
	if err := r.db.db.WithContext(ctx).Select("id").Where("status = ? OR (status = ? AND (lease_until IS NULL OR lease_until <= ?))", media.StatusQueued, media.StatusInProgress, now).Order("created_at ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]media.Job, 0, len(rows))
	for _, row := range rows {
		values = append(values, mediaJobToDomain(row))
	}
	return values, nil
}

// TryClaimMediaJob 原子认领新任务或租约已过期的任务，避免多实例重复执行。
func (r *MediaJobRepository) TryClaimMediaJob(ctx context.Context, id string, now, leaseUntil time.Time, claimToken string) (media.Job, bool, error) {
	if claimToken == "" {
		return media.Job{}, false, repository.ErrConflict
	}
	result := r.db.db.WithContext(ctx).Model(&mediaJobModel{}).
		Where("id = ? AND (status = ? OR (status = ? AND (lease_until IS NULL OR lease_until <= ?)))", id, media.StatusQueued, media.StatusInProgress, now).
		Updates(map[string]any{"status": media.StatusInProgress, "lease_until": leaseUntil, "claim_token": claimToken, "generation": gorm.Expr("generation + 1"), "updated_at": now})
	if result.Error != nil {
		return media.Job{}, false, result.Error
	}
	if result.RowsAffected == 0 {
		return media.Job{}, false, nil
	}
	var row mediaJobModel
	if err := r.db.db.WithContext(ctx).Where("id = ? AND claim_token = ?", id, claimToken).First(&row).Error; err != nil {
		return media.Job{}, false, mapError(err)
	}
	return mediaJobToDomain(row), true, nil
}

func mediaJobFromDomain(value media.Job) *mediaJobModel {
	operation := value.Operation
	if operation == "" {
		operation = media.VideoOperationGenerate
	}
	kind := value.Kind
	if kind == "" {
		kind = media.KindVideo
	}
	itemCount := max(1, value.ItemCount)
	checkpoint := value.Checkpoint
	if checkpoint == "" {
		checkpoint = "{}"
	}
	resultAssetIDs, _ := json.Marshal(append([]string{}, value.ResultAssetIDs...))
	params, _ := json.Marshal(value.Params)
	row := &mediaJobModel{
		ID: value.ID, Kind: string(kind), RequestID: value.RequestID, ClientKeyID: value.ClientKeyID, ClientKeyName: value.ClientKeyName, ClientIP: value.ClientIP,
		AccountID: mediaJobAccountID(value.AccountID), AccountName: value.AccountName,
		EgressNodeID: value.EgressNodeID, EgressNodeName: value.EgressNodeName, EgressScope: value.EgressScope, EgressMode: value.EgressMode,
		Provider: value.Provider,
		Model:    value.Model, ModelRouteID: value.ModelRouteID, UpstreamModel: value.UpstreamModel, Operation: string(operation),
		Prompt: value.Prompt, Seconds: value.Seconds, Size: value.Size, Quality: value.Quality,
		Status: string(value.Status), Progress: value.Progress, InputJSON: value.InputJSON, InputImageCount: value.InputImageCount, UpstreamURL: value.UpstreamURL,
		ResultAssetID: value.ResultAssetID, ResultAssetIDs: string(resultAssetIDs), ItemCount: itemCount, CheckpointJSON: checkpoint, Submitted: value.Submitted,
		ParamsJSON: string(params), IdempotencyKey: value.IdempotencyKey, PayloadHash: value.PayloadHash, Generation: value.Generation,
		ResumeItem: value.ResumeItem, ResumeGeneration: value.ResumeGeneration,
		ContentType: value.ContentType, ErrorCode: value.ErrorCode,
		// Upstream error text may carry invalid UTF-8 or NUL, which PostgreSQL
		// rejects: the terminal write would fail and leave the job running.
		ErrorMessage: strings.ToValidUTF8(strings.ReplaceAll(value.ErrorMessage, "\x00", ""), "�"),
		LeaseUntil:   value.LeaseUntil, ClaimToken: value.ClaimToken, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		CompletedAt: value.CompletedAt, UsageRecordedAt: value.UsageRecordedAt,
	}
	if value.Price != nil {
		ticks := value.Price.CostInUSDTicks
		row.PriceID, row.PricingModel, row.PriceTicks = value.Price.PriceID, value.Price.Model, &ticks
	}
	return row
}

func mediaJobToDomain(row mediaJobModel) media.Job {
	var accountID uint64
	if row.AccountID != nil {
		accountID = *row.AccountID
	}
	operation := media.VideoOperation(row.Operation)
	if operation == "" {
		operation = media.VideoOperationGenerate
	}
	kind := media.Kind(row.Kind)
	if kind == "" {
		kind = media.KindVideo
	}
	var resultAssetIDs []string
	_ = json.Unmarshal([]byte(row.ResultAssetIDs), &resultAssetIDs)
	var params media.ImageParams
	_ = json.Unmarshal([]byte(row.ParamsJSON), &params)
	job := media.Job{
		ID: row.ID, Kind: kind, RequestID: row.RequestID, ClientKeyID: row.ClientKeyID, ClientKeyName: row.ClientKeyName, ClientIP: row.ClientIP,
		AccountID: accountID, AccountName: row.AccountName,
		EgressNodeID: row.EgressNodeID, EgressNodeName: row.EgressNodeName, EgressScope: row.EgressScope, EgressMode: row.EgressMode,
		Provider: row.Provider,
		Model:    row.Model, ModelRouteID: row.ModelRouteID, UpstreamModel: row.UpstreamModel, Operation: operation,
		Prompt: row.Prompt, Seconds: row.Seconds, Size: row.Size, Quality: row.Quality,
		Status: media.Status(row.Status), Progress: row.Progress, InputJSON: row.InputJSON, InputImageCount: row.InputImageCount, UpstreamURL: row.UpstreamURL,
		ResultAssetID: row.ResultAssetID, ResultAssetIDs: resultAssetIDs, ItemCount: max(1, row.ItemCount), Checkpoint: row.CheckpointJSON, Submitted: row.Submitted,
		Params: params, IdempotencyKey: row.IdempotencyKey, PayloadHash: row.PayloadHash, Generation: row.Generation,
		ResumeItem: row.ResumeItem, ResumeGeneration: row.ResumeGeneration,
		ContentType: row.ContentType, ErrorCode: row.ErrorCode, ErrorMessage: row.ErrorMessage,
		LeaseUntil: row.LeaseUntil, ClaimToken: row.ClaimToken, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		CompletedAt: row.CompletedAt, UsageRecordedAt: row.UsageRecordedAt,
	}
	if row.PriceTicks != nil {
		job.Price = &audit.PricingResult{Model: row.PricingModel, CostInUSDTicks: *row.PriceTicks, PriceID: row.PriceID}
	}
	return job
}

func mediaJobAccountID(value uint64) *uint64 {
	if value == 0 {
		return nil
	}
	return &value
}
