package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDownloadSizeMigrationPreservesRuntimeSize(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:download-size-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE image_versions(id INTEGER PRIMARY KEY, size_bytes INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO image_versions(size_bytes) VALUES (8192)").Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 248)
	injected := errors.New("after adding the download column")
	bad := m
	bad.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return injected
	}
	if err := applyMigration(db, bad); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 248).Count(&count).Error; err != nil || count != 0 || db.Migrator().HasColumn(&Image{}, "DownloadBytes") {
		t.Fatalf("rollback marker=%d err=%v", count, err)
	}
	for range 2 {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	var marker SchemaMigration
	if err := db.First(&marker, 248).Error; err != nil || marker.Name != m.Name || !db.Migrator().HasColumn(&Image{}, "DownloadBytes") {
		t.Fatalf("marker=%+v err=%v", marker, err)
	}
	var row Image
	if err := db.First(&row).Error; err != nil || row.SizeBytes != 8192 || row.DownloadBytes != 0 {
		t.Fatalf("legacy sizes=%d/%d err=%v", row.SizeBytes, row.DownloadBytes, err)
	}
}
