package relational

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/systemlog"
	"gorm.io/gorm"
)

type systemLogModel struct {
	ID         uint64    `gorm:"primaryKey;autoIncrement"`
	CreatedAt  time.Time `gorm:"not null"`
	Level      string    `gorm:"size:8;not null"`
	Message    string    `gorm:"type:text;not null"`
	InstanceID string    `gorm:"size:128;not null"`
	Provider   string    `gorm:"size:64;not null"`
	AccountID  uint64    `gorm:"not null"`
	RequestID  string    `gorm:"size:64;not null"`
	FieldsJSON string    `gorm:"type:text;not null"`
}

func (systemLogModel) TableName() string { return "system_logs" }

type SystemLogRepository struct{ db *Database }

func NewSystemLogRepository(db *Database) *SystemLogRepository { return &SystemLogRepository{db: db} }

func (r *SystemLogRepository) AppendSystemLogs(ctx context.Context, entries []systemlog.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	rows := make([]systemLogModel, 0, len(entries))
	for _, entry := range entries {
		fieldsValue := entry.Fields
		if fieldsValue == nil {
			fieldsValue = map[string]any{}
		}
		fields, err := json.Marshal(fieldsValue)
		if err != nil {
			return err
		}
		rows = append(rows, systemLogModel{CreatedAt: entry.CreatedAt, Level: entry.Level, Message: entry.Message, InstanceID: entry.InstanceID, Provider: entry.Provider, AccountID: entry.AccountID, RequestID: entry.RequestID, FieldsJSON: string(fields)})
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.CreateInBatches(rows, 50).Error; err != nil {
			return err
		}
		return pruneSystemLogs(tx, time.Now().UTC().AddDate(0, 0, -systemlog.RetentionDays), systemlog.MaxEntries)
	})
}

func (r *SystemLogRepository) ListSystemLogs(ctx context.Context, input systemlog.Query) ([]systemlog.Entry, bool, error) {
	limit := min(max(input.Limit, 1), 200)
	query := r.db.db.WithContext(ctx).Model(&systemLogModel{})
	if input.BeforeID > 0 {
		query = query.Where("id < ?", input.BeforeID)
	}
	if input.Level != "" {
		query = query.Where("level = ?", input.Level)
	}
	if input.Provider != "" {
		query = query.Where("provider = ?", input.Provider)
	}
	if input.AccountID > 0 {
		query = query.Where("account_id = ?", input.AccountID)
	}
	if input.RequestID != "" {
		query = query.Where("request_id = ?", input.RequestID)
	}
	if !input.Since.IsZero() {
		query = query.Where("created_at >= ?", input.Since)
	}
	if input.Search != "" {
		pattern := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.ToLower(input.Search)) + "%"
		query = query.Where("(LOWER(message) LIKE ? ESCAPE '!' OR LOWER(fields_json) LIKE ? ESCAPE '!')", pattern, pattern)
	}
	var rows []systemLogModel
	if err := query.Order("id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	values := make([]systemlog.Entry, 0, len(rows))
	for _, row := range rows {
		fields := map[string]any{}
		if err := json.Unmarshal([]byte(row.FieldsJSON), &fields); err != nil {
			return nil, false, err
		}
		values = append(values, systemlog.Entry{ID: row.ID, CreatedAt: row.CreatedAt, Level: row.Level, Message: row.Message, InstanceID: row.InstanceID, Provider: row.Provider, AccountID: row.AccountID, RequestID: row.RequestID, Fields: fields})
	}
	return values, hasMore, nil
}

// Keep the newest rows globally across instances. No inference/audit tables are touched.
func (r *SystemLogRepository) PruneSystemLogs(ctx context.Context, cutoff time.Time, keep int) error {
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return pruneSystemLogs(tx, cutoff, keep) })
}

func pruneSystemLogs(tx *gorm.DB, cutoff time.Time, keep int) error {
	if err := tx.Where("created_at < ?", cutoff).Delete(&systemLogModel{}).Error; err != nil {
		return err
	}
	var boundary systemLogModel
	err := tx.Select("id").Order("id DESC").Offset(max(keep, 1)).Limit(1).Find(&boundary).Error
	if err != nil || boundary.ID == 0 {
		return err
	}
	return tx.Where("id <= ?", boundary.ID).Delete(&systemLogModel{}).Error
}
