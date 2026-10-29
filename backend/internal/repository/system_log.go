package repository

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/systemlog"
)

type SystemLogRepository interface {
	AppendSystemLogs(context.Context, []systemlog.Entry) error
	ListSystemLogs(context.Context, systemlog.Query) ([]systemlog.Entry, bool, error)
	PruneSystemLogs(context.Context, time.Time, int) error
}
