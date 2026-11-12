package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

// SchemaMigration records an applied application-schema migration.
type SchemaMigration struct {
	Version   int    `gorm:"primaryKey"`
	Name      string `gorm:"size:128"`
	AppliedAt time.Time
}

type migration struct {
	Version int
	Name    string
	Apply   func(*gorm.DB) error
}

func migratePartitionCapabilityFramework(db *gorm.DB) error {
	if db.Migrator().HasTable(&Node{}) {
		if err := autoMigratePreservingGPUIndexes(db, &Node{}); err != nil {
			return err
		}
	}
	if db.Migrator().HasTable(&Deployment{}) {
		if err := db.AutoMigrate(&Deployment{}); err != nil {
			return err
		}
		if err := db.Exec("UPDATE deployments SET partition_backend = ? WHERE v_ram_limit_bytes > 0 AND (partition_backend IS NULL OR trim(partition_backend) = '')", "nvidia-dgpu").Error; err != nil {
			return err
		}
	}
	// partition_mode was an independently-derived claim at both node and GPU
	// scope. The per-GPU capability set replaces it completely. Use raw DROP
	// COLUMN because the removed fields are intentionally absent from the GORM
	// models and therefore cannot be addressed through model-field migration.
	for _, item := range []struct {
		table  string
		column string
	}{
		{"nodes", "partition_mode"}, {"gpus", "partition_mode"},
	} {
		if db.Migrator().HasTable(item.table) && db.Migrator().HasColumn(item.table, item.column) {
			if err := db.Exec("ALTER TABLE " + item.table + " DROP COLUMN " + item.column).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func renameDeploymentStateColumn(db *gorm.DB) error {
	if !db.Migrator().HasTable("deployments") {
		return nil
	}
	// Some historical migration fixtures create only the columns they exercise.
	// They do not represent a production deployments schema and are left alone.
	if !db.Migrator().HasColumn("deployments", "state") {
		return nil
	}
	if db.Migrator().HasColumn("deployments", "desired_state") {
		// Earlier AutoMigrate calls can add the new column before this rename.
		// Preserve explicit new targets while recovering the old stopped state.
		return db.Exec(`UPDATE deployments SET desired_state = state
			WHERE (desired_state IS NULL OR trim(desired_state) = '')
			AND state IS NOT NULL AND trim(state) <> ''`).Error
	}
	return db.Exec(`ALTER TABLE "deployments" RENAME COLUMN "state" TO "desired_state"`).Error
}

var migrations = []migration{
	{
		Version: 1,
		Name:    "baseline",
		Apply: func(db *gorm.DB) error {
			// The baseline is intentionally explicit but uses GORM's idempotent
			// table/column creation for legacy v1 databases.
			return autoMigratePreservingGPUIndexes(db,
				&Node{}, &GPU{}, &User{}, &APIKey{},
				&Model{}, &ModelFile{}, &Deployment{}, &Instance{}, &InstanceEvent{},
				&Provider{}, &ProviderInstance{}, &ProviderBootstrapToken{}, &Setting{}, &AIConfig{}, &legacyJob{}, &Profile{},
			)
		},
	},
	{
		Version: 2,
		Name:    "gpu_vendor_arch_id",
		Apply: func(db *gorm.DB) error {
			return autoMigratePreservingGPUIndexes(db, &GPU{})
		},
	},
	{
		Version: 3,
		Name:    "node_gpu_vendor_driver",
		Apply: func(db *gorm.DB) error {
			return autoMigratePreservingGPUIndexes(db, &Node{})
		},
	},
	{
		Version: 4,
		Name:    "gpu_partition_mode",
		Apply: func(db *gorm.DB) error {
			return autoMigratePreservingGPUIndexes(db, &GPU{})
		},
	},
	{
		Version: 5,
		Name:    "node_capabilities",
		Apply: func(db *gorm.DB) error {
			return autoMigratePreservingGPUIndexes(db, &Node{})
		},
	},
	{
		Version: 6,
		Name:    "provider_bootstrap_tokens",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&ProviderBootstrapToken{})
		},
	},
	{
		Version: 7,
		Name:    "measured_profiles",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Profile{})
		},
	},
	{
		Version: 8,
		Name:    "profile_args_hash",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Profile{})
		},
	},
	{
		Version: 9,
		Name:    "deployment_distributed_fields",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Deployment{})
		},
	},
	{
		Version: 10,
		Name:    "deployment_node_ids",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Deployment{})
		},
	},
	{
		Version: 11,
		Name:    "instance_distributed_fields",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Instance{})
		},
	},
	{
		Version: 12,
		Name:    "distributed_group_attempts",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Deployment{}, &Instance{})
		},
	},
	{
		Version: 13,
		Name:    "job_workload_specs",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&legacyJob{})
		},
	},
	{
		Version: 14,
		Name:    "job_message_capacity",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&legacyJob{})
		},
	},
	{
		Version: 15,
		Name:    "model_engine_support",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Model{})
		},
	},
	{
		Version: 16,
		Name:    "model_file_acquisition",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&ModelFile{})
		},
	},
	{
		Version: 17,
		Name:    "model_file_transferred_bytes",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&ModelFile{})
		},
	},
	{
		Version: 18,
		Name:    "gpu_health_details",
		Apply: func(db *gorm.DB) error {
			return autoMigratePreservingGPUIndexes(db, &GPU{})
		},
	},
	{
		Version: 19,
		Name:    "image_catalog_storage_and_model_meta",
		Apply: func(db *gorm.DB) error {
			return autoMigratePreservingGPUIndexes(db, &Image{}, &ImageNode{}, &Storage{}, &Node{}, &Model{})
		},
	},
	{
		Version: 20,
		Name:    "image_offline_blob_metadata",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Image{})
		},
	},
	{
		Version: 21,
		Name:    "node_server_dial_address",
		Apply: func(db *gorm.DB) error {
			return autoMigratePreservingGPUIndexes(db, &Node{})
		},
	},
	{
		Version: 22,
		Name:    "model_capability_override",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Model{})
		},
	},
	{
		Version: 23,
		Name:    "image_series_versions_rebuild",
		Apply: func(db *gorm.DB) error {
			// The series/versions rebuild intentionally does not migrate the
			// old flat catalog. Clear model associations first, then replace
			// both catalog tables.
			if err := db.Model(&Model{}).Where("image_id <> 0").Update("image_id", 0).Error; err != nil {
				return err
			}
			if err := db.Exec("DROP TABLE IF EXISTS image_nodes").Error; err != nil {
				return err
			}
			if err := db.Exec("DROP TABLE IF EXISTS images").Error; err != nil {
				return err
			}
			if err := db.Exec("DROP TABLE IF EXISTS image_versions").Error; err != nil {
				return err
			}
			return db.AutoMigrate(&ImageSeries{}, &Image{}, &ImageNode{})
		},
	},
	{
		Version: 24,
		Name:    "image_catalog_cleanup_final",
		Apply: func(db *gorm.DB) error {
			// The image migrations were authored as versions 22/23 before the
			// upstream model-capability migration claimed version 22. Databases
			// that ran an intermediate build may therefore have either image
			// migration recorded under an old number. This final cleanup makes
			// the no-compatibility policy deterministic, while also ensuring the
			// upstream Model schema change is present on those databases.
			if err := db.Model(&Model{}).Where("image_id <> 0").Update("image_id", 0).Error; err != nil {
				return err
			}
			for _, table := range []string{"image_nodes", "images", "image_versions"} {
				if err := db.Exec("DROP TABLE IF EXISTS " + table).Error; err != nil {
					return err
				}
			}
			return db.AutoMigrate(&Model{}, &ImageSeries{}, &Image{}, &ImageNode{})
		},
	},
	{
		Version: 25,
		Name:    "gpu_power_limit_watts",
		Apply: func(db *gorm.DB) error {
			return autoMigratePreservingGPUIndexes(db, &GPU{})
		},
	},
	{
		Version: 26,
		Name:    "image_node_source",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&ImageNode{})
		},
	},
	{
		Version: 27,
		Name:    "terminal_sessions",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&TerminalSession{})
		},
	},
	{
		Version: 28,
		Name:    "deployment_dgpu_quota_fields",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Deployment{})
		},
	},
	{
		Version: 29,
		Name:    "node_agent_management",
		Apply: func(db *gorm.DB) error {
			return autoMigratePreservingGPUIndexes(db, &Node{})
		},
	},
	{
		Version: 30,
		Name:    "node_agent_management_reconcile",
		Apply: func(db *gorm.DB) error {
			// Migration 28 was also used by an abandoned benchmark branch for a
			// different schema change. Some live databases therefore record
			// version 28 as already applied while still lacking the agent
			// management columns. Re-running both model migrations here is
			// idempotent and reconciles either lineage without touching unrelated
			// legacy columns.
			if err := autoMigratePreservingGPUIndexes(db, &Node{}); err != nil {
				return err
			}
			return db.AutoMigrate(&Deployment{})
		},
	},
	{
		Version: 31,
		Name:    "instance_startup_progress",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&Instance{})
		},
	},
	{
		Version: 32,
		Name:    "image_catalog_v2_runspec_verification",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&ImageSeries{}, &Image{}, &Instance{})
		},
	},
	{
		Version: 33,
		Name:    "image_node_inspection_id",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&ImageNode{}) },
	},
	{
		Version: 34,
		Name:    "image_source_custom_normalization",
		Apply: func(db *gorm.DB) error {
			if err := db.Exec("UPDATE image_series SET source = 'custom' WHERE source IS NULL OR trim(source) = ''").Error; err != nil {
				return err
			}
			if err := db.Exec("UPDATE image_versions SET source = 'custom' WHERE source IS NULL OR trim(source) = ''").Error; err != nil {
				return err
			}
			return normalizeBuiltinImageSources(db)
		},
	},
	{
		Version: 35,
		Name:    "deployment_runspec_overrides",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&Deployment{}) },
	},
	{
		Version: 36,
		Name:    "image_runspec_inspect_metadata",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&Image{}) },
	},
	{
		Version: 37,
		Name:    "image_catalog_m20_fields",
		Apply: func(db *gorm.DB) error {
			return db.AutoMigrate(&ImageSeries{}, &Image{})
		},
	},
	{
		Version: 38,
		Name:    "remove_image_id_verification_state",
		Apply: func(db *gorm.DB) error {
			// SQLite cannot drop an indexed column before its generated GORM
			// index is removed.
			for _, index := range []string{"idx_image_versions_verify_state", "idx_images_verify_state"} {
				if err := db.Exec("DROP INDEX IF EXISTS " + index).Error; err != nil {
					return err
				}
			}
			for _, column := range []string{"expected_image_id", "verify_state", "verify_message"} {
				var present int
				if err := db.Raw("SELECT count(*) FROM pragma_table_info(?) WHERE name = ?", "image_versions", column).Scan(&present).Error; err != nil {
					return err
				}
				if present > 0 {
					if err := db.Exec("ALTER TABLE image_versions DROP COLUMN " + column).Error; err != nil {
						return err
					}
				}
			}
			return nil
		},
	},
	{
		Version: 39,
		Name:    "split_mixed_framework_image_series",
		Apply:   splitMixedFrameworkSeries,
	},
	{
		Version: 40,
		Name:    "instance_image_digest_snapshot",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&Instance{}) },
	},
	{
		Version: 41,
		Name:    "benchmark_schema_reconcile",
		Apply: func(db *gorm.DB) error {
			// Versions 37-40 are occupied by image migrations in some deployed
			// databases, so benchmark schema starts at 41.
			return db.AutoMigrate(&BenchmarkRun{}, &BenchmarkPoint{})
		},
	},
	{
		Version: 42,
		Name:    "benchmark_legacy_cleanup_reconcile",
		Apply: func(db *gorm.DB) error {
			// Version 38 was already occupied by an image migration on some
			// deployed databases, so its jobs cleanup was skipped there. Recheck
			// the benchmark schema as well to keep this migration self-contained.
			if err := db.AutoMigrate(&BenchmarkRun{}, &BenchmarkPoint{}); err != nil {
				return err
			}
			return db.Exec("DROP TABLE IF EXISTS jobs").Error
		},
	},
	{
		Version: 43,
		Name:    "modelscope_default_revision",
		Apply: func(db *gorm.DB) error {
			// Some intermediate schemas carried a model_files.revision column while
			// the current entity did not. AutoMigrate is idempotent for both lineages
			// and gives each node copy a place to record its resolved commit.
			if err := db.AutoMigrate(&ModelFile{}); err != nil {
				return err
			}
			// Compatibility migration tests also exercise sparse historical schemas
			// whose baseline version is recorded but whose models table never existed.
			if !db.Migrator().HasTable(&Model{}) {
				return nil
			}
			if err := db.Exec("UPDATE model_files SET revision = '' WHERE revision = 'main' AND model_id IN (SELECT id FROM models WHERE source = 'modelscope')").Error; err != nil {
				return err
			}
			return db.Exec("UPDATE models SET revision = '' WHERE source = 'modelscope' AND revision = 'main'").Error
		},
	},
	{
		Version: 44,
		Name:    "node_server_external_url",
		Apply:   func(db *gorm.DB) error { return autoMigratePreservingGPUIndexes(db, &Node{}) },
	},
	{
		Version: 45,
		Name:    "provider_instance_ssh_connections",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&ProviderInstance{}) },
	},
	{
		Version: 46,
		Name:    "model_source_id_casefold_identity",
		Apply: func(db *gorm.DB) error {
			if !db.Migrator().HasTable(&Model{}) {
				return nil
			}
			if err := mergeDuplicateModels(db); err != nil {
				return err
			}
			if err := db.Exec("DROP INDEX IF EXISTS idx_model_src").Error; err != nil {
				return err
			}
			if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_model_lookup ON models(source, source_id, revision)").Error; err != nil {
				return err
			}
			return db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_model_source_id_casefold ON models(lower(source), lower(source_id), revision)").Error
		},
	},
	{
		Version: 47,
		Name:    "image_registry_verification",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&Image{}) },
	},
	{Version: 48, Name: "retired_template_catalog", Apply: func(*gorm.DB) error { return nil }},
	{Version: 49, Name: "retired_deployment_provenance", Apply: func(*gorm.DB) error { return nil }},
	{
		Version: 50,
		Name:    "partition_capability_framework",
		Apply:   migratePartitionCapabilityFramework,
	},
	{
		Version: 51,
		Name:    "node_out_of_band_recovery",
		Apply: func(db *gorm.DB) error {
			// Re-running the node/GPU models is idempotent and keeps status
			// updates working while adding the recovery schema.
			return autoMigratePreservingGPUIndexes(db, &Node{}, &GPU{}, &NodeSSHCredential{})
		},
	},
	{Version: 52, Name: "retired_template_metadata", Apply: func(*gorm.DB) error { return nil }},
	{
		Version: 53,
		Name:    "benchmark_verdict",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&BenchmarkRun{}) },
	},
	{
		Version: 54,
		Name:    "benchmark_point_peak_throughput",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&BenchmarkPoint{}) },
	},
	{
		Version: 55,
		Name:    "normalize_stored_times_to_utc",
		Apply:   normalizeStoredTimesToUTC,
	},
	{
		Version: 56,
		Name:    "instance_event_codes",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&InstanceEvent{}) },
	},
	{
		// Versions 51-56 are already occupied on the master lineage. Reconcile
		// the probe column at a new global migration number so databases that
		// previously applied partition_capability_framework as v50 cannot skip it.
		Version: 57,
		Name:    "node_partition_probes_reconcile",
		Apply:   migratePartitionCapabilityFramework,
	},
	{
		Version: 58,
		Name:    "benchmark_curve_and_target_snapshot",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&BenchmarkRun{}, &BenchmarkPoint{}) },
	},
	{
		Version: 59,
		Name:    "remove_benchmark_baselines",
		Apply:   func(db *gorm.DB) error { return db.Exec("DROP TABLE IF EXISTS benchmark_baselines").Error },
	},
	{
		Version: 66,
		Name:    "flat_inference_spec_catalog",
		Apply:   migrateFlatInferenceCatalog,
	},
	{
		Version: 67,
		Name:    "inference_model_kinds",
		// Retired projection. Keep the version in the ledger so existing
		// databases do not replay it; v83 removes the historical column.
		Apply: func(*gorm.DB) error { return nil },
	},
	{
		Version: 68,
		Name:    "inference_spec_display_profile",
		Apply: func(db *gorm.DB) error {
			if db.Migrator().HasColumn(&inferenceSpecDisplayProfileColumn{}, "DisplayProfile") {
				return nil
			}
			return db.Migrator().AddColumn(&inferenceSpecDisplayProfileColumn{}, "DisplayProfile")
		},
	},
	{
		Version: 69,
		Name:    "executable_inference_catalog_contracts",
		Apply:   migrateInferenceCatalogContracts,
	},
	{
		Version: 70,
		Name:    "instance_desired_state_launch_snapshot",
		Apply:   func(db *gorm.DB) error { return db.AutoMigrate(&Instance{}) },
	},
	{
		Version: 71,
		Name:    "model_artifact_subfolder_identity",
		Apply: func(db *gorm.DB) error {
			if !db.Migrator().HasTable(&Model{}) {
				return nil
			}
			if err := db.AutoMigrate(&Model{}); err != nil {
				return err
			}
			if err := db.Exec("UPDATE models SET artifact_subfolder = '' WHERE artifact_subfolder IS NULL").Error; err != nil {
				return err
			}
			if err := db.Exec("DROP INDEX IF EXISTS idx_model_source_id_casefold").Error; err != nil {
				return err
			}
			return db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_model_source_id_casefold ON models(lower(source), lower(source_id), revision, artifact_subfolder)").Error
		},
	},
	{
		Version: 72,
		Name:    "inference_spec_source_identity",
		Apply:   migrateInferenceSpecSourceIdentity,
	},
	{
		Version: 73,
		Name:    "inference_spec_runtime_weight_bytes",
		Apply: func(db *gorm.DB) error {
			if !db.Migrator().HasTable(&InferenceSpecRow{}) || db.Migrator().HasColumn(&InferenceSpecRow{}, "RuntimeWeightBytes") {
				return nil
			}
			return db.Migrator().AddColumn(&InferenceSpecRow{}, "RuntimeWeightBytes")
		},
	},
	{
		Version: 74,
		Name:    "deployment_desired_state_column",
		Apply:   renameDeploymentStateColumn,
	},
	{
		Version: 75,
		Name:    "inference_model_input_modalities",
		Apply: func(db *gorm.DB) error {
			if !db.Migrator().HasColumn(&InferenceModel{}, "InputModalities") {
				return db.Migrator().AddColumn(&InferenceModel{}, "InputModalities")
			}
			return nil
		},
	},
	{Version: 76, Name: "modelfile_task_proxy", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&ModelFile{}) {
			return nil
		}
		if !db.Migrator().HasColumn(&modelFileNetworkColumns{}, "Proxy") {
			return db.Migrator().AddColumn(&modelFileNetworkColumns{}, "Proxy")
		}
		return nil
	}},
	{Version: 77, Name: "modelfile_task_network", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&ModelFile{}) {
			return nil
		}
		for _, field := range []string{"Endpoint", "NetworkExplicit"} {
			if !db.Migrator().HasColumn(&modelFileNetworkColumns{}, field) {
				if err := db.Migrator().AddColumn(&modelFileNetworkColumns{}, field); err != nil {
					return err
				}
			}
		}
		return nil
	}},
	{Version: 78, Name: "drop_default_model_source_setting", Apply: func(db *gorm.DB) error {
		// Acquisition is now an explicit per-deployment choice. A lingering
		// global preference would silently re-enter template resolution.
		if !db.Migrator().HasTable(&Setting{}) {
			return nil
		}
		return db.Where("key = ?", "default_model_source").Delete(&Setting{}).Error
	}},
	{Version: 79, Name: "model_endpoint_to_network_rewrite", Apply: migrateModelEndpointToNetworkRewrite},
	{Version: 80, Name: "model_inference_config", Apply: migrateModelInferenceConfig},
	{Version: 81, Name: "drop_model_engine_support", Apply: dropModelEngineSupport},
	{Version: 82, Name: "drop_model_capability_override", Apply: dropModelCapabilityOverride},
	{Version: 83, Name: "drop_inference_model_kinds", Apply: dropInferenceModelKinds},
	{Version: 84, Name: "node_runtime_plugin", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Node{}) || db.Migrator().HasColumn(&Node{}, "runtime_plugin") {
			return nil
		}
		return db.Migrator().AddColumn(&Node{}, "runtime_plugin")
	}},
	{Version: 85, Name: "inference_model_model_config", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&InferenceModel{}) || db.Migrator().HasColumn(&InferenceModel{}, "ModelConfig") {
			return nil
		}
		return db.Migrator().AddColumn(&InferenceModel{}, "ModelConfig")
	}},
	// The catalog carries each model's KV cost per token from here on. A
	// database created before this version has no column for it, and the
	// baseline AutoMigrate has already run, so without this the projection
	// silently loses every value the seed catalog ships.
	{Version: 86, Name: "inference_model_kv_bytes_per_token", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&InferenceModel{}) || db.Migrator().HasColumn(&InferenceModel{}, "KVBytesPerToken") {
			return nil
		}
		return db.Migrator().AddColumn(&InferenceModel{}, "KVBytesPerToken")
	}},
	// Events outlive the instance and deployment rows they describe, so a row
	// has to be readable without joining back to either. Existing rows are
	// backfilled from the instance they still point at; rows whose instance is
	// already gone keep InstanceID as their only (opaque) link, which is the
	// best that can be recovered after the fact.
	{Version: 87, Name: "instance_event_self_identity", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&InstanceEvent{}) {
			return nil
		}
		if !db.Migrator().HasColumn(&InstanceEvent{}, "InstanceUUID") {
			if err := db.Migrator().AddColumn(&InstanceEvent{}, "InstanceUUID"); err != nil {
				return err
			}
		}
		if !db.Migrator().HasColumn(&InstanceEvent{}, "DeploymentID") {
			if err := db.Migrator().AddColumn(&InstanceEvent{}, "DeploymentID"); err != nil {
				return err
			}
		}
		// Indexes are created by AutoMigrate on the next open; adding them here
		// as well keeps a database that never re-runs AutoMigrate queryable.
		for _, idx := range []struct{ name, column string }{
			{"idx_instance_events_instance_uuid", "instance_uuid"},
			{"idx_instance_events_deployment_id", "deployment_id"},
		} {
			if !db.Migrator().HasIndex(&InstanceEvent{}, idx.name) {
				if err := db.Exec("CREATE INDEX IF NOT EXISTS " + idx.name +
					" ON instance_events (" + idx.column + ")").Error; err != nil {
					return err
				}
			}
		}
		if !db.Migrator().HasTable(&Instance{}) {
			return nil
		}
		if err := db.Exec(`UPDATE instance_events SET instance_uuid = COALESCE((
			SELECT uuid FROM instances WHERE instances.id = instance_events.instance_id), '')
			WHERE (instance_uuid IS NULL OR instance_uuid = '') AND instance_id > 0`).Error; err != nil {
			return err
		}
		return db.Exec(`UPDATE instance_events SET deployment_id = COALESCE((
			SELECT deployment_id FROM instances WHERE instances.id = instance_events.instance_id), 0)
			WHERE (deployment_id IS NULL OR deployment_id = 0) AND instance_id > 0`).Error
	}},
	{Version: 88, Name: "node_runtime_preference", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Node{}) || db.Migrator().HasColumn(&Node{}, "RuntimePreference") {
			return nil
		}
		return db.Migrator().AddColumn(&Node{}, "RuntimePreference")
	}},
	// A model imported from an HTTP directory index keeps the directory it was
	// registered from. Nothing else in the row can reproduce it: the source and
	// repository id only say which repository the weights are, not which file
	// server serves them.
	{Version: 89, Name: "model_source_url", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Model{}) || db.Migrator().HasColumn(&Model{}, "SourceURL") {
			return nil
		}
		return db.Migrator().AddColumn(&Model{}, "SourceURL")
	}},
	{Version: 90, Name: "drop_instance_injected_args", Apply: dropInstanceInjectedArgs},
	// The timeline is read newest-first, scoped to one instance or deployment,
	// and folds runs of identical milestones. All three walk the rows of one
	// scope in id order; the single-column indexes forced a sort of every row
	// in the scope first, which on a database still holding pre-fold progress
	// rows means tens of thousands of them per request.
	{Version: 91, Name: "instance_event_timeline_index", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&InstanceEvent{}) {
			return nil
		}
		for _, statement := range []string{
			"CREATE INDEX IF NOT EXISTS idx_instance_events_uuid_id ON instance_events (instance_uuid, id)",
			"CREATE INDEX IF NOT EXISTS idx_instance_events_deployment_id_id ON instance_events (deployment_id, id)",
		} {
			if err := db.Exec(statement).Error; err != nil {
				return err
			}
		}
		return nil
	}},
	{Version: 92, Name: "instance_runtime_observation", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Instance{}) {
			return nil
		}
		for _, field := range []string{"RuntimeObservation", "RuntimeObservationReceivedAt"} {
			if !db.Migrator().HasColumn(&Instance{}, field) {
				if err := db.Migrator().AddColumn(&Instance{}, field); err != nil {
					return err
				}
			}
		}
		return nil
	}},
	{Version: 93, Name: "node_partition_safety_protocol", Apply: func(db *gorm.DB) error {
		return autoMigratePreservingGPUIndexes(db, &Node{})
	}},
	{Version: 94, Name: "deployment_routing_policy", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Deployment{}) {
			return nil
		}
		if db.Migrator().HasColumn(&Deployment{}, "RoutingPolicy") {
			return nil
		}
		return db.Migrator().AddColumn(&Deployment{}, "RoutingPolicy")
	}},
	{Version: 95, Name: "peer_sync_rate_limits", Apply: func(db *gorm.DB) error {
		for _, item := range []struct {
			model any
			name  string
		}{
			{&ImageNode{}, "RateLimitKiB"},
			{&ImageNode{}, "LastRateLimitKiB"},
			{&ModelFile{}, "LastRateLimit"},
		} {
			if !db.Migrator().HasTable(item.model) || db.Migrator().HasColumn(item.model, item.name) {
				continue
			}
			if err := db.Migrator().AddColumn(item.model, item.name); err != nil {
				return err
			}
		}
		return nil
	}},
	{Version: 96, Name: "network_objects", Apply: func(db *gorm.DB) error {
		if db.Migrator().HasTable(&Node{}) && !db.Migrator().HasColumn(&Node{}, "NetworkInventory") {
			if err := db.Migrator().AddColumn(&Node{}, "NetworkInventory"); err != nil {
				return err
			}
		}
		return db.AutoMigrate(&Network{}, &NetworkAttachment{})
	}},
	{Version: 97, Name: "network_test_runs", Apply: func(db *gorm.DB) error {
		return db.AutoMigrate(&NetworkTestRun{})
	}},
	{Version: 98, Name: "node_serving_forward_status", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Node{}) || db.Migrator().HasColumn(&Node{}, "ServingForward") {
			return nil
		}
		return db.Migrator().AddColumn(&Node{}, "ServingForward")
	}},
	{Version: 99, Name: "network_type_rdma", Apply: func(db *gorm.DB) error {
		// RoCE and InfiniBand became one RDMA type whose link layer is read
		// from each node's ports.
		if !db.Migrator().HasTable(&Network{}) {
			return nil
		}
		return db.Model(&Network{}).Where("type IN ?", []string{"roce", "ib"}).Update("type", "rdma").Error
	}},
	{Version: 100, Name: "network_test_run_kind", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&NetworkTestRun{}) {
			return nil
		}
		for _, column := range []string{"Kind", "Image"} {
			if db.Migrator().HasColumn(&networkTestRunImageColumns{}, column) {
				continue
			}
			if err := db.Migrator().AddColumn(&networkTestRunImageColumns{}, column); err != nil {
				return err
			}
		}
		return nil
	}},
	{Version: 101, Name: "network_test_run_pull_image", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&NetworkTestRun{}) || db.Migrator().HasColumn(&networkTestRunImageColumns{}, "PullImage") {
			return nil
		}
		return db.Migrator().AddColumn(&networkTestRunImageColumns{}, "PullImage")
	}},
	{Version: 102, Name: "node_management_roles_off", Apply: func(db *gorm.DB) error {
		if db.Migrator().HasColumn(&Node{}, "ManagementRolesOff") {
			return nil
		}
		return db.Migrator().AddColumn(&Node{}, "ManagementRolesOff")
	}},
	// A node's address toward other nodes is its management network address;
	// the server reaches server_dial nodes at server_dial_address.
	{Version: 103, Name: "drop_node_direct_address", Apply: func(db *gorm.DB) error {
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM pragma_table_info('nodes') WHERE name = 'direct_address'").Scan(&n).Error; err != nil || n == 0 {
			return err
		}
		return db.Exec("ALTER TABLE nodes DROP COLUMN direct_address").Error
	}},
	{Version: 104, Name: "drop_access_setting", Apply: func(db *gorm.DB) error {
		// The server no longer keeps an external address of its own: every
		// address it hands out is the one the caller gives.
		if !db.Migrator().HasTable(&Setting{}) {
			return nil
		}
		return db.Where("key = ?", "access").Delete(&Setting{}).Error
	}},
	{Version: 105, Name: "drop_nccl_network_tests", Apply: func(db *gorm.DB) error {
		// NCCL tests in an image were removed: their runs cannot be read any
		// more, and the image columns only served them.
		if !db.Migrator().HasTable(&NetworkTestRun{}) {
			return nil
		}
		if db.Migrator().HasColumn(&networkTestRunImageColumns{}, "Kind") {
			if err := db.Where("kind = ?", "nccl_image").Delete(&NetworkTestRun{}).Error; err != nil {
				return err
			}
		}
		// Through store.dropColumn, not gorm's migrator: the migrator left
		// these columns in place and destroyed the table's index while
		// reporting success (see migrate_dropcolumn.go).
		for _, column := range []string{"image", "pull_image"} {
			if !db.Migrator().HasColumn(&networkTestRunImageColumns{}, column) {
				continue
			}
			if err := dropColumn(db, "network_test_runs", column); err != nil {
				return err
			}
		}
		return nil
	}}, {Version: 106, Name: "rdma_network_inferred", Apply: func(db *gorm.DB) error {
		// RDMA networks infer their rails and bootstrap from each node's
		// inventory: the HCA glob is gone, and an RDMA network keeps no
		// interface glob (its CIDR still picks the bootstrap address).
		if !db.Migrator().HasTable(&Network{}) {
			return nil
		}
		if err := db.Model(&Network{}).Where("type = ?", "rdma").Update("interface", "").Error; err != nil {
			return err
		}
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM pragma_table_info('networks') WHERE name = 'hca'").Scan(&n).Error; err != nil || n == 0 {
			return err
		}
		return db.Exec("ALTER TABLE networks DROP COLUMN hca").Error
	}},
	{Version: 107, Name: "network_test_run_pattern", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&NetworkTestRun{}) || db.Migrator().HasColumn(&NetworkTestRun{}, "Pattern") {
			return nil
		}
		return db.Migrator().AddColumn(&NetworkTestRun{}, "Pattern")
	}},
	{Version: 108, Name: "instance_pd_role", Apply: func(db *gorm.DB) error {
		// A PD disaggregated deployment's instances carry the role they play
		// (ADR 0028); an ordinary instance's role is empty. Version 108 leaves
		// 106 and 107 to the concurrent RDMA zero-configuration work.
		if !db.Migrator().HasTable(&Instance{}) {
			return nil
		}
		if !db.Migrator().HasColumn(&Instance{}, "Role") {
			if err := db.Migrator().AddColumn(&Instance{}, "Role"); err != nil {
				return err
			}
		}
		// Existing rows read as the empty role, not NULL, so a filter on
		// role = '' finds every ordinary instance.
		return db.Exec("UPDATE instances SET role = '' WHERE role IS NULL").Error
	}},
	{Version: 109, Name: "inference_spec_pd_mode", Apply: func(db *gorm.DB) error {
		// A catalog spec may exist only for PD disaggregated deployment in one
		// mode (ADR 0028); every existing spec is an ordinary one.
		if !db.Migrator().HasTable(&InferenceSpecRow{}) || db.Migrator().HasColumn(&InferenceSpecRow{}, "PDMode") {
			return nil
		}
		if err := db.Migrator().AddColumn(&InferenceSpecRow{}, "PDMode"); err != nil {
			return err
		}
		return db.Exec("UPDATE inference_specs SET pd_mode = '' WHERE pd_mode IS NULL").Error
	}},
	{Version: 110, Name: "drop_deployment_restart_count", Apply: func(db *gorm.DB) error {
		// A unit's spent attempts are its failed leader rows (ADR 0029); the
		// multi-node group reset counter is no longer written or read.
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM pragma_table_info('deployments') WHERE name = 'restart_count'").Scan(&n).Error; err != nil || n == 0 {
			return err
		}
		return db.Exec("ALTER TABLE deployments DROP COLUMN restart_count").Error
	}},
	{Version: 111, Name: "rename_network_types", Apply: func(db *gorm.DB) error {
		// Network types name where the port lives now that accelerator cards
		// bring a third kind (accelerator_rdma): ethernet and rdma are the
		// host's. The type an instance started on is recorded in its schedule
		// explanation and read back later, so it is renamed there too.
		renames := [][2]string{{"ethernet", "host_ethernet"}, {"rdma", "host_rdma"}}
		if db.Migrator().HasTable(&Network{}) {
			for _, r := range renames {
				if err := db.Exec("UPDATE networks SET type = ? WHERE type = ?", r[1], r[0]).Error; err != nil {
					return err
				}
			}
		}
		if !db.Migrator().HasTable(&Instance{}) || !db.Migrator().HasColumn(&Instance{}, "ScheduleExpl") {
			return nil
		}
		const path = "$.network.parameter.type"
		for _, r := range renames {
			if err := db.Exec("UPDATE instances SET schedule_expl = json_set(schedule_expl, ?, ?) WHERE json_valid(schedule_expl) AND json_extract(schedule_expl, ?) = ?",
				path, r[1], path, r[0]).Error; err != nil {
				return err
			}
		}
		return nil
	}},
	{Version: 112, Name: "image_catalog_launch_config_removed", Apply: dropImageCatalogLaunchConfig},
	{Version: 113, Name: "unused_image_and_storage_columns_removed", Apply: dropUnusedImageAndStorageColumns},
	{Version: 114, Name: "network_test_run_scope_label", Apply: splitNetworkTestRunScopeDetail},
	{Version: 115, Name: "unused_image_adaptation_columns_removed", Apply: dropUnusedImageAdaptationColumns},
	{Version: 116, Name: "deployment_restart_requested_at", Apply: func(db *gorm.DB) error {
		// A rolling restart replaces the replicas whose generation predates
		// the request, one at a time; the request time is the only record of
		// which replicas are done. NULL means no restart is in progress.
		if !db.Migrator().HasTable(&Deployment{}) || db.Migrator().HasColumn(&Deployment{}, "RestartRequestedAt") {
			return nil
		}
		return db.Migrator().AddColumn(&Deployment{}, "RestartRequestedAt")
	}},
	{Version: 117, Name: "gpu_identity_normalized", Apply: normalizeGPUIdentity},
	{Version: 118, Name: "rename_parameter_network_to_compute", Apply: renameComputeNetwork},
	{Version: 119, Name: "free_reserved_label_namespace", Apply: freeReservedLabelNamespace},
	{Version: 120, Name: "deployment_node_requirements", Apply: moveNodeRequirementsOutOfPlacement},
	{Version: 121, Name: "node_scheduling", Apply: func(db *gorm.DB) error {
		// Cordon and drain: every node starts schedulable ('').
		if !db.Migrator().HasTable(&Node{}) || db.Migrator().HasColumn(&Node{}, "Scheduling") {
			return nil
		}
		return db.Migrator().AddColumn(&Node{}, "Scheduling")
	}},
	{Version: 122, Name: "storage_first_class", Apply: migrateStorageFirstClass},
	{Version: 123, Name: "model_file_storage", Apply: migrateModelFileStorage},
	{Version: 124, Name: "deployment_weight_storage", Apply: migrateDeploymentWeightStorage},
	{Version: 125, Name: "drop_models_root_and_builtin", Apply: dropModelsRootAndBuiltin},
	{Version: 126, Name: "storage_paths_explicit", Apply: migrateStoragePathsExplicit},
	{Version: 127, Name: "instance_model_files", Apply: migrateInstanceModelFiles},
	{Version: 128, Name: "platform_ssh_keys", Apply: func(db *gorm.DB) error {
		return db.AutoMigrate(&PlatformSSHKey{})
	}},
	{Version: 129, Name: "template_model_operations", Apply: backfillTemplateModelOperations},
	{Version: 130, Name: "callable_template_args", Apply: migrateCallableTemplateArgs},
	{Version: 131, Name: "image_catalog_simplify", Apply: migrateImageCatalogSimplify},
	{Version: 132, Name: "deployment_start_timeout", Apply: func(db *gorm.DB) error {
		// A deployment's own health budget for its instances. Existing rows
		// state none (0) and keep following the cluster default.
		if !db.Migrator().HasTable(&Deployment{}) || db.Migrator().HasColumn(&Deployment{}, "StartTimeoutSec") {
			return nil
		}
		return db.Migrator().AddColumn(&Deployment{}, "StartTimeoutSec")
	}},
	{Version: 133, Name: "inference_spec_image_sources", Apply: migrateInferenceSpecImageSources},
	{Version: 134, Name: "benchmark_point_request_rate", Apply: func(db *gorm.DB) error {
		// Open-loop points are identified by their arrival rate. Existing
		// points are closed loop and keep 0.
		if !db.Migrator().HasTable(&BenchmarkPoint{}) || db.Migrator().HasColumn(&BenchmarkPoint{}, "RequestRate") {
			return nil
		}
		return db.Migrator().AddColumn(&BenchmarkPoint{}, "RequestRate")
	}},
	{Version: 135, Name: "cloud_host_snapshots", Apply: func(db *gorm.DB) error {
		return db.AutoMigrate(&cloudstore.CloudHost{})
	}},
	{Version: 136, Name: "cloud_resource_leases", Apply: func(db *gorm.DB) error {
		return db.AutoMigrate(&cloudstore.CloudResourceLease{})
	}},
	{Version: 137, Name: "platform_tokens", Apply: func(db *gorm.DB) error {
		return db.AutoMigrate(&PlatformToken{})
	}},
	{Version: 138, Name: "deployment_health_check", Apply: func(db *gorm.DB) error {
		// The health check is opt-in: existing deployments are not checked
		// until turned on, and then wait the default 60 seconds.
		if !db.Migrator().HasTable(&Deployment{}) {
			return nil
		}
		for _, column := range []string{"HealthCheck", "HealthCheckThresholdSec"} {
			if db.Migrator().HasColumn(&Deployment{}, column) {
				continue
			}
			if err := db.Migrator().AddColumn(&Deployment{}, column); err != nil {
				return err
			}
		}
		return nil
	}},
	{Version: 139, Name: "drop_monitor_rules_setting", Apply: func(db *gorm.DB) error {
		// The monitor no longer judges health by rules and thresholds; a
		// deployment's health is its health check. Their settings go with
		// them.
		if !db.Migrator().HasTable(&Setting{}) {
			return nil
		}
		return db.Where("key = ?", "monitor.rules").Delete(&Setting{}).Error
	}},
	{Version: 140, Name: "management_sessions", Apply: func(db *gorm.DB) error { return db.AutoMigrate(&ManagementSession{}) }},
	{Version: 141, Name: "node_gpu_init_failures", Apply: func(db *gorm.DB) error {
		// Added here and dropped again by 145, which gave those cards rows
		// of their own. The shape is declared locally because Node no
		// longer has the field, and a fresh database still runs this.
		if !db.Migrator().HasTable(&Node{}) || db.Migrator().HasColumn(&Node{}, "gpu_init_failures") {
			return nil
		}
		return db.Table("nodes").Migrator().AddColumn(&nodeGPUInitFailuresColumn{}, "GPUInitFailures")
	}},
	// 142 was nodes.runtime_gpu_ready, a nullable bool for the same fact
	// migration 149 keeps as a reason code. It is not in this list because
	// a database that already ran it has the marker, so re-numbering it
	// would skip a later migration; 149 drops the column it left behind.
	{Version: 143, Name: "instance_gpu_idents", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Instance{}) {
			return nil
		}
		if !db.Migrator().HasColumn(&Instance{}, "GPUIdents") {
			if err := db.Migrator().AddColumn(&Instance{}, "GPUIdents"); err != nil {
				return err
			}
		}
		// Back-fill from the cards as last probed: today's indexes still name
		// the cards these instances were placed on, so this is the one moment
		// the binding can be recovered. Afterwards only realignInstanceGPUs
		// maintains it. It runs whether or not the column was just added: an
		// old enough database got the column from AutoMigrate earlier.
		if !db.Migrator().HasTable(&GPU{}) {
			return nil
		}
		return backfillInstanceGPUIdents(db)
	}},
	{Version: 144, Name: "node_connection_states", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Node{}) {
			return nil
		}
		for _, column := range []string{"GPUProbeError", "GPUProbeFailedAt"} {
			if db.Migrator().HasColumn(&Node{}, column) {
				continue
			}
			if err := db.Migrator().AddColumn(&Node{}, column); err != nil {
				return err
			}
		}
		// "pending" is the same thing "connecting" now names: no session
		// yet. "degraded" said an accelerator probe failed, which no longer
		// has anything to do with the connection — those nodes had a live
		// session, so they are online, and the next heartbeat or disconnect
		// settles any that have since dropped.
		if err := db.Model(&Node{}).Where("state = ?", "pending").Update("state", NodeStateConnecting).Error; err != nil {
			return err
		}
		return db.Model(&Node{}).Where("state = ?", "degraded").Update("state", NodeStateOnline).Error
	}},
	{Version: 145, Name: "card_identity_and_ownership", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&GPU{}) {
			return nil
		}
		for _, column := range []string{"Ident", "Ownership"} {
			if db.Migrator().HasColumn(&GPU{}, column) {
				continue
			}
			if err := db.Migrator().AddColumn(&GPU{}, column); err != nil {
				return err
			}
		}
		if err := backfillCardIdents(db); err != nil {
			return err
		}
		// The key moves from the device index to the identity. Both indexes
		// are touched by name because GORM's AutoMigrate would leave the
		// old unique index in place, and it forbids exactly what this
		// change allows: two rows of one node without an index (-1), which
		// is every card the vendor CLI does not list.
		if db.Migrator().HasIndex(&GPU{}, "idx_node_gpu") {
			if err := db.Migrator().DropIndex(&GPU{}, "idx_node_gpu"); err != nil {
				return err
			}
		}
		if !db.Migrator().HasIndex(&GPU{}, "idx_node_card") {
			if err := db.Migrator().CreateIndex(&GPU{}, "idx_node_card"); err != nil {
				return err
			}
		}
		// Cards the driver could not bring up are cards now, each with
		// its own row and health, so the node's list of them goes. It is
		// dropped through store.dropColumn, which puts the table's
		// indexes back: SQLite rebuilds the table for a column drop and
		// the rebuilt one has none of them.
		if db.Migrator().HasColumn(&nodeGPUInitFailuresColumn{}, "gpu_init_failures") {
			return dropColumn(db, "nodes", "gpu_init_failures")
		}
		return nil
	}},
	// 146 keeps its name for the databases that carry its marker. It once
	// also added gpus.schedulable, a stored copy of GPU.CanTakeWork that
	// 151 drops.
	{Version: 146, Name: "card_schedulable_verdict", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&GPU{}) {
			return nil
		}
		// Existing cards get the normalized level from what their last
		// probe reported; the next probe rewrites it.
		return backfillCardHealthLevels(db)
	}},
	{Version: 147, Name: "card_foreign_processes", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&GPU{}) {
			return nil
		}
		// Empty for every existing card; the next probe of each node fills
		// it, and a card nobody else uses keeps it empty.
		if db.Migrator().HasColumn(&GPU{}, "ForeignProcesses") {
			return nil
		}
		return db.Migrator().AddColumn(&GPU{}, "ForeignProcesses")
	}},
	{Version: 148, Name: "card_health_without_the_boolean", Apply: func(db *gorm.DB) error {
		// A card's health is its level and its ownership (GPU.Healthy
		// answers from those two), so the boolean an agent predating
		// levels reports is turned into a level when the card is written
		// and not kept beside it. Migration 146 already gave every
		// stored row its normalized level.
		if !db.Migrator().HasTable(&GPU{}) {
			return nil
		}
		present, err := tableHasColumn(db, "gpus", "healthy")
		if err != nil || !present {
			return err
		}
		return dropColumn(db, "gpus", "healthy")
	}},
	{Version: 149, Name: "node_runtime_gpu_blocked", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Node{}) {
			return nil
		}
		if !db.Migrator().HasColumn(&Node{}, "RuntimeGPUBlocked") {
			// Empty until each node's next report says otherwise.
			if err := db.Migrator().AddColumn(&Node{}, "RuntimeGPUBlocked"); err != nil {
				return err
			}
		}
		// runtime_gpu_ready held the same fact as a nullable bool (see the
		// note where 142 was). A database that ran it carries the column
		// with no field left to read it, so it goes here rather than
		// outliving the thing it answered.
		present, err := tableHasColumn(db, "nodes", "runtime_gpu_ready")
		if err != nil || !present {
			return err
		}
		return dropColumn(db, "nodes", "runtime_gpu_ready")
	}},
	// 150 indexed gpus.schedulable, which 151 drops; it is not in this list
	// for the reason 142 is not.
	{Version: 151, Name: "card_verdict_not_stored", Apply: func(db *gorm.DB) error {
		// Whether a card can take work, and whether something outside the
		// platform holds it, were stored beside the facts they follow
		// from (GPU.CanTakeWork answers from those facts). A stored copy
		// can only repeat them or, once the rule changes, contradict them
		// until every node has reported again.
		if !db.Migrator().HasTable(&GPU{}) {
			return nil
		}
		// The index goes first: SQLite refuses to drop a column an index
		// still names.
		if err := db.Exec("DROP INDEX IF EXISTS idx_gpus_schedulable_for_sql").Error; err != nil {
			return err
		}
		for _, column := range []string{"schedulable", "foreign_process_count"} {
			present, err := tableHasColumn(db, "gpus", column)
			if err != nil {
				return err
			}
			if !present {
				continue
			}
			if err := dropColumn(db, "gpus", column); err != nil {
				return err
			}
		}
		return nil
	}},
	{Version: 152, Name: "pull_settings_tables", Apply: migratePullSettingsTables},
	{Version: 153, Name: "pull_source_rules_from_rewrites", Apply: migratePullSourceRules},
	{Version: 154, Name: "deployment_acquisition_choices_dropped", Apply: migrateDropAcquisitionChoices},
	{Version: 155, Name: "node_runtime_fallback", Apply: addNodeRuntimeFallback},
	{Version: 156, Name: "node_accelerator_choice", Apply: addNodeAcceleratorChoice},
	{Version: 157, Name: "pull_source_rules_control_plane", Apply: addPullSourceRuleControlPlane},
	{Version: 158, Name: "network_rewrites_to_site_rules", Apply: migrateNetworkRewritesToSiteRules},
	{Version: 159, Name: "source_credentials", Apply: migrateSourceCredentials},
	{Version: 160, Name: "storage_node_override_scope", Apply: migrateStorageNodeOverrideScope},
	{Version: 161, Name: "storage_cache_operations", Apply: createStorageCacheOperations},
	{Version: 162, Name: "node_install_dir", Apply: addNodeInstallDir},
	{Version: 163, Name: "legacy_instance_role_and_deployment_state", Apply: reconcileLegacyResourceValues},
	{Version: 164, Name: "legacy_schema_repairs", Apply: migrateLegacySchemaRepairs},
	{Version: 165, Name: "deployment_tokenize_disabled", Apply: addDeploymentTokenizeDisabled},
	{Version: 166, Name: "notice_reads", Apply: createNoticeReads},
	{Version: 167, Name: "inference_spec_shm_bytes", Apply: addInferenceSpecShmBytes},
	{Version: 168, Name: "inference_spec_memlock_unlimited", Apply: addInferenceSpecMemlockUnlimited},
	{Version: 169, Name: "api_key_revoked_at", Apply: addAPIKeyRevokedAt},
	{Version: 170, Name: "operations_usage", Apply: createOperationsUsage},
	{Version: 171, Name: "deleted_nodes", Apply: createDeletedNodes},
	{Version: 172, Name: "legacy_template_model_operations", Apply: repairLegacyTemplateModelOperations},
	{Version: 173, Name: "instance_spec_gpu_idents", Apply: copyInstanceGPUIdentsIntoSpec},
	{Version: 174, Name: "instance_transcription_served", Apply: addInstanceTranscriptionServed},
	{Version: 175, Name: "image_node_runtime_size", Apply: addImageNodeRuntimeSize},
	{Version: 176, Name: "instance_sleep_residue", Apply: addInstanceSleepResidue},
	{Version: 177, Name: "deployment_node_replace_after", Apply: addDeploymentNodeReplaceAfter},
	{Version: 178, Name: "deployment_units_auto", Apply: addDeploymentUnitsAuto},
	{Version: 179, Name: "ai_config_single_default", Apply: addAIConfigSingleDefault},
	{Version: 180, Name: "model_download_deployment", Apply: addModelDownloadDeployment},
	{Version: 181, Name: "agent_upgrade_runs", Apply: createAgentUpgradeTables},
	{Version: 182, Name: "deployment_idle_sleep", Apply: addDeploymentIdleSleep},
	{Version: 183, Name: "instance_wake_waiting", Apply: addInstanceWakeWaiting},
	{Version: 184, Name: "lifecycle_schedules", Apply: createLifecycleSchedules},
	{Version: 185, Name: "instance_event_kind_index", Apply: indexInstanceEventKind},
	{Version: 186, Name: "deployment_service_level", Apply: addDeploymentServiceLevel},
	{Version: 187, Name: "batch_tables", Apply: createBatchTables},
	{Version: 188, Name: "lifecycle_schedule_role", Apply: addLifecycleScheduleRole},
	{Version: 189, Name: "deployment_yield_policy", Apply: addDeploymentYieldPolicy},
	{Version: 190, Name: "deployment_yield_policy_reshape", Apply: reshapeDeploymentYieldPolicy},
	{Version: 191, Name: "instance_sleep_held", Apply: moveSleepHeldToInstances},
	{Version: 192, Name: "instance_l1_host_buffer", Apply: addInstanceL1HostBuffer},
	{Version: 193, Name: "api_key_scope", Apply: addAPIKeyScope},
	{Version: 194, Name: "node_scheduling_source", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Node{}) || db.Migrator().HasColumn(&Node{}, "SchedulingBy") {
			return nil
		}
		// Empty on every existing row: a cordon set before this was
		// recorded has no known author, and the platform leaves one it
		// cannot prove is its own alone.
		return db.Migrator().AddColumn(&Node{}, "SchedulingBy")
	}},
	{Version: 195, Name: "gpu_operator_scheduling", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&GPU{}) {
			return nil
		}
		// Both empty on existing cards: no operator has decided anything
		// about them yet.
		for _, column := range []string{"SchedulingStopped", "SchedulingAllowedAt"} {
			if db.Migrator().HasColumn(&GPU{}, column) {
				continue
			}
			if err := db.Migrator().AddColumn(&GPU{}, column); err != nil {
				return err
			}
		}
		return nil
	}},
	{Version: 196, Name: "card_health_episodes", Apply: createCardHealthEpisodes},
	{Version: 197, Name: "inference_spec_display_names", Apply: replaceInferenceSpecDisplayProfile},
	{Version: 198, Name: "sync_group_unique_names", Apply: migrateSyncGroupNames},
	{Version: 199, Name: "node_peer_memory", Apply: func(db *gorm.DB) error {
		if !db.Migrator().HasTable(&Node{}) || db.Migrator().HasColumn(&Node{}, "PeerMemory") {
			return nil
		}
		// Empty until each node's next report says otherwise.
		return db.Migrator().AddColumn(&Node{}, "PeerMemory")
	}},
	{Version: 200, Name: "model_source_id_index", Apply: createModelSourceIDIndex},
	{Version: 201, Name: "instance_runtime_failure", Apply: addInstanceRuntimeFailure},
	{Version: 202, Name: "kube_clusters", Apply: createKubeClusters},
	{Version: 203, Name: "kube_cluster_image_pull_secret", Apply: addKubeClusterImagePullSecret},
	{Version: 204, Name: "cloud_kubernetes_sources", Apply: addCloudKubernetesSources},
	{Version: 205, Name: "kube_cluster_pools", Apply: createKubeClusterPools},
	{Version: 206, Name: "kube_cluster_agent_image_source", Apply: addKubeClusterAgentImageSource},
	{Version: 207, Name: "legacy_column_and_index_leftovers", Apply: repairLegacyColumnAndIndexLeftovers},
	{Version: 208, Name: "peer_transfer_ownership", Apply: addPeerTransferOwnership},
	{Version: 209, Name: "kube_cluster_priority", Apply: addKubeClusterPriority},
}

// modelFileNetworkColumns are the columns migrations 76 and 77 added to
// model_files for a download's hand-picked endpoint and proxy, and 158
// dropped: where a node downloads from is planned from the pull source rules.
type modelFileNetworkColumns struct {
	Proxy           string `gorm:"size:512"`
	Endpoint        string `gorm:"size:512"`
	NetworkExplicit bool
}

func (modelFileNetworkColumns) TableName() string { return "model_files" }

// nodeGPUInitFailuresColumn is the column migration 141 added to nodes and
// 145 dropped: a node-level list of the cards whose driver initialization
// failed, before each such card became a row of its own.
type nodeGPUInitFailuresColumn struct {
	GPUInitFailures string `gorm:"size:1024;column:gpu_init_failures"`
}

func (nodeGPUInitFailuresColumn) TableName() string { return "nodes" }

// networkTestRunImageColumns is network_test_runs as migrations 100 and 101
// shaped it, for those migrations and for 105, which drops the image columns.
type networkTestRunImageColumns struct {
	Kind      string `gorm:"size:16"`
	Image     string `gorm:"size:512"`
	PullImage bool
}

func (networkTestRunImageColumns) TableName() string { return "network_test_runs" }

// dropInstanceInjectedArgs removes the retired platform-injection ledger. The
// instance's desired state carries the complete entrypoint and argv, which is
// what the UI and the AI helper read.
func dropInstanceInjectedArgs(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Instance{}) {
		return nil
	}
	if db.Migrator().HasColumn("instances", "injected_args") {
		return db.Exec("ALTER TABLE instances DROP COLUMN injected_args").Error
	}
	return nil
}

// dropModelCapabilityOverride removes the retired model-level capability
// claim. API operations are the only model inference configuration.
func dropModelCapabilityOverride(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Model{}) {
		return nil
	}
	if db.Migrator().HasColumn("models", "capability_override") {
		return db.Exec("ALTER TABLE models DROP COLUMN capability_override").Error
	}
	return nil
}

func dropInferenceModelKinds(db *gorm.DB) error {
	if !db.Migrator().HasTable(&InferenceModel{}) {
		return nil
	}
	if db.Migrator().HasColumn("inference_models", "kinds") {
		return db.Exec("ALTER TABLE inference_models DROP COLUMN kinds").Error
	}
	return nil
}

// dropModelEngineSupport removes the retired model-level engine claims. Engine
// applicability belongs to each inference template spec and is never a model
// fact, so these columns must not survive the schema cutover.
func dropModelEngineSupport(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Model{}) {
		return nil
	}
	for _, column := range []string{"vllm_support", "engine_support"} {
		if db.Migrator().HasColumn("models", column) {
			if err := db.Exec("ALTER TABLE models DROP COLUMN " + column).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// migrateModelInferenceConfig introduces the model-level editable contract.
// Existing capability/modalities values are converted once; subsequent reads
// use operations and input_modalities directly.
func migrateModelInferenceConfig(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Model{}) {
		return nil
	}
	if err := db.AutoMigrate(&Model{}); err != nil {
		return err
	}
	var models []Model
	if err := db.Find(&models).Error; err != nil {
		return err
	}
	for _, model := range models {
		if strings.TrimSpace(model.Operations) == "" {
			raw, err := json.Marshal([]map[string]string{{"type": "openai.chat"}})
			if err != nil {
				return err
			}
			model.Operations = string(raw)
		}
		if strings.TrimSpace(model.InputModalities) == "" {
			modalities := []string{"text"}
			if strings.TrimSpace(model.Meta) != "" {
				if cfg, err := modelconfig.Parse(strings.NewReader(model.Meta)); err == nil {
					if inferred := modelmeta.InputModalities(cfg); len(inferred) > 0 {
						modalities = inferred
					}
				}
			}
			raw, err := json.Marshal(modalities)
			if err != nil {
				return err
			}
			model.InputModalities = string(raw)
		}
		if err := db.Model(&Model{}).Where("id = ?", model.ID).Updates(map[string]any{
			"operations": model.Operations, "input_modalities": model.InputModalities,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func migrateFlatInferenceCatalog(db *gorm.DB) error {
	// This is an intentional cutover. The old graph is not a source of truth
	// and must not remain queryable alongside the flat catalog.
	for _, table := range []string{
		"validation_runs", "template_evidence", "template_profiles",
		"chip_supports", "template_variants", "inference_templates",
		"chip_classes", "chip_registry",
	} {
		if db.Migrator().HasTable(table) {
			if err := db.Migrator().DropTable(table); err != nil {
				return fmt.Errorf("drop retired catalog table %s: %w", table, err)
			}
		}
	}
	if err := db.AutoMigrate(&InferenceModel{}, &InferenceSpecRow{}, &Deployment{}); err != nil {
		return err
	}
	if db.Migrator().HasColumn("deployments", "template_profile_key") {
		// SQLite refuses DROP COLUMN while any surviving index SQL still
		// references that column. Production v65 databases have the GORM index
		// below, whereas a freshly synthesized test column does not.
		var indexes []struct{ Name string }
		if err := db.Raw(`SELECT name FROM sqlite_schema
			WHERE type = 'index' AND tbl_name = 'deployments' AND sql IS NOT NULL
			AND lower(sql) LIKE '%template_profile_key%'`).Scan(&indexes).Error; err != nil {
			return fmt.Errorf("find retired deployment template profile indexes: %w", err)
		}
		for _, index := range indexes {
			if err := db.Exec("DROP INDEX " + quoteSQLiteIdentifier(index.Name)).Error; err != nil {
				return fmt.Errorf("drop retired deployment template profile index %s: %w", index.Name, err)
			}
		}
		if err := db.Exec("ALTER TABLE deployments DROP COLUMN template_profile_key").Error; err != nil {
			return fmt.Errorf("drop retired deployment template profile key: %w", err)
		}
	}
	return nil
}

// normalizeStoredTimesToUTC converts every persisted timestamp to the same
// representation that the UTC GORM NowFunc writes. SQLite compares and orders
// DATETIME columns as text, so mixing historical offsets with new +00:00 rows
// would otherwise make last-seen cutoffs and chronological ordering incorrect.
//
// This deliberately operates on the schema rather than a hand-maintained table
// list: every GORM timestamp is named *_at, plus APIKey.last_used. Passing the
// parsed time.Time back through the driver preserves five-, six-, or
// nanosecond-width fractional seconds while normalizing the offset.
func normalizeStoredTimesToUTC(db *gorm.DB) error {
	var tables []string
	if err := db.Raw(`SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables).Error; err != nil {
		return err
	}
	for _, table := range tables {
		var columns []struct{ Name string }
		if err := db.Raw("PRAGMA table_info(" + quoteSQLiteIdentifier(table) + ")").Scan(&columns).Error; err != nil {
			return err
		}
		for _, column := range columns {
			if !isStoredTimeColumn(column.Name) {
				continue
			}
			var rows []struct {
				RowID int64  `gorm:"column:row_id"`
				Value string `gorm:"column:value"`
			}
			quotedTable := quoteSQLiteIdentifier(table)
			quotedColumn := quoteSQLiteIdentifier(column.Name)
			query := "SELECT rowid AS row_id, CAST(" + quotedColumn + " AS TEXT) AS value FROM " + quotedTable +
				" WHERE " + quotedColumn + " IS NOT NULL AND trim(CAST(" + quotedColumn + " AS TEXT)) <> ''"
			if err := db.Raw(query).Scan(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				parsed, err := parseStoredTime(row.Value)
				if err != nil {
					return fmt.Errorf("parse %s.%s rowid %d timestamp %q: %w", table, column.Name, row.RowID, row.Value, err)
				}
				update := "UPDATE " + quotedTable + " SET " + quotedColumn + " = ? WHERE rowid = ?"
				if err := db.Exec(update, parsed.UTC(), row.RowID).Error; err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func isStoredTimeColumn(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), "_at") || strings.EqualFold(name, "last_used")
}

func quoteSQLiteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func parseStoredTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if len(value) > len("2006-01-02") && value[len("2006-01-02")] == ' ' {
		value = value[:len("2006-01-02")] + "T" + value[len("2006-01-02")+1:]
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported SQLite datetime format")
}

func (s *Store) migrate() error {
	return migrateDB(s.db, migrations)
}

// migrateDB applies an explicit migration list. Keeping the list as an
// argument makes the marker-skip/idempotence contract testable without
// executing every historical production migration.
func migrateDB(db *gorm.DB, migrationList []migration) error {
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		return err
	}
	for _, m := range migrationList {
		var row SchemaMigration
		err := db.Where("version = ?", m.Version).First(&row).Error
		if err == nil {
			if row.Name != m.Name {
				if repairableLegacyMigration(m) {
					if err := repairLegacyMigration(db, m); err != nil {
						return fmt.Errorf("repair migration %d (%s): %w", m.Version, m.Name, err)
					}
					continue
				}
				// Only a warning: master has renamed migrations it had
				// already shipped (48, 49, 52, 135), and failing here would
				// stop those installations from starting. What it catches is
				// a database that ran a different migration under this
				// number, typically a branch build, which is then skipped.
				slog.Warn("schema migration recorded under another name; skipped as applied",
					"version", m.Version, "recorded", row.Name, "expected", m.Name)
			}
			continue
		}
		if err != gorm.ErrRecordNotFound {
			return err
		}
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

// applyMigration runs both the schema change and its marker insert in one
// transaction. Keeping this small unit separate makes rollback semantics
// directly testable and prevents future migrations from accidentally writing
// their marker outside the transaction.
func applyMigration(db *gorm.DB, m migration) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return tx.Create(&SchemaMigration{Version: m.Version, Name: m.Name, AppliedAt: time.Now().UTC()}).Error
	})
}

// AutoMigrateDev exposes the old schema convenience path for local development.
func (s *Store) AutoMigrateDev() error {
	return s.db.AutoMigrate(
		&SchemaMigration{}, &Node{}, &GPU{}, &User{}, &APIKey{}, &APIKeyDeployment{}, &ManagementSession{},
		&Model{}, &ModelFile{}, &Deployment{}, &Instance{}, &InstanceEvent{},
		&Provider{}, &ProviderInstance{}, &ProviderBootstrapToken{}, &NodeSSHCredential{}, &Setting{}, &AIConfig{}, &Profile{}, &ImageSeries{}, &Image{}, &ImageNode{}, &Storage{}, &StorageNode{}, &StorageNodeState{}, &TerminalSession{}, &BenchmarkRun{}, &BenchmarkPoint{}, &InferenceModel{}, &InferenceSpecRow{}, &Network{}, &NetworkAttachment{}, &NetworkTestRun{}, &DeploymentNodeRequirement{}, &InstanceModelFile{}, &PlatformSSHKey{}, &PlatformToken{},
		&PullSourceRule{}, &PullSourceRuleEndpoint{}, &PullSourceRuleRequirement{}, &SyncGroup{}, &SyncGroupRequirement{}, &SourceCredential{}, &PullProxy{}, &PullProxyRequirement{},
		&NoticeRead{}, &LifecycleSchedule{}, &BatchFile{}, &Batch{}, &BatchMetadata{},
		&UsageHourly{}, &UsageDaily{}, &UsageRolledDay{}, &UsageRollupState{}, &DeletedDeployment{}, &DeletedNode{},
	)
}

// CurrentMigration returns the highest applied schema version.
func (s *Store) CurrentMigration() (int, error) {
	var row SchemaMigration
	if err := s.db.Order("version desc").First(&row).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return 0, nil
		}
		return 0, err
	}
	return row.Version, nil
}
