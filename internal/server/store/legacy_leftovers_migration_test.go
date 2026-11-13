package store

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Rows the leftovers migration must carry over: a stopped deployment whose
// target still lives in the pre-rename state column, an instance holding a
// retired run_spec_snapshot value, and the model both reference.
var legacyLeftoverRows = []string{
	"INSERT INTO models (id, source, source_id, revision, name, created_at, updated_at) VALUES (900, 'huggingface', 'Qwen/Qwen3-8B', 'main', 'qwen3-8b', '2026-08-23 12:00:00', '2026-08-23 12:00:00')",
	"INSERT INTO deployments (id, name, model_id, served_name, engine, replicas, state, created_at, updated_at) VALUES (900, 'legacy-stopped', 900, 'qwen3-8b', 'vllm', 1, 'stopped', '2026-08-23 12:00:00', '2026-08-23 12:00:00')",
	"INSERT INTO instances (id, uuid, deployment_id, state, image, run_spec_snapshot, created_at, updated_at) VALUES (901, 'legacy-instance', 900, 'stopped', 'vllm/vllm-openai:v0.10.0', '{\"model_mount_path\":\"/model\"}', '2026-08-23 12:00:00', '2026-08-23 12:00:00')",
}

var sqlWhitespace = regexp.MustCompile(`\s+`)

// schemaOf describes every table column (type, NOT NULL, default) and every
// named index definition, normalised so that quoting, spacing and column
// order (ALTER TABLE ADD COLUMN appends) do not count as differences.
func schemaOf(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	var tables []string
	if err := db.Raw("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, table := range tables {
		var columns []struct {
			Name    string
			Type    string
			NotNull int `gorm:"column:notnull"`
			Default string
		}
		if err := db.Raw("SELECT name, type, \"notnull\", COALESCE(dflt_value, '') AS \"default\" FROM pragma_table_info(?)", table).Scan(&columns).Error; err != nil {
			t.Fatal(err)
		}
		out["table "+table] = ""
		for _, c := range columns {
			out["column "+table+"."+c.Name] = fmt.Sprintf("%s notnull=%d default=%s", strings.ToLower(c.Type), c.NotNull, c.Default)
		}
	}
	var indexes []struct{ Name, SQL string }
	if err := db.Raw("SELECT name, sql FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL").Scan(&indexes).Error; err != nil {
		t.Fatal(err)
	}
	for _, index := range indexes {
		sql := strings.NewReplacer("`", "", `"`, "").Replace(strings.ToLower(index.SQL))
		out["index "+index.Name] = strings.ReplaceAll(sqlWhitespace.ReplaceAllString(sql, ""), "ifnotexists", "")
	}
	return out
}

func assertSchemaMatches(t *testing.T, got, want map[string]string) {
	t.Helper()
	var diffs []string
	for key, value := range want {
		if other, ok := got[key]; !ok {
			diffs = append(diffs, "missing "+key)
		} else if other != value {
			diffs = append(diffs, fmt.Sprintf("%s: %q, want %q", key, other, value))
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			diffs = append(diffs, "extra "+key)
		}
	}
	if len(diffs) > 0 {
		t.Fatalf("schema differs from a fresh database:\n%s", strings.Join(diffs, "\n"))
	}
}

func openSQLiteFile(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// A v40 database of either historical lineage, upgraded straight to the
// current version, must end with exactly the schema of a fresh database:
// deployments.state, instances.run_spec_snapshot and the stale definitions
// of idx_deployments_state and idx_model_lookup used to survive.
func TestLineageV40UpgradeMatchesFreshSchema(t *testing.T) {
	fresh := schemaOf(t, newTestStore(t).db)
	for _, fixture := range []string{"benchmark_lineage_v40.sql", "image_lineage_v40.sql"} {
		t.Run(strings.TrimSuffix(fixture, ".sql"), func(t *testing.T) {
			path := historicalLineageDB(t, fixture, legacyLeftoverRows...)
			st, err := Open(path, bus.New())
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			assertSchemaMatches(t, schemaOf(t, st.db), fresh)

			var dep Deployment
			if err := st.db.First(&dep, 900).Error; err != nil || dep.Name != "legacy-stopped" || dep.State != DeployStopped || dep.ServedName != "qwen3-8b" {
				t.Fatalf("deployment=%+v err=%v", dep, err)
			}
			var inst Instance
			if err := st.db.First(&inst, 901).Error; err != nil || inst.UUID != "legacy-instance" || inst.DeploymentID != 900 || inst.Image != "vllm/vllm-openai:v0.10.0" {
				t.Fatalf("instance=%+v err=%v", inst, err)
			}
			var model Model
			if err := st.db.First(&model, 900).Error; err != nil || model.SourceID != "Qwen/Qwen3-8B" {
				t.Fatalf("model=%+v err=%v", model, err)
			}
			markers := markerNames(t, st.db)
			for _, m := range migrations {
				if markers[m.Version] != m.Name {
					t.Fatalf("marker %d = %q, want %q", m.Version, markers[m.Version], m.Name)
				}
			}
			if len(markers) != len(migrations) {
				t.Fatalf("%d markers, want %d", len(markers), len(migrations))
			}
			var applied []SchemaMigration
			if err := st.db.Order("version").Find(&applied).Error; err != nil {
				t.Fatal(err)
			}
			schema := schemaOf(t, st.db)
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			again, err := Open(path, bus.New())
			if err != nil {
				t.Fatalf("second start: %v", err)
			}
			t.Cleanup(func() { _ = again.Close() })
			assertSchemaMatches(t, schemaOf(t, again.db), schema)
			var reapplied []SchemaMigration
			if err := again.db.Order("version").Find(&reapplied).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reapplied, applied) {
				t.Fatal("second start rewrote schema markers")
			}
			var count int64
			if err := again.db.Model(&Deployment{}).Where("desired_state = ?", DeployStopped).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("stopped deployments after second start=%d err=%v", count, err)
			}
		})
	}
}

// The leftover repair either finishes completely or leaves the database as
// it found it, both when the change itself fails and when its marker cannot
// be written, and a later start completes it.
func TestLegacyLeftoversMigrationRollsBackAndRetries(t *testing.T) {
	m := migrationByName(t, "legacy_column_and_index_leftovers")
	// The repair must run after every migration that left these pieces
	// behind; later, unrelated migrations may follow it.
	for _, name := range []string{"model_source_id_casefold_identity", "instance_desired_state_launch_snapshot", "model_artifact_subfolder_identity",
		"deployment_desired_state_column", "image_catalog_launch_config_removed", "legacy_instance_role_and_deployment_state"} {
		if earlier := migrationByName(t, name); earlier.Version >= m.Version {
			t.Fatalf("leftover repair is migration %d but %s is %d", m.Version, name, earlier.Version)
		}
	}
	index := -1
	for i, candidate := range migrations {
		if candidate.Version == m.Version {
			index = i
		}
	}
	earlier := migrations[:index]
	failures := map[string]func(*testing.T, *gorm.DB) migration{
		"change": func(t *testing.T, db *gorm.DB) migration {
			failing := m
			failing.Apply = func(tx *gorm.DB) error {
				if err := m.Apply(tx); err != nil {
					return err
				}
				return errors.New("injected leftover repair failure")
			}
			return failing
		},
		"marker": func(t *testing.T, db *gorm.DB) migration {
			if err := db.Exec(fmt.Sprintf("CREATE TRIGGER fail_leftover_marker BEFORE INSERT ON schema_migrations WHEN NEW.version = %d BEGIN SELECT RAISE(ABORT, 'injected'); END", m.Version)).Error; err != nil {
				t.Fatal(err)
			}
			return m
		},
	}
	for name, inject := range failures {
		t.Run(name, func(t *testing.T) {
			db := openSQLiteFile(t, historicalLineageDB(t, "benchmark_lineage_v40.sql", legacyLeftoverRows...))
			if err := migrateDB(db, earlier); err != nil {
				t.Fatal(err)
			}
			before := schemaOf(t, db)
			for _, leftover := range []string{"column deployments.state", "column instances.run_spec_snapshot"} {
				if _, ok := before[leftover]; !ok {
					t.Fatalf("the fixture does not reproduce %s", leftover)
				}
			}
			if err := migrateDB(db, []migration{inject(t, db)}); err == nil {
				t.Fatal("injected failure was not reported")
			}
			assertSchemaMatches(t, schemaOf(t, db), before)
			if _, ok := markerNames(t, db)[m.Version]; ok {
				t.Fatal("failed repair left its marker")
			}
			var stateValue string
			if err := db.Raw("SELECT state FROM deployments WHERE id = 900").Scan(&stateValue).Error; err != nil || stateValue != "stopped" {
				t.Fatalf("deployments.state after rollback=%q err=%v", stateValue, err)
			}

			if err := db.Exec("DROP TRIGGER IF EXISTS fail_leftover_marker").Error; err != nil {
				t.Fatal(err)
			}
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatalf("retry: %v", err)
			}
			after := schemaOf(t, db)
			for _, leftover := range []string{"column deployments.state", "column instances.run_spec_snapshot"} {
				if _, ok := after[leftover]; ok {
					t.Fatalf("retry left %s", leftover)
				}
			}
			if got := after["index idx_deployments_state"]; got != "createindexidx_deployments_stateondeployments(desired_state)" {
				t.Fatalf("idx_deployments_state after retry = %q", got)
			}
			if got := markerNames(t, db)[m.Version]; got != m.Name {
				t.Fatalf("marker after retry = %q", got)
			}
			var desired string
			if err := db.Raw("SELECT desired_state FROM deployments WHERE id = 900").Scan(&desired).Error; err != nil || desired != "stopped" {
				t.Fatalf("desired_state after retry=%q err=%v", desired, err)
			}
		})
	}
}

// A database whose rename never ran gets it here, and an emptied target is
// refilled from the old column before that column goes away. A fresh-shaped
// table is left alone, and running the change twice is harmless.
func TestLegacyLeftoversMigrationFinishesDeploymentRename(t *testing.T) {
	m := migrationByName(t, "legacy_column_and_index_leftovers")
	cases := map[string][]string{
		"rename pending": {
			"CREATE TABLE `deployments` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text,`state` text)",
			"CREATE INDEX `idx_deployments_state` ON `deployments`(`state`)",
			"INSERT INTO deployments (id, name, state) VALUES (1, 'a', 'stopped')",
		},
		"empty target": {
			"CREATE TABLE `deployments` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text,`state` text,`desired_state` text)",
			"CREATE INDEX `idx_deployments_state` ON `deployments`(`state`)",
			"INSERT INTO deployments (id, name, state, desired_state) VALUES (1, 'a', 'stopped', '')",
		},
		"already current": {
			"CREATE TABLE `deployments` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text,`desired_state` text)",
			"CREATE INDEX `idx_deployments_state` ON `deployments`(`desired_state`)",
			"INSERT INTO deployments (id, name, desired_state) VALUES (1, 'a', 'stopped')",
		},
	}
	for name, statements := range cases {
		t.Run(name, func(t *testing.T) {
			db := openUpgradeCompatibilityDB(t, statements...)
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatal(err)
			}
			if err := m.Apply(db); err != nil {
				t.Fatalf("second application: %v", err)
			}
			if got := columnsOf(t, db, "deployments"); !reflect.DeepEqual(got, map[string]bool{"id": true, "name": true, "desired_state": true}) {
				t.Fatalf("deployments columns = %v", got)
			}
			schema := schemaOf(t, db)
			if got := schema["index idx_deployments_state"]; got != "createindexidx_deployments_stateondeployments(desired_state)" {
				t.Fatalf("idx_deployments_state = %q", got)
			}
			var desired string
			if err := db.Raw("SELECT desired_state FROM deployments WHERE id = 1").Scan(&desired).Error; err != nil || desired != "stopped" {
				t.Fatalf("desired_state=%q err=%v", desired, err)
			}
		})
	}
}

// Without these tables (the store's own partial fixtures) there is nothing
// to repair, and the migration must not create anything.
func TestLegacyLeftoversMigrationSkipsMissingTables(t *testing.T) {
	db := openUpgradeCompatibilityDB(t)
	before := schemaOf(t, db)
	if err := migrateDB(db, []migration{migrationByName(t, "legacy_column_and_index_leftovers")}); err != nil {
		t.Fatal(err)
	}
	assertSchemaMatches(t, schemaOf(t, db), before)
}
