package store

import "gorm.io/gorm"

// addProviderSyncedAt adds when an account's instances were last listed
// (Provider.SyncedAt). Existing accounts start without one: the time of their
// last sync was never recorded, and their next sync sets it.
func addProviderSyncedAt(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Provider{}) || db.Migrator().HasColumn(&Provider{}, "SyncedAt") {
		return nil
	}
	return db.Migrator().AddColumn(&Provider{}, "SyncedAt")
}
