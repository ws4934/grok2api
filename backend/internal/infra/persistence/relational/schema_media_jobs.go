package relational

import (
	"context"
	"fmt"
	"strings"
)

const mediaJobInputImageConstraint = "chk_media_jobs_input_image_count"

// updateMediaJobInputImageConstraint replaces the former shared eight-image
// ceiling. AutoMigrate creates missing named CHECKs but does not update their
// expressions. Run inside initialization's PostgreSQL transaction / SQLite
// foreign-key guard so existing jobs and their dependents remain intact.
func (d *Database) updateMediaJobInputImageConstraint(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	if d.dialect == "postgres" {
		// The current CHECK references kind; the former shared limit did not.
		// Catalog dependencies avoid comparing PostgreSQL's reformatted SQL.
		var current bool
		if err := db.Raw(`SELECT EXISTS (
			SELECT 1 FROM pg_constraint c JOIN pg_attribute a
			ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
			WHERE c.conrelid = to_regclass('media_jobs') AND c.conname = ? AND a.attname = 'kind'
		)`, mediaJobInputImageConstraint).Scan(&current).Error; err != nil {
			return err
		}
		if current {
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
		if !strings.Contains(strings.Join(strings.Fields(ddl), " "), "input_image_count BETWEEN 0 AND 8") {
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
