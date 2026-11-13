package store

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestCacheOperationFailureSurvivesStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	st, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RequestCacheOperations(1, []uint{1}, "clear", true); err != nil {
		t.Fatal(err)
	}
	op, err := st.CacheOperation(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetCacheOperationResult(op, "failed", "permission denied"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.RecoverCacheOperations(); err != nil {
		t.Fatal(err)
	}
	op, err = st.CacheOperation(1, 1)
	if err != nil || op.State != "failed" || op.Error != "permission denied" {
		t.Fatalf("reopened=%+v err=%v", op, err)
	}
}

func TestCacheEntryBatchDoesNotQuerySharedFactsPerNode(t *testing.T) {
	f := newStorageFixture(t)
	if _, err := f.st.UpsertModelFile(&ModelFile{StorageID: f.cache.ID, ModelID: f.model.ID, State: proto.MFPresent, Path: "model"}); err != nil {
		t.Fatal(err)
	}
	ids := make([]uint, 1000)
	rows := make([]ModelFile, 1000)
	for i := range ids {
		ids[i] = uint(i + 1)
		rows[i] = ModelFile{StorageID: f.cache.ID, NodeID: ids[i], ModelID: f.model.ID, Path: f.cache.CachePath + "/model", State: proto.MFPresent, SizeBytes: 10}
	}
	if err := f.st.db.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int64
	name := "cache_batch_query_count"
	if err := f.st.db.Callback().Query().After("gorm:query").Register(name, func(*gorm.DB) { queries.Add(1) }); err != nil {
		t.Fatal(err)
	}
	defer f.st.db.Callback().Query().Remove(name)
	entries, err := f.st.NodeCacheEntriesForNodes(f.cache.ID, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1000 || queries.Load() > 7 {
		t.Fatalf("nodes=%d queries=%d", len(entries), queries.Load())
	}
	for id, rows := range entries {
		if len(rows) != 1 || !rows[0].Evictable() || rows[0].SizeBytes != 10 {
			t.Fatalf("node %d = %+v", id, rows)
		}
	}
}
