package cloud_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func TestCloudMigrationIsAtomicAndIdempotent(t *testing.T) {
	for _, migration := range []struct {
		version int
		name    string
		entity  any
	}{
		{135, "cloud_host_snapshots", &cloudstore.CloudHost{}},
		{136, "cloud_resource_leases", &cloudstore.CloudResourceLease{}},
	} {
		t.Run(migration.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "migration.db")
			s, err := store.Open(path, bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			var marker store.SchemaMigration
			if err := s.DB().First(&marker, migration.version).Error; err != nil || marker.Name != migration.name {
				t.Fatalf("migration registration = %+v, err=%v", marker, err)
			}
			if err := s.DB().Migrator().DropTable(migration.entity); err != nil {
				t.Fatal(err)
			}
			if err := s.DB().Delete(&store.SchemaMigration{}, migration.version).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.DB().Exec("CREATE TRIGGER fail_cloud_marker BEFORE INSERT ON schema_migrations BEGIN SELECT RAISE(ABORT, 'injected Cloud marker failure'); END").Error; err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := store.Open(path, bus.New())
			if err == nil {
				_ = reopened.Close()
				t.Fatal("migration ignored marker failure")
			}
			if !strings.Contains(err.Error(), "injected Cloud marker failure") {
				t.Fatal(err)
			}
			db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			if db.Migrator().HasTable(migration.entity) {
				t.Fatal("Cloud schema survived rollback")
			}
			var count int64
			if err := db.Model(&store.SchemaMigration{}).Where("version = ?", migration.version).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("Cloud migration marker survived rollback: count=%d err=%v", count, err)
			}
			if err := db.Exec("DROP TRIGGER fail_cloud_marker").Error; err != nil {
				t.Fatal(err)
			}
			if err := sqlDB.Close(); err != nil {
				t.Fatal(err)
			}

			for range 2 {
				reopened, err := store.Open(path, bus.New())
				if err != nil {
					t.Fatal(err)
				}
				func() {
					defer reopened.Close()
					if !reopened.DB().Migrator().HasTable(migration.entity) {
						t.Fatal("Cloud schema missing")
					}
					if err := reopened.DB().Model(&store.SchemaMigration{}).Where("version = ?", migration.version).Count(&count).Error; err != nil || count != 1 {
						t.Fatalf("Cloud migration markers=%d err=%v", count, err)
					}
					if !reopened.DB().Migrator().HasIndex(&cloudstore.CloudHost{}, "idx_cloud_host") ||
						!reopened.DB().Migrator().HasIndex(&cloudstore.CloudResourceLease{}, "idx_cloud_resource_leases_operation_id") {
						t.Fatal("Cloud identity constraints missing")
					}
				}()
			}
		})
	}
}
