package store

import (
	"strings"

	"gorm.io/gorm"
)

// repairLegacyColumnAndIndexLeftovers is migration 207. Three released
// migrations only half-finished on databases older than themselves, and the
// leftovers stayed on every later version:
//
//   - 74 renames deployments.state to desired_state, but on a database that
//     still had state, 50 had already run AutoMigrate with the renamed field
//     and added an empty desired_state. 74 (and 163 after it) then only copied
//     the values across: state stayed, and idx_deployments_state, which
//     AutoMigrate matches by name, kept indexing it instead of desired_state.
//   - 70 replaced instances.run_spec_snapshot with image_defaults_snapshot
//     and left the old column; 112 dropped the replacement, not the original.
//     Nothing has read it since.
//   - 46 created idx_model_lookup on (source, source_id, revision). 71 added
//     artifact_subfolder to that index in the model, and AutoMigrate, again
//     matching by name, kept the three-column index.
//
// It is appended rather than folded into those migrations: they ran under
// their own names on these databases, so they are never replayed. Fresh
// databases have none of the leftovers; every step checks before changing.
func repairLegacyColumnAndIndexLeftovers(db *gorm.DB) error {
	if err := finishDeploymentStateRename(db); err != nil {
		return err
	}
	if err := dropColumns(db, "instances", "run_spec_snapshot"); err != nil {
		return err
	}
	return rebuildModelLookupIndex(db)
}

func finishDeploymentStateRename(db *gorm.DB) error {
	// Renames a database that never got there, otherwise refills empty
	// targets from the old column before it goes.
	if err := renameDeploymentStateColumn(db); err != nil {
		return err
	}
	if err := dropColumns(db, "deployments", "state"); err != nil {
		return err
	}
	return ensureCurrentIndex(db, &Deployment{}, "deployments", "idx_deployments_state", "desired_state")
}

func rebuildModelLookupIndex(db *gorm.DB) error {
	return ensureCurrentIndex(db, &Model{}, "models", "idx_model_lookup", "artifact_subfolder")
}

// ensureCurrentIndex recreates a model-declared index from the model when it
// is missing or does not cover column yet. A table without column predates
// the index's current shape and is left for the migration that adds it.
func ensureCurrentIndex(db *gorm.DB, model any, table, index, column string) error {
	present, err := tableHasColumn(db, table, column)
	if err != nil || !present {
		return err
	}
	definitions, err := indexDefinitions(db, table)
	if err != nil {
		return err
	}
	definition, exists := definitions[index]
	if exists && indexCoversColumn(definition, column) {
		return nil
	}
	if exists {
		if err := db.Exec(`DROP INDEX IF EXISTS "` + index + `"`).Error; err != nil {
			return err
		}
	}
	return db.Migrator().CreateIndex(model, index)
}

// indexCoversColumn reports whether a CREATE INDEX statement lists column.
func indexCoversColumn(definition, column string) bool {
	open := strings.Index(definition, "(")
	if open < 0 {
		return false
	}
	for _, part := range strings.Split(strings.TrimSuffix(strings.TrimSpace(definition[open+1:]), ")"), ",") {
		if strings.Trim(strings.TrimSpace(part), "`\"") == column {
			return true
		}
	}
	return false
}
