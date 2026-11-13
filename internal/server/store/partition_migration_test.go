package store

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestPartitionFrameworkMigrationFromVersion49IsIdempotent(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "partition-v49.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT, partition_mode TEXT)`,
		`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, "index" INTEGER, partition_mode TEXT)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, v_ram_limit_bytes INTEGER, partition_backend TEXT)`,
		`INSERT INTO nodes(id,uuid,partition_mode) VALUES(1,'n1','dgpu')`,
		`INSERT INTO gpus(id,node_id,"index",partition_mode) VALUES(1,1,0,'none')`,
		`INSERT INTO deployments(id,v_ram_limit_bytes,partition_backend) VALUES(1,8589934592,''),(2,0,'')`,
	} {
		if err := gdb.Exec(statement).Error; err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for version := 1; version <= 49; version++ {
		if err := gdb.Exec("INSERT INTO schema_migrations(version,name) VALUES(?,?)", version, "v49").Error; err != nil {
			t.Fatal(err)
		}
	}
	st := &Store{db: gdb, Bus: bus.New()}
	assertMigrated := func() {
		t.Helper()
		if err := st.migrate(); err != nil {
			t.Fatal(err)
		}
		if st.db.Migrator().HasColumn("nodes", "partition_mode") || st.db.Migrator().HasColumn("gpus", "partition_mode") {
			t.Fatal("legacy partition_mode column survived")
		}
		if !st.db.Migrator().HasColumn(&Node{}, "PartitionAssets") || !st.db.Migrator().HasColumn(&Node{}, "PartitionProbes") || !st.db.Migrator().HasColumn(&Deployment{}, "PartitionBackend") {
			t.Fatal("new partition framework columns are missing")
		}
		var sliced, whole Deployment
		if err := st.db.First(&sliced, 1).Error; err != nil {
			t.Fatal(err)
		}
		if err := st.db.First(&whole, 2).Error; err != nil {
			t.Fatal(err)
		}
		if sliced.PartitionBackend != "nvidia-dgpu" || whole.PartitionBackend != "" {
			t.Fatalf("backfill sliced=%q whole=%q", sliced.PartitionBackend, whole.PartitionBackend)
		}
	}
	assertMigrated()
	if err := st.db.Delete(&SchemaMigration{}, 50).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.db.Delete(&SchemaMigration{}, 51).Error; err != nil {
		t.Fatal(err)
	}
	assertMigrated()
}

func TestPartitionProbeMigrationReconcilesOccupiedVersion51And52(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "partition-occupied-v52.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, applied_at DATETIME)`,
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT, partition_assets TEXT, partition_mode TEXT)`,
		`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, "index" INTEGER, partition_mode TEXT)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, v_ram_limit_bytes INTEGER, partition_backend TEXT)`,
		`INSERT INTO nodes(id,uuid,partition_mode) VALUES(1,'occupied-node','dgpu')`,
		`INSERT INTO gpus(id,node_id,"index",partition_mode) VALUES(1,1,0,'none')`,
		`INSERT INTO deployments(id,v_ram_limit_bytes,partition_backend) VALUES(1,8589934592,'')`,
		`INSERT INTO schema_migrations(version,name) VALUES(50,'partition_capability_framework')`,
		`INSERT INTO schema_migrations(version,name) VALUES(51,'node_out_of_band_recovery')`,
		`INSERT INTO schema_migrations(version,name) VALUES(52,'blueprint_family_metadata')`,
	} {
		if err := gdb.Exec(statement).Error; err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	st := &Store{db: gdb, Bus: bus.New()}
	if err := st.migrate(); err != nil {
		t.Fatal(err)
	}
	if !st.db.Migrator().HasColumn(&Node{}, "PartitionProbes") {
		t.Fatal("version-occupied database did not gain nodes.partition_probes")
	}
	if st.db.Migrator().HasColumn("nodes", "partition_mode") || st.db.Migrator().HasColumn("gpus", "partition_mode") {
		t.Fatal("version-occupied database retained a legacy partition_mode column")
	}
	var deployment Deployment
	if err := st.db.First(&deployment, 1).Error; err != nil || deployment.PartitionBackend != "nvidia-dgpu" {
		t.Fatalf("version-occupied deployment backfill = %+v, err=%v", deployment, err)
	}
	var migration SchemaMigration
	if err := st.db.First(&migration, 57).Error; err != nil || migration.Name != "node_partition_probes_reconcile" {
		t.Fatalf("reconcile migration = %+v, err=%v", migration, err)
	}
	if err := st.migrate(); err != nil {
		t.Fatalf("reconcile migration is not idempotent: %v", err)
	}
}
