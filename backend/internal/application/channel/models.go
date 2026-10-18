package channel

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/channel"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type ModelInput struct {
	PublicID      string   `json:"public_id"`
	UpstreamModel string   `json:"upstream_model"`
	Operation     string   `json:"operation"`
	Enabled       bool     `json:"enabled"`
	AccountIDs    []uint64 `json:"account_ids"`
}

func (s *Service) Models(ctx context.Context, p account.Provider) ([]model.Route, error) {
	if _, e := s.module(p); e != nil {
		return nil, e
	}
	rows, _, e := s.models.List(ctx, repository.ModelListQuery{Page: repository.PageQuery{Limit: 1000}, Filter: repository.ModelListFilter{Provider: string(p)}})
	return rows, e
}
func (s *Service) CreateModel(ctx context.Context, p account.Provider, v ModelInput) (model.Route, error) {
	route, e := s.validateModel(ctx, p, v)
	if e != nil {
		return model.Route{}, e
	}
	return s.models.Create(ctx, route, v.AccountIDs)
}
func (s *Service) SetModelEnabled(ctx context.Context, p account.Provider, id uint64, enabled bool) (model.Route, error) {
	old, e := s.models.Get(ctx, id)
	if e != nil {
		return old, e
	}
	if old.Provider != p {
		return model.Route{}, domain.ErrNotFound
	}
	route, e := s.validateModel(ctx, p, ModelInput{PublicID: old.PublicID, UpstreamModel: old.UpstreamModel, Operation: string(old.Capability), Enabled: enabled, AccountIDs: old.BoundAccountIDs})
	if e != nil {
		return model.Route{}, e
	}
	route.ID = id
	return s.models.Update(ctx, route, nil)
}
func (s *Service) validateModel(ctx context.Context, p account.Provider, v ModelInput) (model.Route, error) {
	m, e := s.module(p)
	if e != nil {
		return model.Route{}, e
	}
	caps := m.Descriptor().Capabilities
	capability := model.Capability(v.Operation)
	allowed := capability == model.CapabilityChat && caps.Chat || capability == model.CapabilityResponses && caps.Responses || capability == model.CapabilityImage && caps.ImageGeneration || capability == model.CapabilityImageEdit && caps.ImageEdit
	if !allowed {
		return model.Route{}, domain.ErrUnsupported
	}
	public, ok := model.NormalizeExternalPublicID(p, v.PublicID)
	if !ok {
		return model.Route{}, errors.New("invalid model name")
	}
	upstream, ok := model.NormalizeUpstreamModel(p, v.UpstreamModel)
	if !ok {
		return model.Route{}, errors.New("invalid upstream model")
	}
	if len(v.AccountIDs) > 1000 {
		return model.Route{}, errors.New("too many account bindings")
	}
	for _, id := range v.AccountIDs {
		if _, e := s.repo.GetAccount(ctx, p, id); e != nil {
			return model.Route{}, errors.New("invalid channel account binding")
		}
	}
	if v.Enabled {
		op := v.Operation
		if op == "responses" {
			op = "chat"
		}
		if _, e := s.resolvePrice(ctx, p, upstream, op, "", ""); e != nil {
			return model.Route{}, e
		}
		cfg, e := s.repo.Settings(ctx, p)
		if e != nil {
			return model.Route{}, e
		}
		target := upstream
		if op == "image" || op == "image_edit" {
			target = cfg.ImageConversationModel
		}
		found := false
		for offset := 0; ; offset += 200 {
			rows, total, e := s.repo.ListAccounts(ctx, p, offset, 200)
			if e != nil {
				return model.Route{}, e
			}
			for _, a := range rows {
				if a.Enabled && a.Status == "verified" && slices.Contains(a.Models, target) && (len(v.AccountIDs) == 0 || slices.Contains(v.AccountIDs, a.ID)) {
					found = true
				}
			}
			if offset+len(rows) >= int(total) {
				break
			}
		}
		if !found {
			return model.Route{}, errors.New("model has no verified supporting account")
		}
	}
	return model.Route{Provider: p, PublicID: public, UpstreamModel: upstream, Capability: capability, Origin: model.OriginManual, Enabled: v.Enabled}, nil
}
func (s *Service) DeleteModel(ctx context.Context, p account.Provider, id uint64) error {
	if _, e := s.module(p); e != nil {
		return e
	}
	v, e := s.models.Get(ctx, id)
	if e != nil {
		return e
	}
	if v.Provider != p {
		return domain.ErrNotFound
	}
	return s.models.Delete(ctx, id)
}
func (s *Service) Resolve(ctx context.Context, public, operation string) (model.Route, bool, error) {
	// Keep the original provider priority for shared external names.
	rows, e := s.models.GetByPublicIDCandidates(ctx, public)
	if e != nil && !errors.Is(e, repository.ErrNotFound) {
		return model.Route{}, false, e
	}
	for _, r := range rows {
		if string(r.Capability) != operation {
			continue
		}
		if _, ok := s.registry.Get(r.Provider); ok {
			hydrated, err := s.models.Get(ctx, r.ID)
			return hydrated, true, err
		}
		if r.Provider.IsLegacy() {
			return model.Route{}, false, nil
		}
	}
	if catalog, ok := s.models.(interface {
		GetConfiguredByPublicIDCandidates(context.Context, string) ([]model.Route, error)
	}); ok {
		configured, err := catalog.GetConfiguredByPublicIDCandidates(ctx, public)
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return model.Route{}, false, err
		}
		for _, r := range configured {
			if _, exists := s.registry.Get(r.Provider); exists && string(r.Capability) == operation {
				hydrated, err := s.models.Get(ctx, r.ID)
				return hydrated, true, err
			}
		}
	}
	return model.Route{}, false, nil
}
func (s *Service) resolvePrice(ctx context.Context, p account.Provider, modelName, op, quality, size string) (domain.Price, error) {
	rows, e := s.repo.Prices(ctx, p)
	if e != nil {
		return domain.Price{}, e
	}
	var best domain.Price
	bestScore := -1
	for _, r := range rows {
		if r.Model == modelName && r.Operation == op && (r.Quality == "" || r.Quality == quality) && (r.Size == "" || r.Size == size) {
			score := 0
			if r.Quality != "" {
				score++
			}
			if r.Size != "" {
				score++
			}
			if score > bestScore || (score == bestScore && r.Version > best.Version) {
				best, bestScore = r, score
			}
		}
	}
	if bestScore >= 0 {
		return best, nil
	}
	return domain.Price{}, errors.New("model has no configured price")
}
func (s *Service) ClientModels(ctx context.Context, key clientkey.Key) ([]model.Route, error) {
	rows, e := s.models.ListConfiguredEnabled(ctx)
	if e != nil {
		return nil, e
	}
	out := []model.Route{}
	for _, r := range rows {
		if _, ok := s.registry.Get(r.Provider); !ok || !key.AllowsModel(r.ID) {
			continue
		}
		cfg, e := s.repo.Settings(ctx, r.Provider)
		if e != nil {
			return nil, e
		}
		if !cfg.Enabled {
			continue
		}
		granted, e := s.repo.Granted(ctx, r.Provider, key.ID)
		if e != nil {
			return nil, e
		}
		if !granted {
			continue
		}
		op := string(r.Capability)
		if op == "responses" {
			op = "chat"
		}
		if _, e := s.resolvePrice(ctx, r.Provider, r.UpstreamModel, op, "", ""); e != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Service) AdminClientModels(ctx context.Context, p account.Provider, id uint64) ([]model.Route, error) {
	if s.clientKeys == nil {
		return nil, domain.ErrUnsupported
	}
	key, e := s.clientKeys.Get(ctx, id)
	if e != nil {
		return nil, e
	}
	if !key.IsAvailable(time.Now()) {
		return []model.Route{}, nil
	}
	rows, e := s.ClientModels(ctx, key)
	if e != nil {
		return nil, e
	}
	out := []model.Route{}
	for _, row := range rows {
		if row.Provider == p {
			row.PublicID = model.ExternalPublicID(p, row.PublicID)
			out = append(out, row)
		}
	}
	return out, nil
}
func (s *Service) authorize(ctx context.Context, key clientkey.Key, r model.Route) (domain.Settings, error) {
	if !r.Enabled || !key.IsAvailable(time.Now()) {
		return domain.Settings{}, domain.ErrNotFound
	}
	cfg, e := s.repo.Settings(ctx, r.Provider)
	if e != nil {
		return cfg, e
	}
	if !cfg.Enabled {
		return cfg, domain.ErrDisabled
	}
	granted, e := s.repo.Granted(ctx, r.Provider, key.ID)
	if e != nil {
		return cfg, e
	}
	if !granted || !key.AllowsModel(r.ID) {
		return cfg, domain.ErrNotFound
	}
	return cfg, nil
}
func cleanOperation(s string) string { return strings.TrimSpace(s) }

func (s *Service) CheckAccess(ctx context.Context, key clientkey.Key, r model.Route) error {
	_, e := s.authorize(ctx, key, r)
	return e
}
