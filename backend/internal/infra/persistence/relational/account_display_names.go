package relational

import (
	"context"
	"strings"

	"gorm.io/gorm"
)

// Direct links only; the order matches attachAccountLinks and the account page.
const accountIdentityEmailSQL = `COALESCE(NULLIF(TRIM(identity.email), ''),
 (SELECT NULLIF(TRIM(peer.email), '') FROM account_provider_links link JOIN provider_accounts peer ON peer.id=link.build_account_id WHERE link.web_account_id=identity.id),
 (SELECT NULLIF(TRIM(peer.email), '') FROM account_provider_links link JOIN provider_accounts peer ON peer.id=link.web_account_id WHERE link.build_account_id=identity.id),
 (SELECT NULLIF(TRIM(peer.email), '') FROM web_console_account_links link JOIN provider_accounts peer ON peer.id=link.console_account_id WHERE link.web_account_id=identity.id),
 (SELECT NULLIF(TRIM(peer.email), '') FROM web_console_account_links link JOIN provider_accounts peer ON peer.id=link.web_account_id WHERE link.console_account_id=identity.id), '')`

// SQL counterpart of Credential.DisplayName, also used for table ordering.
// TRIM(text, chars) is supported by both SQLite and PostgreSQL.
const generatedAccountNameSQL = `(
 (identity.provider='grok_web' AND SUBSTR(TRIM(identity.name),1,9)='Grok Web ' AND LENGTH(TRIM(identity.name))=17 AND LENGTH(TRIM(LOWER(SUBSTR(TRIM(identity.name),10)),'0123456789abcdef'))=0)
 OR (identity.provider='grok_console' AND SUBSTR(TRIM(identity.name),1,13)='Grok Console ' AND LENGTH(TRIM(identity.name))=21 AND LENGTH(TRIM(LOWER(SUBSTR(TRIM(identity.name),14)),'0123456789abcdef'))=0)
 OR (SUBSTR(TRIM(identity.name),1,LENGTH(identity.provider)+1)=identity.provider||'-' AND (
  (LENGTH(TRIM(identity.name))=LENGTH(identity.provider)+9 AND LENGTH(TRIM(LOWER(SUBSTR(TRIM(identity.name),LENGTH(identity.provider)+2)),'0123456789abcdef'))=0)
  OR (TRIM(COALESCE(identity.user_id,''))<>'' AND SUBSTR(TRIM(identity.name),LENGTH(identity.provider)+2)=SUBSTR(TRIM(COALESCE(identity.user_id,'')),1,8))
 )))`
const accountDisplayNameSQL = `CASE WHEN TRIM(identity.name)<>'' AND NOT ` + generatedAccountNameSQL + ` THEN TRIM(identity.name) ELSE COALESCE(NULLIF(` + accountIdentityEmailSQL + `,''), NULLIF(TRIM(identity.name),''),NULLIF(TRIM(COALESCE(identity.user_id,'')),''),identity.provider) END`

func mediaAccountNameSortSQL() string {
	return "LOWER(COALESCE((SELECT " + accountDisplayNameSQL + " FROM provider_accounts identity WHERE identity.id=media_jobs.account_id),media_jobs.account_name))"
}

// DisplayNames is a metadata-only, bounded lookup for admin read views. It never
// loads/decrypts credentials or rewrites historical audit/job snapshots.
func (r *AccountRepository) DisplayNames(ctx context.Context, ids []uint64) (map[uint64]string, error) {
	names := make(map[uint64]string)
	unique := make([]uint64, 0, len(ids))
	seen := map[uint64]bool{}
	for _, id := range ids {
		if id > 0 && !seen[id] {
			unique = append(unique, id)
			seen[id] = true
		}
	}
	for start := 0; start < len(unique); start += 500 {
		var rows []struct {
			ID   uint64
			Name string
		}
		err := r.db.db.WithContext(ctx).Table("provider_accounts AS identity").Select("identity.id,"+accountDisplayNameSQL+" AS name").Where("identity.id IN ?", unique[start:min(start+500, len(unique))]).Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			names[row.ID] = row.Name
		}
	}
	return names, nil
}

// accountIdentitySearchIDs lets old snapshots remain searchable by the email
// currently shown, without reading the entire account pool into memory.
func accountIdentitySearchIDs(db *gorm.DB, pattern string) *gorm.DB {
	return db.Session(&gorm.Session{NewDB: true}).Table("provider_accounts AS identity").Select("identity.id").Where("LOWER(identity.name) LIKE ? OR LOWER("+accountIdentityEmailSQL+") LIKE ?", pattern, pattern)
}

func accountNameSortSQL() string {
	return "LOWER(" + strings.ReplaceAll(accountDisplayNameSQL, "identity.", "provider_accounts.") + ")"
}
