package store

import "gorm.io/gorm"

// Existing transfer/archive sizes cannot backfill a runtime measurement.
func addImageNodeRuntimeSize(db *gorm.DB) error {
	if !db.Migrator().HasTable(&ImageNode{}) || db.Migrator().HasColumn(&ImageNode{}, "RuntimeSizeBytes") {
		return nil
	}
	return db.Migrator().AddColumn(&ImageNode{}, "RuntimeSizeBytes")
}

// Legacy rows carry no measured registry download size.
func addImageDownloadBytes(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Image{}) || db.Migrator().HasColumn(&Image{}, "DownloadBytes") {
		return nil
	}
	return db.Migrator().AddColumn(&Image{}, "DownloadBytes")
}

// Existing official rows get their selection policy when the catalog is seeded.
func addImageManualOnly(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Image{}) || db.Migrator().HasColumn(&Image{}, "ManualOnly") {
		return nil
	}
	return db.Migrator().AddColumn(&Image{}, "ManualOnly")
}
