package store

import "gorm.io/gorm"

// addNodeLastRecoveryCode adds the code that names a node's last
// out-of-band recovery error. Rows recorded before it keep an empty code and
// their error text, which clients show as written.
func addNodeLastRecoveryCode(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Node{}) || db.Migrator().HasColumn(&Node{}, "LastRecoveryCode") {
		return nil
	}
	return db.Migrator().AddColumn(&Node{}, "LastRecoveryCode")
}
