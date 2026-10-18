package channel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/google/uuid"
	"github.com/tiktoken-go/tokenizer"
)

const EstimatorVersion = "o200k_base/messages_v1"

type Billing interface {
	ReserveBilling(context.Context, clientkey.Key, string, int64, time.Duration) (bool, error)
	CancelBilling(context.Context, string) error
	CompleteBilling(string)
}
type AuditWriter interface {
	CreateDurable(context.Context, auditdomain.Record) error
}
type Execution struct {
	billing Billing
	audits  AuditWriter
	codec   tokenizer.Codec
	mu      sync.Mutex
}

func (s *Service) SetExecution(billing Billing, audits AuditWriter) error {
	codec, e := tokenizer.Get(tokenizer.O200kBase)
	if e != nil {
		return e
	}
	s.execution = &Execution{billing: billing, audits: audits, codec: codec}
	return nil
}
func (s *Service) count(text string) (int64, error) {
	s.execution.mu.Lock()
	defer s.execution.mu.Unlock()
	n, e := s.execution.codec.Count(text)
	return int64(n), e
}
func (s *Service) estimate(r domain.ChatRequest, vision int) (int64, error) {
	n := int64(3)
	for _, m := range r.Messages {
		n += 4
		for _, p := range m.Content {
			if p.Type == "text" {
				v, e := s.count(p.Text)
				if e != nil {
					return 0, e
				}
				n += v
			} else {
				n += int64(vision)
			}
		}
	}
	return n, nil
}
func tokenCost(n, rate int64) int64 {
	if n <= 0 || rate <= 0 {
		return 0
	}
	return (n*rate + 999999) / 1000000
}

type ChatResult struct {
	Text         string `json:"text"`
	Reasoning    string `json:"reasoning"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Truncated    bool   `json:"truncated"`
}

var errOutputLimit = errors.New("local output limit")

func (s *Service) Chat(ctx context.Context, key clientkey.Key, route model.Route, r domain.ChatRequest, stream bool, emit func(domain.ChatEvent) error) (ChatResult, error) {
	result := ChatResult{}
	cfg, e := s.authorize(ctx, key, route)
	if e != nil {
		return result, e
	}
	price, e := s.resolvePrice(ctx, route.Provider, route.UpstreamModel, "chat", "", "")
	if e != nil {
		return result, e
	}
	r.Model = route.UpstreamModel
	m, e := s.module(route.Provider)
	if e != nil {
		return result, e
	}
	adapter, ok := m.(domain.ChatAdapter)
	if !ok {
		return result, domain.ErrUnsupported
	}
	if s.execution == nil {
		return result, errors.New("channel execution unavailable")
	}
	result.InputTokens, e = s.estimate(r, cfg.VisionTokens)
	if e != nil {
		return result, e
	}
	if result.InputTokens > 1000000 {
		return result, errors.New("input exceeds gateway estimate limit")
	}
	event := uuid.NewString()
	reserve := tokenCost(result.InputTokens, price.InputPerMillionTicks) + tokenCost(int64(cfg.OutputTokenLimit), price.OutputPerMillionTicks)
	_, e = s.execution.billing.ReserveBilling(ctx, key, event, reserve, 3*time.Minute)
	if e != nil {
		return result, e
	}
	accepted := false
	defer func() {
		if !accepted {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = s.execution.billing.CancelBilling(cleanup, event)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var selected domain.Account
	var release func()
	for offset := 0; release == nil; offset += 200 {
		rows, total, e := s.repo.ListAccounts(ctx, route.Provider, offset, 200)
		if e != nil {
			return result, e
		}
		for _, a := range rows {
			if !a.Enabled || a.Status != "verified" || !slices.Contains(a.Models, r.Model) || (len(route.BoundAccountIDs) > 0 && !slices.Contains(route.BoundAccountIDs, a.ID)) {
				continue
			}
			if s.images == nil {
				return result, errors.New("channel slot store unavailable")
			}
			if e := s.images.jobs.AcquireChannelSlot(ctx, a.ID, event, time.Now().UTC().Add(2*time.Minute)); e == nil {
				if policy, ok := m.(domain.QuotaPolicy); ok {
					if err := s.images.jobs.ReserveChannelQuota(ctx, a.ID, event, policy.ChatQuota(r)); err != nil {
						_ = s.images.jobs.ReleaseChannelSlot(ctx, event)
						if errors.Is(err, domain.ErrQuotaExhausted) {
							continue
						}
						return result, err
					}
				}
				release = func() {
					c, end := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					defer end()
					_ = s.images.jobs.ReleaseChannelSlot(c, event)
				}
				selected = a
				break
			} else if !errors.Is(e, domain.ErrConflict) {
				return result, e
			}

		}
		if offset+len(rows) >= int(total) {
			break
		}
	}
	if release == nil {
		return result, errors.New("no available verified channel account")
	}
	defer release()
	defer func() {
		c, end := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer end()
		_ = s.images.jobs.FinishChannelQuota(c, event, false)
	}()
	secret, e := s.repo.GetSecret(ctx, route.Provider, selected.ID)
	if e != nil {
		return result, e
	}
	credentials, e := s.decrypt(secret)
	if e != nil {
		return result, e
	}
	ctx, releaseEnvironment, e := s.withEnvironment(ctx, route.Provider, selected.ID)
	if e != nil {
		return result, e
	}
	defer releaseEnvironment()
	ctx = domain.WithQuotaObserver(ctx, func(feature string) error { return s.images.jobs.MarkChannelQuotaSent(ctx, event, feature) })
	started := time.Now()
	runErr := adapter.Chat(ctx, credentials, r, func(ev domain.ChatEvent) error {
		if ev.Done && ev.Text == "" && ev.Reasoning == "" {
			return nil
		}
		ev.Done = false // Completion is emitted after durable settlement.
		oldText, oldReason := result.Text, result.Reasoning
		result.Text += ev.Text
		result.Reasoning += ev.Reasoning
		count, e := s.count(result.Text + result.Reasoning)
		if e != nil {
			return e
		}
		if count > int64(cfg.OutputTokenLimit) {
			combined := []rune(ev.Text + ev.Reasoning)
			lo, hi := 0, len(combined)
			for lo < hi {
				mid := (lo + hi + 1) / 2
				n, e := s.count(oldText + oldReason + string(combined[:mid]))
				if e != nil {
					return e
				}
				if n <= int64(cfg.OutputTokenLimit) {
					lo = mid
				} else {
					hi = mid - 1
				}
			}
			result.Text = oldText
			result.Reasoning = oldReason
			if ev.Text != "" {
				ev.Text = string(combined[:lo])
				ev.Reasoning = ""
				result.Text += ev.Text
			} else {
				ev.Reasoning = string(combined[:lo])
				result.Reasoning += ev.Reasoning
			}
			result.Truncated = true
		}
		result.OutputTokens, e = s.count(result.Text + result.Reasoning)
		if e != nil {
			return e
		}
		if ev.Text != "" || ev.Reasoning != "" {
			if e := emit(ev); e != nil {
				return e
			}
		}
		if result.Truncated {
			return errOutputLimit
		}
		return nil
	})
	if errors.Is(runErr, errOutputLimit) {
		runErr = nil
	}
	amount := int64(0)
	if result.Text != "" || result.Reasoning != "" {
		amount = tokenCost(result.InputTokens, price.InputPerMillionTicks) + tokenCost(result.OutputTokens, price.OutputPerMillionTicks)
	}
	status := 200
	if runErr != nil {
		status = 502
	}
	id := selected.ID
	record := auditdomain.Record{EventID: event, RequestID: event, ClientKeyID: key.ID, ClientKeyName: key.Name, ModelRouteID: route.ID, ModelPublicID: route.PublicID, ModelUpstreamModel: route.UpstreamModel, Provider: string(route.Provider), Operation: auditdomain.Operation(route.Capability), UsageSource: auditdomain.UsageSourceEstimated, AccountID: &id, AccountName: selected.Name, StatusCode: status, Streaming: stream, InputTokens: result.InputTokens, OutputTokens: result.OutputTokens, TotalTokens: result.InputTokens + result.OutputTokens, EstimatedCostInUSDTicks: amount, PricingModel: route.UpstreamModel, PricingVersion: fmt.Sprintf("c%d:o2m1", price.Version), DurationMS: time.Since(started).Milliseconds(), CreatedAt: time.Now().UTC()}
	env := domain.EnvironmentFrom(ctx)
	record.EgressScope = string(route.Provider)
	record.EgressMode = auditdomain.EgressMode("direct")
	if env.ProxyURL != "" {
		record.EgressMode = auditdomain.EgressMode("proxy")
	}
	if env.NodeID > 0 {
		record.EgressNodeID = &env.NodeID
	}
	record.Estimation = &auditdomain.EstimationDetails{Version: 1, PriceVersion: price.Version, Tokenizer: EstimatorVersion, VisionTokensPerImage: cfg.VisionTokens, OutputLimitTokens: cfg.OutputTokenLimit, Truncated: result.Truncated}
	for _, message := range r.Messages {
		for _, part := range message.Content {
			if part.Type == "image_url" {
				record.MediaInputImages++
			}
		}
	}
	settle, settleCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer settleCancel()
	// An acknowledgement timeout does not prove the audit transaction rolled
	// back. Keep the reservation until the durable writer commits or it expires.
	accepted = true
	if e := s.execution.audits.CreateDurable(settle, record); e != nil {
		s.execution.billing.CompleteBilling(event)
		return result, errors.New("usage settlement failed")
	}
	accepted = true
	s.execution.billing.CompleteBilling(event)
	if runErr != nil {
		return result, runErr
	}
	return result, emit(domain.ChatEvent{Done: true, Truncated: result.Truncated})
}
