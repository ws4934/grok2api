package relational

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/account"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errAuditBatchRequiresFallback = errors.New("audit batch requires idempotent fallback")

type preparedAudit struct {
	row      requestAuditModel
	attempts []requestAuditAttemptModel
}

func (r *AuditRepository) Create(ctx context.Context, value audit.Record) error {
	return r.CreateBatch(ctx, []audit.Record{value})
}

func (r *AuditRepository) CreateBatch(ctx context.Context, values []audit.Record) error {
	if len(values) == 0 {
		return nil
	}
	prepared, err := prepareAudits(values)
	if err != nil {
		return err
	}
	err = r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return createPreparedAuditBatchFast(tx, prepared)
	})
	if !errors.Is(err, errAuditBatchRequiresFallback) {
		return err
	}
	return r.db.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return createPreparedAuditBatchSafe(tx, prepared)
	})
}

func prepareAudits(values []audit.Record) ([]preparedAudit, error) {
	prepared := make([]preparedAudit, 0, len(values))
	for index, value := range values {
		row, attempts, err := toAuditModels(value)
		if err != nil {
			return nil, &repository.InvalidBatchRecordError{Index: index, Err: err}
		}
		candidate := preparedAudit{row: row, attempts: attempts}
		if err := validatePreparedAudit(candidate); err != nil {
			return nil, &repository.InvalidBatchRecordError{Index: index, Err: err}
		}
		prepared = append(prepared, candidate)
	}
	return prepared, nil
}

func validatePreparedAudit(value preparedAudit) error {
	row := value.row
	if length := utf8.RuneCountInString(row.EventID); length < 16 || length > 64 {
		return errors.New("event_id length must be between 16 and 64")
	}
	if length := utf8.RuneCountInString(row.RequestID); length < 1 || length > 64 {
		return errors.New("request_id length must be between 1 and 64")
	}
	if row.ClientKeyID == 0 {
		return errors.New("client_key_id must be positive")
	}
	if row.ClientIP != "" && net.ParseIP(row.ClientIP) == nil {
		return errors.New("client_ip must be a valid IP address when present")
	}
	if row.ModelRouteID == 0 {
		return errors.New("model_route_id must be positive")
	}
	if !account.Provider(row.Provider).IsValid() {
		return errors.New("provider is invalid")
	}
	if !auditStringAllowed(row.Operation, "responses", "compaction", "chat", "messages", "image", "image_edit", "video", "tts", "stt", "realtime", "voice") {
		return errors.New("operation is invalid")
	}
	if !auditStringAllowed(row.UsageSource, "upstream", "estimated", "none") {
		return errors.New("usage_source is invalid")
	}
	if row.AccountID != nil && *row.AccountID == 0 {
		return errors.New("account_id must be positive when present")
	}
	if row.EgressNodeID != nil && *row.EgressNodeID == 0 {
		return errors.New("egress_node_id must be positive when present")
	}
	if row.EgressScope != "" && !account.Provider(row.EgressScope).IsValid() {
		return errors.New("egress_scope is invalid")
	}
	if !auditStringAllowed(row.EgressMode, "", "direct", "proxy") {
		return errors.New("egress_mode is invalid")
	}
	if row.StatusCode < 100 || row.StatusCode > 599 {
		return errors.New("status_code must be between 100 and 599")
	}
	if utf8.RuneCountInString(row.RequestMethod) > 16 {
		return errors.New("request method exceeds the storage limit")
	}
	if utf8.RuneCountInString(row.RequestPath) > 2048 {
		return errors.New("request path exceeds the storage limit")
	}
	if utf8.RuneCountInString(row.RequestHeadersJSON) > 65536 {
		return errors.New("request headers exceed the storage limit")
	}
	if len(row.RequestParametersJSON) > requestparams.MaxStoredBytes {
		return errors.New("request parameters exceed the storage limit")
	}
	attemptNumbers := make(map[int]struct{}, len(value.attempts))
	for index, attempt := range value.attempts {
		if err := validatePreparedAuditAttempt(attempt); err != nil {
			return fmt.Errorf("attempt %d: %w", index+1, err)
		}
		if _, exists := attemptNumbers[attempt.Number]; exists {
			return fmt.Errorf("attempt %d: number %d is duplicated", index+1, attempt.Number)
		}
		attemptNumbers[attempt.Number] = struct{}{}
	}
	return nil
}

func validatePreparedAuditAttempt(value requestAuditAttemptModel) error {
	if value.Number <= 0 {
		return errors.New("number must be positive")
	}
	if !auditStringAllowed(value.Source, "upstream_http", "gateway_transport", "credential") {
		return errors.New("source is invalid")
	}
	if length := utf8.RuneCountInString(strings.TrimSpace(value.Stage)); length < 1 || length > 64 {
		return errors.New("stage length must be between 1 and 64")
	}
	if value.AccountID != nil && *value.AccountID == 0 {
		return errors.New("account_id must be positive when present")
	}
	if value.UpstreamStatusCode != nil && (*value.UpstreamStatusCode < 100 || *value.UpstreamStatusCode > 599) {
		return errors.New("upstream_status_code must be between 100 and 599 when present")
	}
	if utf8.RuneCountInString(value.ResponseHeadersJSON) > 32768 {
		return errors.New("response headers exceed the storage limit")
	}
	if len(value.ResponseBody) > 65536 {
		return errors.New("response body exceeds the storage limit")
	}
	if utf8.RuneCountInString(value.ErrorChainJSON) > 32768 {
		return errors.New("error chain exceeds the storage limit")
	}
	return nil
}

func auditStringAllowed(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func createPreparedAuditBatchFast(tx *gorm.DB, prepared []preparedAudit) error {
	rows := make([]requestAuditModel, len(prepared))
	for index := range prepared {
		rows[index] = prepared[index].row
	}
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, auditInsertBatchSize)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}
	// A mixed new/duplicate result cannot be mapped safely across database RETURNING implementations.
	if result.RowsAffected != int64(len(rows)) {
		return errAuditBatchRequiresFallback
	}
	idsByEvent, err := loadInsertedAuditIDs(tx, prepared)
	if err != nil {
		return err
	}
	inserted := make([]preparedAudit, len(prepared))
	for index := range prepared {
		auditID := idsByEvent[prepared[index].row.EventID]
		if auditID == 0 {
			return errAuditBatchRequiresFallback
		}
		inserted[index] = prepared[index]
		inserted[index].row.ID = auditID
	}
	if err := insertPreparedAuditAttempts(tx, inserted); err != nil {
		return err
	}
	return settleInsertedAudits(tx, inserted)
}

func loadInsertedAuditIDs(tx *gorm.DB, prepared []preparedAudit) (map[string]uint64, error) {
	eventIDs := make([]string, len(prepared))
	for index := range prepared {
		eventIDs[index] = prepared[index].row.EventID
	}
	idsByEvent := make(map[string]uint64, len(eventIDs))
	// "event_id <> ''" lets SQLite and PostgreSQL use the partial unique index
	// idx_audits_event_id; without it large batches scan the whole table.
	for start := 0; start < len(eventIDs); start += auditLookupBatchSize {
		end := min(start+auditLookupBatchSize, len(eventIDs))
		var rows []struct {
			ID      uint64
			EventID string
		}
		if err := tx.Model(&requestAuditModel{}).Select("id", "event_id").Where("event_id IN ? AND event_id <> ''", eventIDs[start:end]).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			idsByEvent[row.EventID] = row.ID
		}
	}
	return idsByEvent, nil
}

func createPreparedAuditBatchSafe(tx *gorm.DB, prepared []preparedAudit) error {
	inserted := make([]preparedAudit, 0, len(prepared))
	for index := range prepared {
		row := prepared[index].row
		row.ID = 0
		wasInserted, err := insertAudit(tx, &row)
		if err != nil {
			return err
		}
		if !wasInserted {
			continue
		}
		attempts := append([]requestAuditAttemptModel(nil), prepared[index].attempts...)
		if err := insertAuditAttempts(tx, row.ID, attempts); err != nil {
			return err
		}
		inserted = append(inserted, preparedAudit{row: row})
	}
	return settleInsertedAudits(tx, inserted)
}

func insertPreparedAuditAttempts(tx *gorm.DB, inserted []preparedAudit) error {
	attemptCount := 0
	for _, value := range inserted {
		attemptCount += len(value.attempts)
	}
	if attemptCount == 0 {
		return nil
	}
	attempts := make([]requestAuditAttemptModel, 0, attemptCount)
	for _, value := range inserted {
		for _, attempt := range value.attempts {
			attempt.AuditID = value.row.ID
			attempts = append(attempts, attempt)
		}
	}
	return tx.CreateInBatches(&attempts, attemptInsertBatchSize).Error
}

func toAuditModels(value audit.Record) (requestAuditModel, []requestAuditAttemptModel, error) {
	estimationJSON := ""
	if value.Estimation != nil {
		raw, err := json.Marshal(value.Estimation)
		if err != nil {
			return requestAuditModel{}, nil, err
		}
		estimationJSON = string(raw)
	}
	provider := value.Provider
	if provider == "" {
		provider = "grok_build"
	}
	operation := value.Operation
	if operation == "" {
		operation = audit.OperationResponses
	}
	usageSource := value.UsageSource
	if usageSource == "" {
		usageSource = audit.UsageSourceUpstream
	}
	eventID := strings.TrimSpace(value.EventID)
	if eventID == "" {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%d", value.RequestID, value.ClientKeyID, value.ModelRouteID, value.CreatedAt.UnixNano())))
		eventID = fmt.Sprintf("evt_%x", digest[:18])
	}
	requestHeadersJSON := "{}"
	if len(value.RequestHeaders) > 0 {
		if raw, err := json.Marshal(value.RequestHeaders); err == nil {
			requestHeadersJSON = string(raw)
		}
	}
	parametersJSON := "{}"
	if parameters := value.RequestParameters.Freeze(); parameters != nil {
		raw, err := json.Marshal(parameters)
		if err != nil {
			return requestAuditModel{}, nil, err
		}
		parametersJSON = string(raw)
	}
	row := requestAuditModel{
		EstimationJSON: estimationJSON,
		EventID:        truncate(eventID, 64), RequestID: truncate(value.RequestID, 64), ClientKeyID: value.ClientKeyID, ClientKeyName: truncate(value.ClientKeyName, 160), ClientIP: strings.TrimSpace(value.ClientIP),
		ModelRouteID: value.ModelRouteID, ModelPublicID: truncate(value.ModelPublicID, 255), ModelUpstreamModel: truncate(value.ModelUpstreamModel, 255),
		Provider: truncate(provider, 32), Operation: string(operation), UsageSource: string(usageSource),
		ReasoningEffort: audit.NormalizeReasoningEffort(value.ReasoningEffort),
		AccountID:       value.AccountID, AccountName: truncate(value.AccountName, 160),
		EgressNodeID: value.EgressNodeID, EgressNodeName: truncate(value.EgressNodeName, 160), EgressScope: truncate(value.EgressScope, 32), EgressMode: string(value.EgressMode),
		StatusCode: value.StatusCode, Streaming: value.Streaming,
		MediaInputImages: nonNegative(value.MediaInputImages), MediaOutputImages: nonNegative(value.MediaOutputImages), MediaOutputSeconds: nonNegative(value.MediaOutputSeconds),
		InputTokens: nonNegative(value.InputTokens), CachedInputTokens: nonNegative(value.CachedInputTokens), OutputTokens: nonNegative(value.OutputTokens),
		ReasoningTokens: nonNegative(value.ReasoningTokens), TotalTokens: nonNegative(value.TotalTokens), CostInUSDTicks: nonNegative(value.CostInUSDTicks),
		EstimatedCostInUSDTicks: nonNegative(value.EstimatedCostInUSDTicks), PricingModel: truncate(value.PricingModel, 100), PricingVersion: truncate(value.PricingVersion, 20), PriceID: value.PriceID,
		NumSourcesUsed: nonNegative(value.NumSourcesUsed), NumServerSideToolsUsed: nonNegative(value.NumServerSideToolsUsed),
		ContextInputTokens: nonNegative(value.ContextInputTokens), ContextOutputTokens: nonNegative(value.ContextOutputTokens), FirstTokenMS: normalizedFirstToken(value), DurationMS: nonNegative(value.DurationMS),
		ErrorCode:             truncate(value.ErrorCode, 100),
		RequestMethod:         truncate(value.RequestMethod, 16),
		RequestPath:           truncate(value.RequestPath, 2048),
		RequestHeadersJSON:    truncate(requestHeadersJSON, 65536),
		RequestParametersJSON: parametersJSON,
		AttemptCount:          len(value.Attempts), CreatedAt: value.CreatedAt,
	}
	attempts := make([]requestAuditAttemptModel, 0, len(value.Attempts))
	for _, attempt := range value.Attempts {
		responseHeaders, err := json.Marshal(attempt.ResponseHeaders)
		if err != nil {
			return requestAuditModel{}, nil, fmt.Errorf("序列化审计响应头: %w", err)
		}
		if attempt.ResponseHeaders == nil {
			responseHeaders = []byte("{}")
		}
		errorChain, err := json.Marshal(attempt.ErrorChain)
		if err != nil {
			return requestAuditModel{}, nil, fmt.Errorf("序列化审计错误链: %w", err)
		}
		if attempt.ErrorChain == nil {
			errorChain = []byte("[]")
		}
		attempts = append(attempts, requestAuditAttemptModel{
			Number:                attempt.Number,
			Source:                string(attempt.Source),
			Stage:                 attempt.Stage,
			AccountID:             attempt.AccountID,
			AccountName:           truncate(attempt.AccountName, 160),
			Method:                truncate(attempt.Method, 16),
			RequestPath:           truncate(attempt.RequestPath, 2048),
			UpstreamURL:           truncate(attempt.UpstreamURL, 4096),
			StartedAt:             attempt.StartedAt,
			DurationMS:            nonNegative(attempt.DurationMS),
			UpstreamStatusCode:    attempt.UpstreamStatusCode,
			UpstreamStatus:        truncate(attempt.UpstreamStatus, 128),
			ResponseHeadersJSON:   string(responseHeaders),
			ResponseBody:          truncateBytes(attempt.ResponseBody, 65536),
			ResponseBodyTruncated: attempt.ResponseBodyTruncated || len(attempt.ResponseBody) > 65536,
			TransportError:        truncate(attempt.TransportError, 2048),
			ErrorChainJSON:        string(errorChain),
		})
	}
	return row, attempts, nil
}

func truncateBytes(value []byte, limit int) []byte {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func insertAudit(tx *gorm.DB, row *requestAuditModel) (bool, error) {
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	return result.RowsAffected == 1, result.Error
}

func insertAuditAttempts(tx *gorm.DB, auditID uint64, attempts []requestAuditAttemptModel) error {
	if len(attempts) == 0 {
		return nil
	}
	for index := range attempts {
		attempts[index].AuditID = auditID
	}
	return tx.Create(&attempts).Error
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func normalizedFirstToken(value audit.Record) *int64 {
	if value.FirstTokenMS == nil || !value.Streaming || value.StatusCode < 200 || value.StatusCode >= 300 || value.ErrorCode != "" {
		return nil
	}
	normalized := nonNegative(*value.FirstTokenMS)
	if normalized > nonNegative(value.DurationMS) {
		return nil
	}
	return &normalized
}
