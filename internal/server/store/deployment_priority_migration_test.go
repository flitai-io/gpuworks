package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDeploymentPrioritySchedulingMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE deployments (id INTEGER PRIMARY KEY, name TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO deployments (id, name) VALUES (1, 'old')").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 213)

	db := open("deployment-priority")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Deployment{}, "PriorityScheduling") {
		t.Fatal("priority_scheduling was not added")
	}
	// A deployment from before the switch keeps running without it.
	var row struct {
		PriorityScheduling bool
	}
	if err := db.Raw("SELECT priority_scheduling FROM deployments WHERE id = 1").Scan(&row).Error; err != nil || row.PriorityScheduling {
		t.Fatalf("existing row = %+v err=%v", row, err)
	}
	// The column is NOT NULL with a false default: a row written without it
	// (an older binary's insert) is off too.
	if err := db.Exec("INSERT INTO deployments (id, name) VALUES (2, 'later')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("SELECT priority_scheduling FROM deployments WHERE id = 2").Scan(&row).Error; err != nil || row.PriorityScheduling {
		t.Fatalf("row without the column = %+v err=%v", row, err)
	}
	if err := db.Exec("INSERT INTO deployments (id, name, priority_scheduling) VALUES (3, 'null', NULL)").Error; err == nil {
		t.Fatal("priority_scheduling accepted NULL")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 213).Error; err != nil || marker.Name != "deployment_priority_scheduling" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 213").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("deployment-priority-rollback")
	want := errors.New("injected priority_scheduling failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&Deployment{}, "PriorityScheduling") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 213).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&Deployment{}, "PriorityScheduling") {
		t.Fatalf("restart after rollback: err=%v", err)
	}

	// A database without the deployments table yet has nothing to add to.
	empty, err := gorm.Open(sqlite.Open("file:deployment-priority-empty?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(empty, []migration{m}); err != nil || empty.Migrator().HasTable(&Deployment{}) {
		t.Fatalf("without deployments: err=%v", err)
	}
}
