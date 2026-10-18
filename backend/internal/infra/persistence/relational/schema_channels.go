package relational

import (
	"context"
	"strings"
)

// Remove only the historical business enums. Identity, FK and value constraints
// remain intact. The old migration paths must never recreate these checks.
func (d *Database) dropChannelEnumConstraints(ctx context.Context) error {
	constraints := []consoleConstraint{
		{&accountModel{}, "provider_accounts", "chk_accounts_provider"},
		{&modelRouteModel{}, "model_routes", "chk_model_routes_provider"},
		{&requestAuditModel{}, "request_audits", "chk_request_audits_provider"},
		{&requestAuditModel{}, "request_audits", "chk_request_audits_egress_scope"},
		{&responseOwnershipModel{}, "response_ownership", "chk_response_ownership_provider"},
		{&egressNodeModel{}, "egress_nodes", "chk_egress_nodes_specific_scope"},
		{&egressSubscriptionSourceModel{}, "egress_subscription_sources", "chk_egress_subscription_sources_scope"},
		{&mediaJobModel{}, "media_jobs", "chk_media_jobs_provider"},
		{&mediaJobModel{}, "media_jobs", "chk_media_jobs_egress_scope"},
		{&accountCredentialModel{}, "account_credentials", "chk_account_credentials_auth_type"},
	}
	apply := func() error {
		for _, c := range constraints {
			if d.db.WithContext(ctx).Migrator().HasConstraint(c.model, c.name) {
				if err := d.db.WithContext(ctx).Migrator().DropConstraint(c.model, c.name); err != nil {
					return err
				}
			}
		}

		c := consoleConstraint{&accountCredentialModel{}, "account_credentials", "chk_account_credentials_secret"}
		definition, err := d.constraintDefinition(ctx, c)
		if err != nil {
			return err
		}
		if strings.Contains(definition, "auth_type") {
			if err := d.db.WithContext(ctx).Migrator().DropConstraint(c.model, c.name); err != nil {
				return err
			}
			return d.db.WithContext(ctx).Migrator().CreateConstraint(c.model, c.name)
		}
		return nil
	}
	if d.dialect == "sqlite" {
		if err := d.withSQLiteForeignKeysDisabled(ctx, apply); err != nil {
			return err
		}
	} else if err := apply(); err != nil {
		return err
	}
	return nil
}
