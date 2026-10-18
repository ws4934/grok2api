package channel

import (
	"context"

	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
)

// EstimateChatInput returns the input estimate Chat bills for r, for protocols
// that report input usage before the reply finishes (Messages message_start).
func (s *Service) EstimateChatInput(ctx context.Context, route model.Route, r domain.ChatRequest) int64 {
	cfg, e := s.repo.Settings(ctx, route.Provider)
	if e != nil || s.execution == nil {
		return 0
	}
	n, _ := s.estimate(r, cfg.VisionTokens)
	return n
}
