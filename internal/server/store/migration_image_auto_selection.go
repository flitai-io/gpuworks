package store

import "gorm.io/gorm"

// Earlier review builds recorded migration 249 with either manual_only or
// auto_select. Use a new marker and preserve an already explicit policy.
func migrateImageAutoSelection(db *gorm.DB) error {
	m := db.Migrator()
	if !m.HasTable(&Image{}) {
		return nil
	}
	if !m.HasColumn(&Image{}, "AutoSelect") {
		if err := m.AddColumn(&Image{}, "AutoSelect"); err != nil {
			return err
		}
	}
	if m.HasColumn("image_versions", "manual_only") {
		return db.Exec("UPDATE image_versions SET auto_select = 'manual' WHERE manual_only = TRUE AND COALESCE(auto_select, '') = ''").Error
	}
	return nil
}
