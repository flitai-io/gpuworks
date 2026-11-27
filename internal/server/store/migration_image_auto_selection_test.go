package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestManualSelectionMigrationPreservesExistingPolicy(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:manual-only-migration?mode=memory&cache=shared"), &gorm.Config{})
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
	m := migrationByVersion(t, 250)
	injected := errors.New("after adding the manual selection column")
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
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 250).Count(&count).Error; err != nil || count != 0 || db.Migrator().HasColumn(&Image{}, "AutoSelect") {
		t.Fatalf("rollback marker=%d err=%v", count, err)
	}
	for range 2 {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	var marker SchemaMigration
	if err := db.First(&marker, 250).Error; err != nil || marker.Name != m.Name || !db.Migrator().HasColumn(&Image{}, "AutoSelect") {
		t.Fatalf("marker=%+v err=%v", marker, err)
	}
	var row Image
	if err := db.First(&row).Error; err != nil || row.SizeBytes != 8192 || row.AutoSelect != "" {
		t.Fatalf("legacy size=%d policy=%q err=%v", row.SizeBytes, row.AutoSelect, err)
	}
}

func TestImageAutoSelectionUpgradesRecorded249(t *testing.T) {
	for _, column := range []string{"manual_only BOOLEAN", "auto_select TEXT"} {
		t.Run(column, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open("file:selection-"+strings.Fields(column)[0]+"?mode=memory&cache=shared"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			conn, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			// Replay the production schema before 250. Old migrations use the
			// current Image type, so restore the selection column of the reviewed build.
			i := 0
			for i < len(migrations) && migrations[i].Version < 249 {
				i++
			}
			if err := migrateDB(db, migrations[:i]); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("ALTER TABLE image_versions DROP COLUMN auto_select").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("ALTER TABLE image_versions ADD COLUMN " + column).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&SchemaMigration{Version: 249, Name: "image_manual_selection"}).Error; err != nil {
				t.Fatal(err)
			}
			want := "manual"
			var stored any = true
			if column == "auto_select TEXT" {
				want = "arm64"
				stored = want
			}
			if err := db.Exec("INSERT INTO image_versions(image_ref,"+strings.Fields(column)[0]+") VALUES('example/runner:r1',?),('example/runner:r2',NULL)", stored).Error; err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := migrateDB(db, []migration{migrationByVersion(t, 250)}); err != nil {
					t.Fatal(err)
				}
			}
			var rows []Image
			if err := db.Where("image_ref IN ?", []string{"example/runner:r1", "example/runner:r2"}).Order("id").Find(&rows).Error; err != nil || len(rows) != 2 || rows[0].AutoSelect != want || rows[1].AutoSelect != "" {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
		})
	}
}
