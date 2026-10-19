package gateway

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

const responseOwnershipTTL = 30 * 24 * time.Hour

func (s *Service) CreateResponse(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationResponses
	switch classifyResponsesCompactionRequest(input.Body) {
	case responsesCompactionTrigger:
		input.Operation = audit.OperationCompaction
	case responsesCompactionTUI:
		// Grok TUI compaction is still a normal Responses request. Keep its
		// routing, Provider normalization, and stored-response behavior intact;
		// only its audit classification and quality-hold policy differ.
		input.auditOperation = audit.OperationCompaction
		input.skipQualityHold = true
	case responsesCompactionNone:
		input.notCompaction = true
	}
	return s.createResponseAt(ctx, input, "/responses")
}

func (s *Service) CreateChatCompletion(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationChat
	return s.createResponseAt(ctx, input, "/responses")
}

// CreateMessage executes an Anthropic Messages request through the unified Responses upstream.
func (s *Service) CreateMessage(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationMessages
	return s.createResponseAt(ctx, input, "/responses")
}

func (s *Service) CompactResponse(ctx context.Context, input Input) (*Result, error) {
	input.Streaming = false
	input.Operation = audit.OperationCompaction
	return s.createResponseAt(ctx, input, "/responses/compact")
}

func (s *Service) createResponseAt(ctx context.Context, input Input, path string) (*Result, error) {
	a, modelNotAllowed, err := s.newResponseAttempt(ctx, input, path)
	if err != nil {
		return nil, err
	}
	defer a.finishTiming()
	if err := a.prepare(modelNotAllowed); err != nil {
		return nil, err
	}
	for attempt := 0; a.attemptPolicy.allows(attempt); attempt++ {
		if a.qualityHoldEnabled && a.qualityAccountAttempts >= a.holdCfg.MaxAttempts {
			break
		}
		result, step := a.try(attempt)
		if result != nil {
			return result, nil
		}
		if step == attemptStop {
			break
		}
		if step == attemptRepeat {
			attempt--
		}
	}
	return a.finish()
}
