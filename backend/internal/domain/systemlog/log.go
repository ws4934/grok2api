package systemlog

import "time"

const (
	RetentionDays = 7
	MaxEntries    = 10000
)

// Entry contains only bounded, redacted operational metadata, never request bodies.
type Entry struct {
	ID         uint64         `json:"id"`
	CreatedAt  time.Time      `json:"createdAt"`
	Level      string         `json:"level"`
	Message    string         `json:"message"`
	InstanceID string         `json:"instanceId"`
	Provider   string         `json:"provider"`
	AccountID  uint64         `json:"accountId"`
	RequestID  string         `json:"requestId"`
	Fields     map[string]any `json:"fields"`
}

type Query struct {
	BeforeID                           uint64
	Limit                              int
	Level, Search, Provider, RequestID string
	AccountID                          uint64
	Since                              time.Time
}
