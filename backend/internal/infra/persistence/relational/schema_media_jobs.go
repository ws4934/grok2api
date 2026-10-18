package relational

import "context"

// Unique per client key only when the client sent an Idempotency-Key.
const mediaJobIdempotencyIndex = "CREATE UNIQUE INDEX IF NOT EXISTS uidx_media_jobs_idempotency ON media_jobs(client_key_id, idempotency_key) WHERE idempotency_key <> ''"

// ensureMediaJobKindConstraints upgrades the video-only CHECKs once
// AutoMigrate has added kind (default 'video'): operation and seconds become
// kind-aware and the status set gains canceled/uncertain. Existing rows are
// video jobs and satisfy the new definitions unchanged; SQLite rebuilds the
// table, PostgreSQL replaces the constraints in place.
func (d *Database) ensureMediaJobKindConstraints(ctx context.Context) error {
	for _, constraint := range []struct{ name, marker string }{
		{"chk_media_jobs_operation", "kind"},
		{"chk_media_jobs_seconds", "kind"},
		{"chk_media_jobs_status", "uncertain"},
	} {
		if err := d.ensureNamedConstraints(ctx, []consoleConstraint{
			{model: &mediaJobModel{}, table: "media_jobs", name: constraint.name},
		}, constraint.marker); err != nil {
			return err
		}
	}
	return nil
}
