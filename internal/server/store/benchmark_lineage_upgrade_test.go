package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// historicalLineageDB writes a dumped historical schema (testdata, one
// statement per line) plus extra rows into a database file and closes it, so
// the store under test opens it the way a server start does.
func historicalLineageDB(t *testing.T, fixture string, rows ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	var statements []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
			statements = append(statements, line)
		}
	}
	for _, statement := range append(statements, rows...) {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("fixture statement %q: %v", statement, err)
		}
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertLineageConvergesOnFresh compares the whole schema, including the
// tables migrations 37-42 own and the leftovers 207 removes.
func assertLineageConvergesOnFresh(t *testing.T, upgraded, fresh *gorm.DB) {
	t.Helper()
	assertSchemaMatches(t, schemaOf(t, upgraded), schemaOf(t, fresh))
}

func markerNames(t *testing.T, db *gorm.DB) map[int]string {
	t.Helper()
	var rows []SchemaMigration
	if err := db.Order("version").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	names := make(map[int]string, len(rows))
	for _, row := range rows {
		if _, dup := names[row.Version]; dup {
			t.Fatalf("duplicate marker for version %d", row.Version)
		}
		names[row.Version] = row.Name
	}
	return names
}

// Rows the benchmark branch could hold: a custom series that mixes CUDA and
// CANN versions (39 splits it by inserting a sibling series), a version whose
// verification columns 38 drops, and an instance that 40 extends.
var benchmarkLineageRows = []string{
	"INSERT INTO image_series (id, name, engine, purpose, framework, source, enabled, created_at, updated_at) VALUES (900, 'custom/mixed', 'vllm', 'inference', 'cuda', 'custom', 1, '2026-08-23 12:00:00', '2026-08-23 12:00:00')",
	"INSERT INTO image_versions (id, series_id, name, engine, framework, image_ref, source, enabled, verify_state, verify_message, created_at, updated_at) VALUES (900, 900, 'cuda-build', 'vllm', 'cuda', 'registry.example/custom/vllm:cuda', 'custom', 1, 'verified', 'ok', '2026-08-23 12:00:00', '2026-08-23 12:00:00')",
	"INSERT INTO image_versions (id, series_id, name, engine, framework, image_ref, source, enabled, created_at, updated_at) VALUES (901, 900, 'cann-build', 'vllm', 'cann', 'registry.example/custom/vllm:cann', 'custom', 1, '2026-08-23 12:00:00', '2026-08-23 12:00:00')",
	"INSERT INTO instances (id, uuid, state, image, created_at, updated_at) VALUES (900, 'benchmark-instance', 'running', 'registry.example/custom/vllm:cuda', '2026-08-23 12:00:00', '2026-08-23 12:00:00')",
	"INSERT INTO benchmark_runs (id, name, state, num_gp_us, created_at, updated_at) VALUES (900, 'historical-run', 'succeeded', 2, '2026-08-23 12:00:00', '2026-08-23 12:00:00')",
}

// A database from the benchmark branch recorded 37, 38 and 40 under benchmark
// names, so the image migrations at those numbers were skipped and the image
// catalog written at startup failed on image_series.common_args.
func TestBenchmarkLineageV40UpgradeRepairsImageSchema(t *testing.T) {
	path := historicalLineageDB(t, "benchmark_lineage_v40.sql", benchmarkLineageRows...)

	st, err := Open(path, bus.New())
	if err != nil {
		t.Fatalf("open benchmark-lineage database: %v", err)
	}
	fresh := newTestStore(t)
	assertLineageConvergesOnFresh(t, st.db, fresh.db)
	for _, column := range []string{"common_args", "common_env"} {
		if !columnsOf(t, st.db, "image_series")[column] {
			t.Fatalf("image_series.%s missing after upgrade", column)
		}
	}
	if st.db.Migrator().HasIndex("image_versions", "idx_image_versions_verify_state") {
		t.Fatal("verification index from the benchmark branch survived the upgrade")
	}
	if st.db.Migrator().HasTable("jobs") || st.db.Migrator().HasTable("benchmark_baselines") {
		t.Fatal("benchmark cleanup did not run")
	}

	// The startup image catalog writes every series column.
	if _, err := st.UpsertImageSeries(&ImageSeries{Name: "official/runtime", Engine: "vllm", Framework: "cuda", Source: "official", Enabled: true, CommonArgs: `[{"flag":"--trust-remote-code"}]`, CommonEnv: "[]"}); err != nil {
		t.Fatalf("upsert image series after upgrade: %v", err)
	}
	series, err := st.ListImageSeries()
	if err != nil {
		t.Fatal(err)
	}
	frameworks := map[string]string{}
	for _, row := range series {
		if strings.HasPrefix(row.Name, "custom/mixed") {
			frameworks[row.Name] = row.Framework
		}
	}
	if want := map[string]string{"custom/mixed": "cuda", "custom/mixed · cann": "cann"}; !reflect.DeepEqual(frameworks, want) {
		t.Fatalf("mixed series split = %v, want %v", frameworks, want)
	}
	var inst Instance
	if err := st.db.First(&inst, 900).Error; err != nil || inst.UUID != "benchmark-instance" || inst.State != "running" {
		t.Fatalf("instance=%+v err=%v", inst, err)
	}
	var run BenchmarkRun
	if err := st.db.First(&run, 900).Error; err != nil || run.Name != "historical-run" {
		t.Fatalf("benchmark run=%+v err=%v", run, err)
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
	columnsBefore := map[string]map[string]bool{}
	for _, table := range []string{"image_series", "image_versions", "instances"} {
		columnsBefore[table] = columnsOf(t, st.db, table)
	}
	var seriesBefore int64
	if err := st.db.Model(&ImageSeries{}).Count(&seriesBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(path, bus.New())
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if got := markerNames(t, again.db); !reflect.DeepEqual(got, markers) {
		t.Fatalf("second start changed markers: %v", got)
	}
	for table, want := range columnsBefore {
		if got := columnsOf(t, again.db, table); !reflect.DeepEqual(got, want) {
			t.Fatalf("second start changed %s: %v, want %v", table, got, want)
		}
	}
	var seriesAfter int64
	if err := again.db.Model(&ImageSeries{}).Count(&seriesAfter).Error; err != nil || seriesAfter != seriesBefore {
		t.Fatalf("second start series count=%d err=%v, want %d", seriesAfter, err, seriesBefore)
	}
}

// An image-lineage database already ran 37-40 under their current names; the
// repair must leave those markers (and their timestamps) alone.
func TestImageLineageV40UpgradeKeepsAppliedImageMigrations(t *testing.T) {
	path := historicalLineageDB(t, "image_lineage_v40.sql",
		"INSERT INTO image_series (id, name, engine, purpose, framework, common_args, common_env, source, enabled) VALUES (900, 'custom/runtime', 'vllm', 'inference', 'cuda', '[{\"flag\":\"--x\"}]', '[]', 'custom', 1)")

	st, err := Open(path, bus.New())
	if err != nil {
		t.Fatalf("open image-lineage database: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fresh := newTestStore(t)
	assertLineageConvergesOnFresh(t, st.db, fresh.db)
	var applied []SchemaMigration
	if err := st.db.Where("version BETWEEN 37 AND 40").Order("version").Find(&applied).Error; err != nil || len(applied) != 4 {
		t.Fatalf("markers=%+v err=%v", applied, err)
	}
	historical := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	for _, marker := range applied {
		if marker.Name != migrationByVersion(t, marker.Version).Name || !marker.AppliedAt.Equal(historical) {
			t.Fatalf("image migration %d was re-applied: %+v", marker.Version, marker)
		}
	}
	got, err := st.GetImageSeries(900)
	if err != nil || got.CommonArgs != `[{"flag":"--x"}]` {
		t.Fatalf("series=%+v err=%v", got, err)
	}
	if v, err := st.CurrentMigration(); err != nil || v != migrations[len(migrations)-1].Version {
		t.Fatalf("current migration=%d err=%v", v, err)
	}
}

func TestBenchmarkLineageImageRepairRollsBackAndRetries(t *testing.T) {
	db := openUpgradeCompatibilityDB(t,
		"CREATE TABLE `image_series` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text,`engine` text,`purpose` text,`framework` text,`vendors` text,`source` text,`deprecated_since` text,`notes` text,`enabled` numeric,`created_at` datetime,`updated_at` datetime)",
		"CREATE TABLE `image_versions` (`id` integer PRIMARY KEY AUTOINCREMENT,`series_id` integer,`name` text,`verify_state` text)",
		"CREATE INDEX `idx_image_versions_verify_state` ON `image_versions`(`verify_state`)",
		"INSERT INTO schema_migrations (version, name) VALUES (37, 'benchmark_runs_points_baselines'), (38, 'drop_legacy_jobs')",
	)
	repairs := []migration{migrationByVersion(t, 37), migrationByVersion(t, 38)}
	want := errors.New("injected repair failure")
	for i, base := range repairs {
		failing := base
		failing.Apply = func(tx *gorm.DB) error {
			if err := base.Apply(tx); err != nil {
				return err
			}
			return want
		}
		attempt := append([]migration{}, repairs[:i]...)
		if err := migrateDB(db, append(attempt, failing)); !errors.Is(err, want) {
			t.Fatalf("repair %d error=%v, want %v", base.Version, err, want)
		}
		switch base.Version {
		case 37:
			if columnsOf(t, db, "image_series")["common_args"] {
				t.Fatal("failed repair of 37 left common_args behind")
			}
		case 38:
			if !columnsOf(t, db, "image_versions")["verify_state"] || !db.Migrator().HasIndex("image_versions", "idx_image_versions_verify_state") {
				t.Fatal("failed repair of 38 dropped the verification column")
			}
		}
		want37 := "benchmark_runs_points_baselines"
		if base.Version == 38 {
			want37 = "image_catalog_m20_fields" // repaired by the earlier attempt
		}
		if got := markerNames(t, db); got[37] != want37 || got[38] != "drop_legacy_jobs" {
			t.Fatalf("markers after failed repair %d: %v", base.Version, got)
		}
	}
	if err := migrateDB(db, repairs); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !columnsOf(t, db, "image_series")["common_args"] || columnsOf(t, db, "image_versions")["verify_state"] {
		t.Fatalf("retry schema: image_series=%v image_versions=%v", columnsOf(t, db, "image_series"), columnsOf(t, db, "image_versions"))
	}
	if got := markerNames(t, db); got[37] != "image_catalog_m20_fields" || got[38] != "remove_image_id_verification_state" {
		t.Fatalf("markers after retry: %v", got)
	}
}
