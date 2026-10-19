package relational

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// modelProviderPriorityExpression orders routes that share a public name by
// account.Providers(), the same order the gateway uses.
var modelProviderPriorityExpression = func() string {
	parts := []string{"CASE model_routes.provider"}
	for index, value := range account.Providers() {
		parts = append(parts, fmt.Sprintf("WHEN '%s' THEN %d", value, index))
	}
	return strings.Join(append(parts, fmt.Sprintf("ELSE %d END", len(account.Providers()))), " ")
}()

func (r *ModelRepository) Get(ctx context.Context, id uint64) (model.Route, error) {
	var row modelRouteModel
	if err := r.db.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return model.Route{}, mapError(err)
	}
	values := []model.Route{toModelDomain(row)}
	if err := r.annotateAvailability(ctx, values); err != nil {
		return model.Route{}, err
	}
	return values[0], nil
}

func (r *ModelRepository) GetByPublicID(ctx context.Context, publicID string) (model.Route, error) {
	values, err := r.GetByPublicIDCandidates(ctx, publicID)
	if err != nil {
		return model.Route{}, mapError(err)
	}
	return values[0], nil
}

// GetByPublicIDCandidates 返回同一下游模型名称当前可用的全部来源路由。
// 返回顺序遵循 Build、Web、Console 的稳定 Provider 顺序。
func (r *ModelRepository) GetByPublicIDCandidates(ctx context.Context, publicID string) ([]model.Route, error) {
	db := r.availableRoutes(r.db.db.WithContext(ctx)).Where("enabled = ?", true)
	rows, err := findModelRoutesByPublicID(db, publicID)
	if err != nil {
		return nil, mapError(err)
	}
	return mapModelRows(rows), nil
}

func (r *ModelRepository) GetByPublicIDIncludingDisabled(ctx context.Context, publicID string) (model.Route, error) {
	db := r.db.db.WithContext(ctx)
	rows, err := findModelRoutesByPublicID(db, publicID)
	if err != nil {
		return model.Route{}, mapError(err)
	}
	return toModelDomain(rows[0]), nil
}

func findModelRoutesByPublicID(db *gorm.DB, publicID string) ([]modelRouteModel, error) {
	var catalog []account.Provider
	// NewDB drops the caller's availability predicate (nested per-route EXISTS):
	// the catalog only widens namespace parsing; the route queries below stay scoped.
	if err := db.Session(&gorm.Session{NewDB: true}).Model(&modelRouteModel{}).Distinct("provider").Pluck("provider", &catalog).Error; err != nil {
		return nil, err
	}
	providers := account.Providers()
	for _, p := range catalog {
		if p.IsValid() && !slices.Contains(providers, p) {
			providers = append(providers, p)
		}
	}
	groups := model.PublicIDCandidateGroupsFor(publicID, providers)
	requested := strings.TrimSpace(publicID)
	for index, candidates := range groups {
		aliasRequested := ""
		if len(groups) == 1 || index == len(groups)-1 {
			aliasRequested = requested
		}
		rows, err := findModelRoutesByPublicIDGroup(db, candidates, aliasRequested)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			return rows, nil
		}
	}
	if len(groups) > 0 {
		return nil, gorm.ErrRecordNotFound
	}
	query := db.Session(&gorm.Session{})
	query = query.Where(`
		model_routes.id IN (SELECT alias.model_route_id FROM model_route_aliases alias WHERE alias.alias = ?)
	`, requested).Order(modelProviderPriorityExpression + ", model_routes.id ASC")
	var rows []modelRouteModel
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return rows, nil
}

func findModelRoutesByPublicIDGroup(db *gorm.DB, candidates []string, requested string) ([]modelRouteModel, error) {
	aliasCandidates := append([]string(nil), candidates...)
	if requested != "" && !slices.Contains(aliasCandidates, requested) {
		aliasCandidates = append(aliasCandidates, requested)
	}
	// Matching IDs come from the indexes first, so the caller's availability
	// predicate runs only for those routes (an OR EXISTS forced a full scan).
	query := db.Session(&gorm.Session{}).Where(`
		model_routes.id IN (
			SELECT route.id FROM model_routes route WHERE route.public_id IN ?
			UNION SELECT alias.model_route_id FROM model_route_aliases alias WHERE alias.alias IN ?
		)
	`, candidates, aliasCandidates).Clauses(clause.OrderBy{Expression: clause.Expr{
		SQL:  "CASE WHEN model_routes.public_id IN ? THEN 0 ELSE 1 END, " + modelProviderPriorityExpression + ", model_routes.id ASC",
		Vars: []any{candidates},
	}})
	var rows []modelRouteModel
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *ModelRepository) GetByProviderUpstream(ctx context.Context, provider account.Provider, upstreamModel string) (model.Route, error) {
	var rows []modelRouteModel
	if err := r.availableRoutes(r.db.db.WithContext(ctx)).Where("provider = ? AND upstream_model = ? AND enabled = ?", provider, upstreamModel, true).Find(&rows).Error; err != nil {
		return model.Route{}, mapError(err)
	}
	if len(rows) == 0 {
		return model.Route{}, repository.ErrNotFound
	}
	return toModelDomain(preferProviderUpstreamRoute(provider, upstreamModel, rows)), nil
}

// preferProviderUpstreamRoute 在同一上游存在多条对外名路由时选出稳定代表路由。
func preferProviderUpstreamRoute(provider account.Provider, upstreamModel string, rows []modelRouteModel) modelRouteModel {
	preferred := rows[0]
	localID, _ := discoveredRouteDefaults(provider, upstreamModel)
	canonical, hasCanonical := model.NormalizePublicID(provider, localID)
	for _, row := range rows {
		if hasCanonical && row.PublicID == canonical {
			return row
		}
		originPreferred := row.Origin == string(model.OriginDiscovered) || row.Origin == string(model.OriginCatalog)
		currentPreferred := preferred.Origin == string(model.OriginDiscovered) || preferred.Origin == string(model.OriginCatalog)
		switch {
		case originPreferred && !currentPreferred:
			preferred = row
		case originPreferred == currentPreferred && row.ID < preferred.ID:
			preferred = row
		}
	}
	return preferred
}
