package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestModelDownloadDeploymentMigration(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "idempotent", true: "rollback"}[fail], func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			t.Cleanup(func() { _ = sqlDB.Close() })
			if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec(`CREATE TABLE model_files (id INTEGER PRIMARY KEY, state TEXT);`).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec(`INSERT INTO model_files(id,state) VALUES(1,'downloading')`).Error; err != nil {
				t.Fatal(err)
			}
			m := migrationByName(t, "model_download_deployment")
			original := m.Apply
			if fail {
				m.Apply = func(tx *gorm.DB) error {
					if err := original(tx); err != nil {
						return err
					}
					return errors.New("injected migration failure")
				}
			}
			err = migrateDB(db, []migration{m})
			if fail {
				if err == nil || db.Migrator().HasColumn(&ModelFile{}, "DownloadDeploymentID") {
					t.Fatalf("schema escaped rollback: err=%v", err)
				}
				var markers int64
				db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers)
				if markers != 0 {
					t.Fatal("marker escaped rollback")
				}
				m.Apply = original
			} else if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := migrateDB(db, []migration{m}); err != nil {
					t.Fatal(err)
				}
			}
			if !db.Migrator().HasIndex(&ModelFile{}, "DownloadDeploymentID") {
				t.Fatal("owner lookup index missing")
			}
			var row struct {
				DownloadDeploymentID uint
				State                string
			}
			if err := db.Table("model_files").First(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.DownloadDeploymentID != 0 || row.State != "downloading" {
				t.Fatalf("existing download changed: %+v", row)
			}
			var markers int64
			db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers)
			if markers != 1 {
				t.Fatalf("markers=%d", markers)
			}
		})
	}
}
