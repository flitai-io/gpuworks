package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestBatchAndBenchmarkPriorityClassMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{
			"CREATE TABLE batches (id TEXT PRIMARY KEY, status TEXT)",
			"INSERT INTO batches (id, status) VALUES ('batch_old', 'in_progress')",
			"CREATE TABLE benchmark_runs (id INTEGER PRIMARY KEY, name TEXT)",
			"INSERT INTO benchmark_runs (id, name) VALUES (1, 'old')",
		} {
			if err := db.Exec(stmt).Error; err != nil {
				t.Fatal(err)
			}
		}
		return db
	}
	m := migrationByVersion(t, 215)
	has := func(db *gorm.DB) bool {
		return db.Migrator().HasColumn(&Batch{}, "PriorityClass") && db.Migrator().HasColumn(&BenchmarkRun{}, "PriorityClass")
	}

	db := open("batch-benchmark-priority")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !has(db) {
		t.Fatal("priority_class was not added")
	}
	// Rows from before keep the empty name: a batch at the batch class, a
	// run that asked for none. So does a row an older binary inserts.
	if err := db.Exec("INSERT INTO batches (id, status) VALUES ('batch_later', 'validating')").Error; err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"SELECT priority_class FROM batches WHERE id = 'batch_old'",
		"SELECT priority_class FROM batches WHERE id = 'batch_later'",
		"SELECT priority_class FROM benchmark_runs WHERE id = 1",
	} {
		var got *string
		if err := db.Raw(q).Scan(&got).Error; err != nil || got == nil || *got != "" {
			t.Fatalf("%s = %v err=%v", q, got, err)
		}
	}
	if err := db.Exec("INSERT INTO batches (id, status, priority_class) VALUES ('batch_null', 'validating', NULL)").Error; err == nil {
		t.Fatal("priority_class accepted NULL")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 215).Error; err != nil || marker.Name != "batch_and_benchmark_priority_class" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 215").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("batch-benchmark-priority-rollback")
	want := errors.New("injected priority_class failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Batch{}, "PriorityClass") || rollback.Migrator().HasColumn(&BenchmarkRun{}, "PriorityClass") {
		t.Fatal("a column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 215).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !has(rollback) {
		t.Fatalf("restart after rollback: err=%v", err)
	}

	// A database without the tables yet has nothing to add to.
	empty, err := gorm.Open(sqlite.Open("file:batch-benchmark-priority-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(empty, []migration{m}); err != nil || empty.Migrator().HasTable(&Batch{}) {
		t.Fatalf("without the tables: err=%v", err)
	}
}
