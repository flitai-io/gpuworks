package store

import "gorm.io/gorm"

func addInstanceRuntimeFailure(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Instance{}) || db.Migrator().HasColumn(&Instance{}, "RuntimeFailure") {
		return nil
	}
	return db.Migrator().AddColumn(&Instance{}, "RuntimeFailure")
}
