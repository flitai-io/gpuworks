package store

import "gorm.io/gorm"

// addNodePlacementPolicy adds the node placement policy's columns
// (nodepolicy.Policy, embedded as placement_<key>). Each is NOT NULL with the
// knob's default, so every existing node reads as before the knob existed:
// share_with_foreign off keeps partitions off cards held outside the
// platform unless an operator picks them (ADR 0068).
func addNodePlacementPolicy(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Node{}) {
		return nil
	}
	for _, column := range []string{"placement_share_with_foreign"} { // the knobs this version adds
		if db.Migrator().HasColumn(&Node{}, column) {
			continue
		}
		if err := db.Migrator().AddColumn(&Node{}, column); err != nil {
			return err
		}
	}
	return nil
}
