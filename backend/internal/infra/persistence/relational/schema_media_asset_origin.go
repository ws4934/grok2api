package relational

import "context"

// mediaAssetOriginIndexes back the gallery provenance filters. The origin
// columns come from AutoMigrate with empty defaults, so older rows stay valid.
var mediaAssetOriginIndexes = []string{
	"CREATE INDEX IF NOT EXISTS idx_media_assets_provider_created ON media_assets(provider, created_at DESC, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_assets_client_key_created ON media_assets(client_key_id, created_at DESC, id)",
	"CREATE INDEX IF NOT EXISTS idx_media_assets_account_created ON media_assets(account_id, created_at DESC, id)",
}

// ensureMediaAssetOriginIndexes runs after media_assets constraint rebuilds,
// which drop SQLite indexes.
func (d *Database) ensureMediaAssetOriginIndexes(ctx context.Context) error {
	db := d.db.WithContext(ctx)
	for _, statement := range mediaAssetOriginIndexes {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
