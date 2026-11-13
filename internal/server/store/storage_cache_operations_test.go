package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestCacheOperationGenerationClearOwnershipAndRecovery(t *testing.T) {
	f := newStorageFixture(t)
	request := func(mode string, explicit bool) *StorageCacheOperation {
		t.Helper()
		if err := f.st.RequestCacheOperations(f.cache.ID, []uint{f.n1.ID}, mode, explicit); err != nil {
			t.Fatal(err)
		}
		op, err := f.st.CacheOperation(f.cache.ID, f.n1.ID)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	clear := request("clear", true)
	if _, err := f.st.SetCacheOperationResult(clear, "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RequestCachePolicyTrim(f.cache.ID, []uint{f.n1.ID}); err != nil {
		t.Fatal(err)
	}
	if op := request("trim", false); op.Mode != "clear" || op.Generation != clear.Generation {
		t.Fatalf("automatic trim replaced clear: %+v", op)
	}
	if err := f.st.RecoverCacheOperations(); err != nil {
		t.Fatal(err)
	}
	op, _ := f.st.CacheOperation(f.cache.ID, f.n1.ID)
	if op.State != "pending" {
		t.Fatalf("recovery=%+v", op)
	}
	newer := request("clear", true)
	if ok, err := f.st.SetCacheOperationResult(clear, "completed", ""); err != nil || ok {
		t.Fatalf("stale complete=%v %v", ok, err)
	}
	if _, err := f.st.SetCacheOperationResult(newer, "failed", "permission denied"); err != nil {
		t.Fatal(err)
	}
	if op := request("trim", false); op.State != "failed" || op.Error != "permission denied" {
		t.Fatalf("failure lost=%+v", op)
	}
	if _, err := f.st.SetCacheOperationResult(newer, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if op := request("trim", false); op.Mode != "trim" || op.Generation <= newer.Generation {
		t.Fatalf("clear replayed=%+v", op)
	}
}

func TestCacheOperationMigrationTransactionalAndIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE nodes (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	injected := errors.New("migration interrupted")
	m := migration{Version: 161, Name: "storage_cache_operations", Apply: func(tx *gorm.DB) error {
		if err := createStorageCacheOperations(tx); err != nil {
			return err
		}
		return injected
	}}
	if err := applyMigration(db, m); !errors.Is(err, injected) {
		t.Fatalf("failure=%v", err)
	}
	if db.Migrator().HasTable(&StorageCacheOperation{}) || db.Migrator().HasColumn("nodes", "path_usage_observed_at") {
		t.Fatal("schema survived rollback")
	}
	var count int64
	db.Model(&SchemaMigration{}).Where("version = ?", 161).Count(&count)
	if count != 0 {
		t.Fatal("marker survived rollback")
	}
	m.Apply = createStorageCacheOperations
	for i := 0; i < 2; i++ {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	if !db.Migrator().HasColumn("nodes", "path_usage_observed_at") {
		t.Fatal("disk report observation column absent")
	}
	if !db.Migrator().HasColumn(&StorageCacheOperation{}, "generation") {
		t.Fatal("generation absent")
	}
	db.Model(&SchemaMigration{}).Where("version = ?", 161).Count(&count)
	if count != 1 {
		t.Fatalf("markers=%d", count)
	}
}

func TestCacheEvictionRechecksTransferBackingAndDirectory(t *testing.T) {
	f := newStorageFixture(t)
	f.report(t, f.n1, proto.StorageStatus{ID: f.cache.ID, State: proto.StorageStateReady, CachePath: f.cache.CachePath})
	shared, err := f.st.UpsertModelFile(&ModelFile{StorageID: f.cache.ID, ModelID: f.model.ID, State: proto.MFPresent, Path: "model"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := f.st.UpsertModelFile(&ModelFile{StorageID: f.cache.ID, NodeID: f.n1.ID, ModelID: f.model.ID, State: proto.MFPresent, Path: f.cache.CachePath + "/model"})
	if err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		_, err := f.st.CacheEvictionFile(f.cache.ID, f.n1.ID, file.ID, file.Path)
		if (err == nil) != want {
			t.Fatalf("allowed=%v err=%v", want, err)
		}
	}
	check(true)
	f.st.db.Model(file).Update("state", proto.MFSyncing)
	check(false)
	f.st.db.Model(file).Update("state", proto.MFPresent)
	f.st.db.Model(shared).Update("state", proto.MFFailed)
	check(false)
	f.st.db.Model(shared).Update("state", proto.MFPresent)
	f.st.db.Model(f.cache).Update("cache_path", "/new-cache")
	check(false)
}
