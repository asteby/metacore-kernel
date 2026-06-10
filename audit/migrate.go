package audit

import "gorm.io/gorm"

// Migrate ensures the audit table exists with the Entry schema. It is safe to
// call repeatedly (GORM AutoMigrate is additive: it creates the table and adds
// missing columns/indexes, never drops). Pass an empty tableName to use
// DefaultTableName.
//
// Wire calls this automatically; a host that prefers to own migration timing
// can call Migrate explicitly at boot and pass the already-migrated table to
// Wire (the second AutoMigrate is then a cheap no-op).
func Migrate(db *gorm.DB, tableName string) error {
	if tableName == "" {
		tableName = DefaultTableName
	}
	return db.Table(tableName).AutoMigrate(&Entry{})
}
