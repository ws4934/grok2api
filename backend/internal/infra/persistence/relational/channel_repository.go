package relational

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/channel"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ChannelRepository struct{ db *Database }

func NewChannelRepository(db *Database) *ChannelRepository { return &ChannelRepository{db: db} }

var _ channel.Repository = (*ChannelRepository)(nil)

func channelError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return channel.ErrNotFound
	}
	return err
}
func (r *ChannelRepository) Settings(ctx context.Context, p account.Provider) (channel.Settings, error) {
	var row channelSettingsModel
	err := r.db.db.WithContext(ctx).First(&row, "provider = ?", p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return channel.DefaultSettings(), nil
	}
	if err != nil {
		return channel.Settings{}, err
	}
	var v channel.Settings
	err = json.Unmarshal([]byte(row.ValueJSON), &v)
	return v, err
}
func (r *ChannelRepository) SaveSettings(ctx context.Context, p account.Provider, v channel.Settings) error {
	if err := v.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	row := channelSettingsModel{Provider: string(p), Version: 1, ValueJSON: string(b), UpdatedAt: time.Now().UTC()}
	return r.db.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "provider"}}, DoUpdates: clause.AssignmentColumns([]string{"version", "value_json", "updated_at"})}).Create(&row).Error
}
func channelAccountProjection(a accountModel, x channelAccountModel) (channel.Account, error) {
	v := channel.Account{ID: a.ID, Provider: account.Provider(a.Provider), Name: a.Name, Enabled: a.Enabled, MaxConcurrent: a.MaxConcurrent, Identity: channel.Identity{UserID: a.UserID, WorkspaceID: a.TeamID, Email: a.Email, Plan: x.Plan}, Status: x.Status, Failure: x.Failure, FailureCount: x.FailureCount, NextMaintenance: x.NextMaintenance, CredentialVersion: x.CredentialVersion, Refreshable: x.Refreshable, ReplaceDuplicate: x.ReplaceDuplicate, SyncedAt: x.SyncedAt, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
	if a.Credential != nil {
		v.ExpiresAt = a.Credential.ExpiresAt
	}
	v.EgressNodeID = a.EgressNodeID
	if err := json.Unmarshal([]byte(x.ModelsJSON), &v.Models); err != nil {
		return v, err
	}
	err := json.Unmarshal([]byte(x.QuotasJSON), &v.Quotas)
	return v, err
}
func (r *ChannelRepository) ListAccounts(ctx context.Context, p account.Provider, offset, limit int) ([]channel.Account, int64, error) {
	if offset < 0 {
		offset = 0
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	db := r.db.db.WithContext(ctx)
	q := db.Model(&accountModel{}).Where("provider = ?", p)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []accountModel
	if err := q.Order("id DESC").Offset(offset).Limit(limit).Preload("Credential", func(q *gorm.DB) *gorm.DB { return q.Select("account_id", "expires_at") }).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]channel.Account, 0, len(rows))
	ids := make([]uint64, 0, len(rows))
	for _, a := range rows {
		ids = append(ids, a.ID)
	}
	var extensions []channelAccountModel
	if len(ids) > 0 {
		if err := db.Where("account_id IN ?", ids).Find(&extensions).Error; err != nil {
			return nil, 0, err
		}
	}
	byID := make(map[uint64]channelAccountModel, len(extensions))
	for _, x := range extensions {
		byID[x.AccountID] = x
	}
	for _, a := range rows {
		x, ok := byID[a.ID]
		if !ok {
			continue
		}
		v, err := channelAccountProjection(a, x)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, nil
}
func (r *ChannelRepository) GetAccount(ctx context.Context, p account.Provider, id uint64) (channel.Account, error) {
	var row accountModel
	db := r.db.db.WithContext(ctx)
	if err := db.Preload("Credential", func(q *gorm.DB) *gorm.DB { return q.Select("account_id", "expires_at") }).First(&row, "id = ? AND provider = ?", id, p).Error; err != nil {
		return channel.Account{}, channelError(err)
	}
	var x channelAccountModel
	if err := db.First(&x, "account_id = ? AND provider = ?", id, p).Error; err != nil {
		return channel.Account{}, channelError(err)
	}
	return channelAccountProjection(row, x)
}
func (r *ChannelRepository) GetSecret(ctx context.Context, p account.Provider, id uint64) (channel.AccountSecret, error) {
	// Serialize with SaveAccount: READ COMMITTED alone does not make separate
	// account, credential and metadata queries a consistent versioned snapshot.
	var out channel.AccountSecret
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var a accountModel
		var x channelAccountModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Credential").First(&a, "id = ? AND provider = ?", id, p).Error; err != nil {
			return err
		}
		if err := tx.First(&x, "account_id = ? AND provider = ?", id, p).Error; err != nil {
			return err
		}
		v, err := channelAccountProjection(a, x)
		if err != nil {
			return err
		}
		if a.Credential == nil {
			return channel.ErrNotFound
		}
		out = channel.AccountSecret{Account: v, Fingerprint: x.Fingerprint, EncryptedCredentials: a.Credential.EncryptedPrimary}
		return nil
	})
	return out, channelError(err)
}
func channelIdentity(v channel.Account) string {
	key := string(v.Provider) + "\x00" + v.Identity.UserID + "\x00" + v.Identity.WorkspaceID
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
func (r *ChannelRepository) SaveAccount(ctx context.Context, v channel.AccountSecret, expected uint64) (channel.Account, error) {
	if !v.Provider.IsValid() || v.Provider.IsLegacy() || v.Name == "" || len(v.Name) > 160 || v.MaxConcurrent < 1 || v.MaxConcurrent > 256 || v.EncryptedCredentials == "" {
		return channel.Account{}, errors.New("invalid channel account")
	}
	models, err := json.Marshal(v.Models)
	if err != nil {
		return channel.Account{}, err
	}
	quotas, err := json.Marshal(v.Quotas)
	if err != nil {
		return channel.Account{}, err
	}
	err = r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		a := accountModel{ID: v.ID, Provider: string(v.Provider), Name: v.Name, Email: v.Identity.Email, UserID: v.Identity.UserID, TeamID: v.Identity.WorkspaceID, SourceKey: "channel:" + string(v.Provider), Enabled: v.Enabled, AuthStatus: string(account.AuthStatusActive), Priority: 1, MaxConcurrent: v.MaxConcurrent, BuildRouteMode: "auto", CreatedAt: now, UpdatedAt: now}
		if v.Status == "reauth_required" {
			a.AuthStatus = string(account.AuthStatusReauthRequired)
		}
		if v.ID == 0 {
			if expected != 0 {
				return channel.ErrConflict
			}
			if v.Identity.UserID != "" {
				a.IdentityKey = channelIdentity(v.Account)
			} else {
				sum := sha256.Sum256([]byte(string(v.Provider) + "\x00" + v.Fingerprint))
				a.IdentityKey = hex.EncodeToString(sum[:])
			}
			if err := tx.Omit(clause.Associations).Create(&a).Error; err != nil {
				return mapError(err)
			}
			v.ID = a.ID
			x := channelAccountModel{AccountID: a.ID, Provider: string(v.Provider), CredentialVersion: 1, Fingerprint: v.Fingerprint, Status: v.Status, Failure: v.Failure, FailureCount: v.FailureCount, NextMaintenance: v.NextMaintenance, Plan: v.Identity.Plan, Refreshable: v.Refreshable, ReplaceDuplicate: v.ReplaceDuplicate, ModelsJSON: string(models), QuotasJSON: string(quotas), SyncedAt: v.SyncedAt}
			if err := tx.Create(&x).Error; err != nil {
				return err
			}
		} else {
			var locked accountModel
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, "id = ? AND provider = ?", v.ID, v.Provider).Error; err != nil {
				return channelError(err)
			}
			result := tx.Model(&channelAccountModel{}).Where("account_id = ? AND provider = ? AND credential_version = ?", v.ID, v.Provider, expected).Updates(map[string]any{"credential_version": expected + 1, "fingerprint": v.Fingerprint, "status": v.Status, "failure": v.Failure, "failure_count": v.FailureCount, "next_maintenance": v.NextMaintenance, "plan": v.Identity.Plan, "refreshable": v.Refreshable, "replace_duplicate": v.ReplaceDuplicate, "models_json": string(models), "quotas_json": string(quotas), "synced_at": v.SyncedAt})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return channel.ErrConflict
			}
			updates := map[string]any{"name": a.Name, "email": a.Email, "user_id": a.UserID, "team_id": a.TeamID, "enabled": a.Enabled, "auth_status": a.AuthStatus, "max_concurrent": a.MaxConcurrent, "updated_at": now}
			if v.Identity.UserID != "" {
				updates["identity_key"] = channelIdentity(v.Account)
			}
			if err := tx.Model(&accountModel{}).Where("id = ? AND provider = ?", v.ID, v.Provider).Updates(updates).Error; err != nil {
				return err
			}
		}
		c := accountCredentialModel{AccountID: v.ID, AuthType: "channel_v1", EncryptedPrimary: v.EncryptedCredentials, ExpiresAt: v.ExpiresAt, UpdatedAt: now}
		for _, q := range v.Quotas {
			if q.Remaining == nil || q.ObservedAt.IsZero() {
				continue
			}
			// A fresh authoritative snapshot absorbs completed/uncertain sends
			// before the observation, but never current work or later reservations.
			activeImages := tx.Model(&channelImageItemModel{}).Select("id").Where("status IN ?", []string{"queued", "running"})
			activeChats := tx.Model(&channelExecutionSlotModel{}).Select("holder").Where("lease_until > ?", now)
			if err := tx.Model(&channelQuotaReservationModel{}).Where("account_id = ? AND feature = ? AND finished_at IS NULL AND created_at < ? AND event_id NOT IN (?) AND event_id NOT IN (?)", v.ID, q.Feature, q.ObservedAt, activeImages, activeChats).Updates(map[string]any{"state": gorm.Expr("CASE WHEN sent_amount = 0 THEN 'released' ELSE 'spent' END"), "amount": gorm.Expr("sent_amount"), "finished_at": q.ObservedAt.UTC()}).Error; err != nil {
				return err
			}
		}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoUpdates: clause.AssignmentColumns([]string{"auth_type", "encrypted_primary", "expires_at", "updated_at"})}).Create(&c).Error
	})
	if err != nil {
		return channel.Account{}, err
	}
	return r.GetAccount(ctx, v.Provider, v.ID)
}
func (r *ChannelRepository) DeleteAccount(ctx context.Context, p account.Provider, id uint64) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// A shared row lock serializes deletion with future workflow claims.
		var a accountModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&a, "id = ? AND provider = ?", id, p).Error; err != nil {
			return channelError(err)
		}
		if e := tx.Where("account_id = ? AND lease_until <= ?", id, time.Now().UTC()).Delete(&channelExecutionSlotModel{}).Error; e != nil {
			return e
		}
		var active int64
		if e := tx.Model(&channelImageItemModel{}).Where("account_id = ? AND status IN ?", id, []string{"queued", "running"}).Count(&active).Error; e != nil {
			return e
		}
		if active > 0 {
			return channel.ErrConflict
		}
		if e := tx.Model(&channelImageItemModel{}).Where("account_id = ?", id).Update("account_id", nil).Error; e != nil {
			return e
		}
		return tx.Delete(&a).Error
	})
}
func (r *ChannelRepository) CreateOAuth(ctx context.Context, v channel.OAuthSession) error {
	return r.db.db.WithContext(ctx).Create(&channelOAuthModel{ID: v.ID, Provider: string(v.Provider), AdminID: v.AdminID, EncryptedStart: v.EncryptedStart, StateHash: v.StateHash, Status: "pending", ExpiresAt: v.ExpiresAt, CreatedAt: time.Now().UTC()}).Error
}
func (r *ChannelRepository) ClaimOAuth(ctx context.Context, p account.Provider, id string, adminID uint64, stateHash string, now time.Time) (channel.OAuthSession, error) {
	var v channel.OAuthSession
	err := r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&channelOAuthModel{}).Where("id = ? AND provider = ? AND admin_id = ? AND state_hash = ? AND status = ? AND expires_at > ?", id, p, adminID, stateHash, "pending", now).Update("status", "exchanging")
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return channel.ErrConflict
		}
		var row channelOAuthModel
		if err := tx.First(&row, "id = ?", id).Error; err != nil {
			return err
		}
		v = channel.OAuthSession{ID: row.ID, Provider: p, AdminID: row.AdminID, EncryptedStart: row.EncryptedStart, StateHash: row.StateHash, Status: row.Status, ExpiresAt: row.ExpiresAt}
		return nil
	})
	return v, err
}
func (r *ChannelRepository) FinishOAuth(ctx context.Context, id, status string) error {
	return r.db.db.WithContext(ctx).Model(&channelOAuthModel{}).Where("id = ? AND status = ?", id, "exchanging").Updates(map[string]any{"status": status, "encrypted_start": ""}).Error
}
func (r *ChannelRepository) Grants(ctx context.Context, p account.Provider) ([]uint64, error) {
	out := []uint64{}
	err := r.db.db.WithContext(ctx).Model(&channelGrantModel{}).Where("provider = ?", p).Order("client_key_id").Pluck("client_key_id", &out).Error
	return out, err
}
func (r *ChannelRepository) SetGrant(ctx context.Context, p account.Provider, id uint64, on bool) error {
	if !on {
		return r.db.db.WithContext(ctx).Delete(&channelGrantModel{}, "provider = ? AND client_key_id = ?", p, id).Error
	}
	return r.db.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&channelGrantModel{Provider: string(p), ClientKeyID: id, CreatedAt: time.Now().UTC()}).Error
}
func (r *ChannelRepository) Granted(ctx context.Context, p account.Provider, id uint64) (bool, error) {
	var n int64
	err := r.db.db.WithContext(ctx).Model(&channelGrantModel{}).Where("provider = ? AND client_key_id = ?", p, id).Count(&n).Error
	return n == 1, err
}
func channelPrice(row channelPriceModel) channel.Price {
	return channel.Price{ID: row.ID, Provider: account.Provider(row.Provider), Model: row.Model, Operation: row.Operation, Version: row.ID, InputPerMillionTicks: row.InputPerMillionTicks, OutputPerMillionTicks: row.OutputPerMillionTicks, ImageTicks: row.ImageTicks, Quality: row.Quality, Size: row.Size, CreatedAt: row.CreatedAt}
}
func (r *ChannelRepository) Prices(ctx context.Context, p account.Provider) ([]channel.Price, error) {
	var rows []channelPriceModel
	err := r.db.db.WithContext(ctx).Where("provider = ?", p).Order("id DESC").Find(&rows).Error
	out := make([]channel.Price, 0, len(rows))
	for _, row := range rows {
		out = append(out, channelPrice(row))
	}
	return out, err
}
func (r *ChannelRepository) SavePrice(ctx context.Context, v channel.Price) (channel.Price, error) {
	row := channelPriceModel{Provider: string(v.Provider), Model: v.Model, Operation: v.Operation, InputPerMillionTicks: v.InputPerMillionTicks, OutputPerMillionTicks: v.OutputPerMillionTicks, ImageTicks: v.ImageTicks, Quality: v.Quality, Size: v.Size, CreatedAt: time.Now().UTC()}
	err := r.db.db.WithContext(ctx).Create(&row).Error
	return channelPrice(row), err
}

func (r *ChannelRepository) FindAccount(ctx context.Context, p account.Provider, fingerprint string, identity channel.Identity) (channel.Account, error) {
	var row accountModel
	q := r.db.db.WithContext(ctx).Model(&accountModel{}).Where("provider = ?", p)
	if identity.UserID != "" {
		q = q.Where("user_id = ? AND team_id = ?", identity.UserID, identity.WorkspaceID)
	} else if fingerprint != "" {
		q = q.Where("id IN (SELECT account_id FROM channel_accounts WHERE provider = ? AND fingerprint = ?)", p, fingerprint)
	} else {
		return channel.Account{}, channel.ErrNotFound
	}
	if err := q.First(&row).Error; err != nil {
		return channel.Account{}, channelError(err)
	}
	return r.GetAccount(ctx, p, row.ID)
}
