package store

import "gorm.io/gorm"

// Older catalogs have no measured registry layer sizes to backfill.
func addImageDownloadBytes(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Image{}) || db.Migrator().HasColumn(&Image{}, "DownloadBytes") {
		return nil
	}
	return db.Migrator().AddColumn(&Image{}, "DownloadBytes")
}
