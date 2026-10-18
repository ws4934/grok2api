package relational

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/channel"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type channelExecutionSlotModel struct {
	Holder     string        `gorm:"size:64;primaryKey"`
	AccountID  uint64        `gorm:"not null;index"`
	LeaseUntil time.Time     `gorm:"not null;index"`
	Account    *accountModel `gorm:"foreignKey:AccountID;references:ID;constraint:OnDelete:RESTRICT"`
}

func (channelExecutionSlotModel) TableName() string { return "channel_execution_slots" }
func acquireChannelSlot(tx *gorm.DB, id uint64, holder string, until time.Time) error {
	until = until.UTC()
	var a accountModel
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&a, "id = ?", id).Error; e != nil {
		return e
	}
	now := time.Now().UTC()
	if e := tx.Where("account_id = ? AND lease_until <= ?", id, now).Delete(&channelExecutionSlotModel{}).Error; e != nil {
		return e
	}
	var n int64
	if e := tx.Model(&channelExecutionSlotModel{}).Where("account_id = ? AND holder <> ?", id, holder).Count(&n).Error; e != nil {
		return e
	}
	if n >= int64(a.MaxConcurrent) {
		return channel.ErrConflict
	}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "holder"}}, DoUpdates: clause.AssignmentColumns([]string{"lease_until"})}).Create(&channelExecutionSlotModel{Holder: holder, AccountID: id, LeaseUntil: until}).Error
}
func (r *ChannelRepository) AcquireChannelSlot(ctx context.Context, id uint64, holder string, until time.Time) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return acquireChannelSlot(tx, id, holder, until) })
}
func (r *ChannelRepository) ReleaseChannelSlot(ctx context.Context, holder string) error {
	return r.db.db.WithContext(ctx).Delete(&channelExecutionSlotModel{}, "holder = ?", holder).Error
}
