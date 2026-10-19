package relational

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

const (
	modelSupportSortExpression = `(SELECT COUNT(*) FROM provider_accounts account WHERE account.provider = model_routes.provider AND account.enabled = TRUE AND account.auth_status = 'active' AND (EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id AND binding.account_id = account.id) OR (NOT EXISTS (SELECT 1 FROM model_route_accounts binding WHERE binding.model_route_id = model_routes.id) AND (` + modelConsoleStaticSupportExpression + ` OR ` + modelWebBasicMediaStaticSupportExpression + ` OR EXISTS (SELECT 1 FROM account_model_capabilities capability WHERE capability.account_id = account.id AND capability.upstream_model = model_routes.upstream_model) OR ` + modelSharedPaidBuildSupportSortExpression + `))))`
	modelSyncedSortExpression  = `(SELECT MAX(sync.last_success_at) FROM provider_accounts account JOIN account_model_sync_states sync ON sync.account_id = account.id WHERE account.provider = model_routes.provider AND account.enabled = TRUE AND account.auth_status = 'active')`
)

func (r *ModelRepository) List(ctx context.Context, input repository.ModelListQuery) ([]model.Route, int64, error) {
	var total int64
	query := r.db.db.WithContext(ctx).Model(&modelRouteModel{})
	if input.Filter.ActiveScope {
		query = r.availableRoutes(query)
	}
	if search := strings.TrimSpace(input.Page.Search); search != "" {
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where("LOWER(public_id) LIKE ? OR LOWER(upstream_model) LIKE ?", pattern, pattern)
	}
	if input.Filter.Provider != "" {
		query = query.Where("provider = ?", input.Filter.Provider)
	}
	if len(input.Filter.Providers) > 0 {
		query = query.Where("provider IN ?", input.Filter.Providers)
	}
	tierPredicate := modelTierAvailabilityPredicate(input.Filter.Tiers, input.Filter.TierExemptProviders)
	if input.Filter.ActiveScope {
		tierPredicate = modelTierAvailabilityPredicateWithAvailability(input.Filter.Tiers, input.Filter.TierExemptProviders, true)
	}
	if tierPredicate != "" {
		query = query.Where(tierPredicate)
	}
	if input.Filter.Enabled != nil {
		query = query.Where("enabled = ?", *input.Filter.Enabled)
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []modelRouteModel
	query = applyStableSort(query, input.Page.Sort, map[string]sortSpec{
		"publicId":       {expression: "LOWER(model_routes.public_id)"},
		"upstreamModel":  {expression: "LOWER(model_routes.upstream_model)"},
		"status":         {expression: "CASE WHEN model_routes.enabled = TRUE THEN 0 ELSE 1 END"},
		"provider":       {expression: "model_routes.provider"},
		"accountSupport": {expression: modelSupportSortExpression, defaultDirection: repository.SortDescending},
		"lastSyncedAt":   {expression: modelSyncedSortExpression, nullsLast: true, defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "model_routes.created_at", defaultDirection: repository.SortDescending}, "model_routes.id")
	if err := query.Offset(input.Page.Offset).Limit(input.Page.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, 0, err
	}
	return values, total, nil
}

type modelRouteGroupRow struct {
	Provider      string
	PublicID      string
	UpstreamModel string
	Origin        string
	ManualRouteID uint64
	RouteIDs      string
}

// ListGroups paginates logical model targets before loading their member
// routes. This keeps capability groups complete under filters and avoids the
// admin UI fetching and annotating the entire route table.
func (r *ModelRepository) ListGroups(ctx context.Context, input repository.ModelListQuery) ([]model.RouteGroup, int64, error) {
	// Route-level metrics must be calculated before grouping. In particular,
	// PostgreSQL does not allow a correlated metric to reference model_routes.id
	// from inside a grouped SELECT when id is not itself a grouping key.
	buildRouteMetricsQuery := func(includeMetrics bool) *gorm.DB {
		columns := []string{
			"model_routes.id",
			"model_routes.provider",
			"model_routes.public_id",
			"model_routes.upstream_model",
			"model_routes.origin",
			"model_routes.enabled",
			"model_routes.created_at",
		}
		if includeMetrics && input.Page.Sort.Field == "accountSupport" {
			columns = append(columns, modelSupportSortExpression+" AS support_sort")
		}
		if includeMetrics && input.Page.Sort.Field == "lastSyncedAt" {
			columns = append(columns, modelSyncedSortExpression+" AS last_synced_sort")
		}
		query := r.db.db.WithContext(ctx).Model(&modelRouteModel{}).Select(strings.Join(columns, ", "))
		if search := strings.TrimSpace(input.Page.Search); search != "" {
			pattern := "%" + strings.ToLower(search) + "%"
			query = query.Where("LOWER(model_routes.public_id) LIKE ? OR LOWER(model_routes.upstream_model) LIKE ?", pattern, pattern)
		}
		if input.Filter.Provider != "" {
			query = query.Where("model_routes.provider = ?", input.Filter.Provider)
		}
		return query
	}

	manualRouteIDExpression := "CASE WHEN route_metrics.origin = 'manual' THEN route_metrics.id ELSE 0 END"
	routeIDsExpression := "GROUP_CONCAT(CAST(route_metrics.id AS TEXT), ',')"
	if r.db.dialect == "postgres" {
		routeIDsExpression = "STRING_AGG(CAST(route_metrics.id AS TEXT), ',' ORDER BY route_metrics.id)"
	}
	enabledCountExpression := "SUM(CASE WHEN route_metrics.enabled = TRUE THEN 1 ELSE 0 END)"
	groupColumns := "route_metrics.provider, route_metrics.public_id, route_metrics.upstream_model, route_metrics.origin, " + manualRouteIDExpression
	supportSortExpression := "0"
	if input.Page.Sort.Field == "accountSupport" {
		supportSortExpression = "MAX(route_metrics.support_sort)"
	}
	lastSyncedSortExpression := "NULL"
	if input.Page.Sort.Field == "lastSyncedAt" {
		lastSyncedSortExpression = "MAX(route_metrics.last_synced_sort)"
	}

	buildQuery := func(includeMetrics bool) *gorm.DB {
		query := r.db.db.WithContext(ctx).Table("(?) AS route_metrics", buildRouteMetricsQuery(includeMetrics))
		if includeMetrics {
			query = query.Select(fmt.Sprintf(`
				route_metrics.provider,
				route_metrics.public_id,
				route_metrics.upstream_model,
				route_metrics.origin,
				%s AS manual_route_id,
				%s AS route_ids,
				CASE
					WHEN %s = COUNT(*) THEN 0
					WHEN %s > 0 THEN 1
					ELSE 2
				END AS status_sort,
				%s AS group_support_sort,
				%s AS group_last_synced_sort,
				MAX(route_metrics.created_at) AS created_sort
			`, manualRouteIDExpression, routeIDsExpression, enabledCountExpression, enabledCountExpression, supportSortExpression, lastSyncedSortExpression))
		} else {
			query = query.Select("route_metrics.provider, route_metrics.public_id, route_metrics.upstream_model, route_metrics.origin, " + manualRouteIDExpression + " AS manual_route_id")
		}
		query = query.Group(groupColumns)
		if input.Filter.Enabled != nil {
			if *input.Filter.Enabled {
				query = query.Having(enabledCountExpression + " > 0")
			} else {
				query = query.Having(enabledCountExpression + " = 0")
			}
		}
		return query
	}

	var total int64
	if err := r.db.db.WithContext(ctx).Table("(?) AS grouped_model_routes", buildQuery(false)).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	query := buildQuery(true)
	spec, direction := stableSortSpec(input.Page.Sort, map[string]sortSpec{
		"publicId":       {expression: "LOWER(route_metrics.public_id)"},
		"upstreamModel":  {expression: "LOWER(route_metrics.upstream_model)"},
		"status":         {expression: "status_sort"},
		"provider":       {expression: "route_metrics.provider"},
		"accountSupport": {expression: "group_support_sort", defaultDirection: repository.SortDescending},
		"lastSyncedAt":   {expression: "group_last_synced_sort", nullsLast: true, defaultDirection: repository.SortDescending},
	}, sortSpec{expression: "created_sort", defaultDirection: repository.SortDescending})
	if spec.nullsLast {
		// PostgreSQL permits a SELECT alias as a plain ORDER BY item, but not
		// inside another ORDER BY expression. Repeat the aggregate only for the
		// NULL discriminator and use the alias for the value ordering below.
		nullSortExpression := spec.expression
		if input.Page.Sort.Field == "lastSyncedAt" {
			nullSortExpression = lastSyncedSortExpression
		}
		query = query.Order("CASE WHEN " + nullSortExpression + " IS NULL THEN 1 ELSE 0 END ASC")
	}
	query = query.Order(spec.expression + " " + direction).
		Order("route_metrics.provider ASC").
		Order("LOWER(route_metrics.public_id) ASC").
		Order("LOWER(route_metrics.upstream_model) ASC").
		Order("route_metrics.origin ASC").
		Order("manual_route_id ASC")
	var groupRows []modelRouteGroupRow
	if err := query.Offset(input.Page.Offset).Limit(input.Page.Limit).Scan(&groupRows).Error; err != nil {
		return nil, 0, err
	}

	groupIDs := make([][]uint64, 0, len(groupRows))
	allIDs := make([]uint64, 0, len(groupRows)*2)
	for _, row := range groupRows {
		ids, err := parseModelRouteGroupIDs(row.RouteIDs)
		if err != nil {
			return nil, 0, err
		}
		groupIDs = append(groupIDs, ids)
		allIDs = append(allIDs, ids...)
	}
	if len(allIDs) == 0 {
		return []model.RouteGroup{}, total, nil
	}
	var rows []modelRouteModel
	if err := r.db.db.WithContext(ctx).Where("id IN ?", allIDs).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, 0, err
	}
	byID := make(map[uint64]model.Route, len(values))
	for _, value := range values {
		byID[value.ID] = value
	}
	groups := make([]model.RouteGroup, 0, len(groupIDs))
	for _, ids := range groupIDs {
		members := make([]model.Route, 0, len(ids))
		for _, id := range ids {
			if value, ok := byID[id]; ok {
				members = append(members, value)
			}
		}
		if len(members) == 0 {
			continue
		}
		groups = append(groups, model.RouteGroup{Routes: members})
	}
	return groups, total, nil
}

func parseModelRouteGroupIDs(value string) ([]uint64, error) {
	parts := strings.Split(strings.TrimSpace(value), ",")
	ids := make([]uint64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("模型能力组包含无效路由 ID %q", part)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids, nil
}

func (r *ModelRepository) ListEnabled(ctx context.Context) ([]model.Route, error) {
	var rows []modelRouteModel
	if err := r.availableRoutes(r.db.db.WithContext(ctx)).Where("enabled = ?", true).Order("public_id ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, err
	}
	return values, nil
}

// ListEnabledForScope skips annotateAvailability: the public model list needs
// only route identity, and the account counts dominated its cost.
func (r *ModelRepository) ListEnabledForScope(ctx context.Context, filter repository.ModelListFilter) ([]model.Route, error) {
	query := r.availableRoutes(r.db.db.WithContext(ctx)).Where("enabled = ?", true)
	if len(filter.Providers) > 0 {
		query = query.Where("provider IN ?", filter.Providers)
	}
	if tierPredicate := modelTierAvailabilityPredicateWithAvailability(filter.Tiers, filter.TierExemptProviders, true); tierPredicate != "" {
		query = query.Where(tierPredicate)
	}
	var rows []modelRouteModel
	if err := query.Order("public_id ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return mapModelRows(rows), nil
}

// ListConfiguredEnabled 返回所有已启用配置，包括暂时没有可用账号的路由，供 readiness 展示部分故障。
func (r *ModelRepository) ListConfiguredEnabled(ctx context.Context) ([]model.Route, error) {
	var rows []modelRouteModel
	if err := r.db.db.WithContext(ctx).Where("enabled = ?", true).Order("public_id ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := mapModelRows(rows)
	if err := r.annotateAvailability(ctx, values); err != nil {
		return nil, err
	}
	return values, nil
}
