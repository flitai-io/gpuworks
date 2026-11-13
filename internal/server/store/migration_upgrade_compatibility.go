package store

import (
	"time"

	"gorm.io/gorm"
)

// repairableLegacyMigration lists schema changes whose numbers were occupied
// by other historical branches. Replaying arbitrary renamed migrations could
// repeat destructive cutovers, so the repair is deliberately bounded to
// changes that converge on the current entities when run again.
//
// Versions 37, 38 and 40 were recorded by the benchmark branch as
// benchmark_runs_points_baselines, drop_legacy_jobs and benchmark_run_num_gpus
// (it never recorded 39). Those databases skipped the image migrations at the
// same numbers and kept an image_series table without common_args/common_env,
// which the image catalog writes on startup. 37 only adds columns; 38 only
// drops verification columns the current Image entity no longer declares and
// checks each column first. Both must run in place, before 39 inserts split
// series rows with every current ImageSeries column; the benchmark schema
// itself is reconciled by 41 and 42.
func repairableLegacyMigration(m migration) bool {
	switch m.Version {
	case 27:
		return m.Name == "terminal_sessions"
	case 28:
		return m.Name == "deployment_dgpu_quota_fields"
	case 37:
		return m.Name == "image_catalog_m20_fields"
	case 38:
		return m.Name == "remove_image_id_verification_state"
	case 40:
		return m.Name == "instance_image_digest_snapshot"
	}
	return false
}

func repairLegacyMigration(db *gorm.DB, m migration) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return tx.Model(&SchemaMigration{}).Where("version = ?", m.Version).
			Updates(map[string]any{"name": m.Name, "applied_at": time.Now().UTC()}).Error
	})
}

// reconcileLegacyResourceValues also repairs databases that already recorded
// the original migrations after AutoMigrate had introduced their columns.
func reconcileLegacyResourceValues(db *gorm.DB) error {
	if db.Migrator().HasColumn(&Instance{}, "Role") {
		if err := db.Exec("UPDATE instances SET role = '' WHERE role IS NULL").Error; err != nil {
			return err
		}
	}
	return renameDeploymentStateColumn(db)
}
