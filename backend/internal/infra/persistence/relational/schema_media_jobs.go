package relational

import (
	"context"
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

const mediaJobInputImageConstraint = "chk_media_jobs_input_image_count"

// updateMediaJobInputImageConstraint updates this one authorized image ceiling.
// AutoMigrate creates missing named CHECKs but does not update their
// expressions. Run inside initialization's PostgreSQL transaction / SQLite
// foreign-key guard so existing jobs and their dependents remain intact.
func (d *Database) updateMediaJobInputImageConstraint(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	if d.dialect == "postgres" {
		// Both the fifteen- and twenty-image CHECKs reference kind; inspect
		// their numeric ceiling instead of treating that dependency as current.
		var definition string
		if err := db.Raw(`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid = to_regclass('media_jobs') AND conname = ?`, mediaJobInputImageConstraint).Scan(&definition).Error; err != nil {
			return err
		}
		if strings.Contains(strings.Join(strings.Fields(definition), " "), fmt.Sprintf("input_image_count <= %d", media.MaxVideoInputImages)) {
			return nil
		}
		if err := db.Migrator().DropConstraint(&mediaJobModel{}, mediaJobInputImageConstraint); err != nil {
			return fmt.Errorf("替换媒体任务图片数量约束: %w", err)
		}
	} else {
		var ddl string
		if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'media_jobs'").Scan(&ddl).Error; err != nil {
			return err
		}
		if strings.Contains(strings.Join(strings.Fields(ddl), " "), fmt.Sprintf("input_image_count BETWEEN 0 AND %d", media.MaxVideoInputImages)) {
			return nil
		}
		// The SQLite migrator replaces an existing named CHECK in one table
		// rebuild; dropping it first would copy the same job data twice.
	}
	if err := db.Migrator().CreateConstraint(&mediaJobModel{}, mediaJobInputImageConstraint); err != nil {
		return fmt.Errorf("更新媒体任务图片数量约束: %w", err)
	}
	return nil
}
