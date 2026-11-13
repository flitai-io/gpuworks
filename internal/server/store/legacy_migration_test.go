package store

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestRemainingMigrationsPreserveLegacyNodeGPUAndTopologyFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	legacySQL := []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT, token_hash TEXT, hostname TEXT, state TEXT, nvidia_driver TEXT, topo_json TEXT)`,
		`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, "index" INTEGER, uuid TEXT, name TEXT, memory_total INTEGER, mig_enabled INTEGER)`,
		`CREATE TABLE models (id INTEGER PRIMARY KEY, image_id INTEGER, source TEXT, source_id TEXT, revision TEXT, name TEXT)`,
		`INSERT INTO nodes (id, uuid, token_hash, hostname, state, nvidia_driver, topo_json) VALUES (1, 'legacy-node', 'hash', 'legacy-host', 'online', '535.129', '{"links":[{"a":0,"b":1,"kind":"nvlink"}]}')`,
		`INSERT INTO gpus (id, node_id, "index", uuid, name, memory_total, mig_enabled) VALUES (1, 1, 0, 'legacy-gpu', 'A100', 85899345920, 1)`,
	}
	for _, statement := range legacySQL {
		if err := gdb.Exec(statement).Error; err != nil {
			t.Fatalf("legacy schema statement %q: %v", statement, err)
		}
	}
	for version := 1; version <= 20; version++ {
		if err := gdb.Exec("INSERT INTO schema_migrations(version,name) VALUES(?,?)", version, "legacy").Error; err != nil {
			t.Fatal(err)
		}
	}
	st := &Store{db: gdb, Bus: bus.New()}
	if err := st.migrate(); err != nil {
		t.Fatalf("remaining migrations: %v", err)
	}
	// The point of this assertion is that a legacy database migrates all the way
	// to head, so head comes from the registry rather than a literal that has to
	// be bumped by every new migration.
	head := migrations[len(migrations)-1].Version
	version, err := st.CurrentMigration()
	if err != nil || version != head {
		t.Fatalf("current migration = %d, err=%v, want %d", version, err, head)
	}
	node, err := st.NodeWithGPUs("legacy-node")
	if err != nil {
		t.Fatal(err)
	}
	if node.NvidiaDriver != "535.129" || node.TopoJSON == "" {
		t.Fatalf("legacy node fields lost: %+v", node)
	}
	if len(node.GPUs) != 1 || !node.GPUs[0].MIGEnabled || node.GPUs[0].MemoryTotal != 85899345920 {
		t.Fatalf("legacy GPU fields lost: %+v", node.GPUs)
	}
	if !st.db.Migrator().HasColumn(&GPU{}, "PowerLimitWatts") ||
		!st.db.Migrator().HasColumn(&ImageNode{}, "Source") ||
		!st.db.Migrator().HasColumn(&Node{}, "ServerURL") ||
		!st.db.Migrator().HasColumn(&Instance{}, "Role") ||
		st.db.Migrator().HasColumn("instances", "image_defaults_snapshot") {
		t.Fatal("remaining migrations did not add newest compatibility columns")
	}
}

func TestBenchmarkCleanupReconcilesOccupiedMigrationVersions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "occupied-migrations.db")
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE jobs (id INTEGER PRIMARY KEY, kind TEXT)`,
	} {
		if err := gdb.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	for version := 1; version <= 41; version++ {
		if err := gdb.Exec("INSERT INTO schema_migrations(version,name) VALUES(?,?)", version, "occupied").Error; err != nil {
			t.Fatal(err)
		}
	}
	st := &Store{db: gdb, Bus: bus.New()}
	if err := st.migrate(); err != nil {
		t.Fatal(err)
	}
	if st.db.Migrator().HasTable("jobs") {
		t.Fatal("legacy jobs table survived benchmark cleanup reconciliation")
	}
	for _, table := range []any{&BenchmarkRun{}, &BenchmarkPoint{}} {
		if !st.db.Migrator().HasTable(table) {
			t.Fatalf("benchmark table for %T was not reconciled", table)
		}
	}
	if st.db.Migrator().HasTable("benchmark_baselines") {
		t.Fatal("removed benchmark_baselines table was recreated")
	}
	version, err := st.CurrentMigration()
	head := migrations[len(migrations)-1].Version
	if err != nil || version != head {
		t.Fatalf("current migration = %d, err=%v, want %d", version, err, head)
	}
}

func TestOutOfBandRecoveryMigratesWhenVersion50IsOccupied(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "occupied-v50.db")
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT)`,
		`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, "index" INTEGER, uuid TEXT)`,
	} {
		if err := gdb.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	for version := 1; version <= 50; version++ {
		name := "occupied"
		if version == 50 {
			name = "partition_capability_framework"
		}
		if err := gdb.Exec("INSERT INTO schema_migrations(version,name) VALUES(?,?)", version, name).Error; err != nil {
			t.Fatal(err)
		}
	}

	st := &Store{db: gdb, Bus: bus.New()}
	if err := st.migrate(); err != nil {
		t.Fatal(err)
	}
	if !st.db.Migrator().HasTable(&NodeSSHCredential{}) {
		t.Fatal("node SSH credential table was not created after occupied v50")
	}
	if st.db.Migrator().HasColumn("gpus", "partition_mode") {
		t.Fatal("legacy GPU partition mode survived occupied v50 reconciliation")
	}
	for _, column := range []string{"RecoveryPhase", "RecoveryAttempts", "LastProbeAt", "LastRecoveryError"} {
		if !st.db.Migrator().HasColumn(&Node{}, column) {
			t.Fatalf("node recovery column %s was not created after occupied v50", column)
		}
	}
	var migration SchemaMigration
	if err := st.db.First(&migration, "version = ?", 51).Error; err != nil {
		t.Fatalf("migration 51 missing: %v", err)
	}
	if migration.Name != "node_out_of_band_recovery" {
		t.Fatalf("migration 51 name = %q", migration.Name)
	}
}

func TestMigrationsHaveUniqueVersions(t *testing.T) {
	seen := make(map[int]string, len(migrations))
	for _, m := range migrations {
		if previous, ok := seen[m.Version]; ok {
			t.Fatalf("migration version %d is used by %q and %q", m.Version, previous, m.Name)
		}
		seen[m.Version] = m.Name
	}
}
