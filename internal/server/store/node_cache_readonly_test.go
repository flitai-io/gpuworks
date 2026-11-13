package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDisabledCacheDeletionProtections(t *testing.T) {
	for _, scenario := range []string{"safe", "transferring", "missing_source", "active_deployment", "old_directory", "scope_removed", "type_changed", "not_ready", "source_file"} {
		t.Run(scenario, func(t *testing.T) {
			f := newStorageFixture(t)
			no := false
			if _, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{Enabled: &no}); err != nil {
				t.Fatal(err)
			}
			f.report(t, f.n1, proto.StorageStatus{ID: f.cache.ID, State: proto.StorageStateReady, CachePath: f.cache.CachePath, Writable: false})
			shared, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID, State: proto.MFPresent, Path: "model"})
			if err != nil {
				t.Fatal(err)
			}
			file, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID, NodeID: f.n1.ID, State: proto.MFPresent, Path: f.cache.CachePath + "/model"})
			if err != nil {
				t.Fatal(err)
			}
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "transferring":
				must(f.st.db.Model(file).Update("state", proto.MFSyncing).Error)
			case "missing_source":
				must(f.st.db.Model(shared).Update("state", proto.MFFailed).Error)
			case "active_deployment":
				_, err := f.st.CreateDeployment(&Deployment{Name: "d", ServedName: "d", ModelID: f.model.ID, NodeID: f.n1.ID, SaveToStorageID: f.cache.ID})
				must(err)
			case "old_directory":
				must(f.st.db.Model(f.cache).Update("cache_path", "/other-cache").Error)
			case "scope_removed":
				_, err := f.st.UpdateStorage(f.cache.ID, StoragePatch{AllNodes: &no, Targets: &[]StorageNode{{NodeID: f.n2.ID}}})
				must(err)
			case "type_changed":
				// UpdateStorage refuses this switch while the cache holds the
				// entry; the eviction guard must still refuse a storage whose
				// type is no longer nfs_cache, however it changed.
				must(f.st.db.Model(f.cache).Update("type", proto.StorageNFS).Error)
			case "not_ready":
				f.report(t, f.n1, proto.StorageStatus{ID: f.cache.ID, State: proto.StorageStateFailed, CachePath: f.cache.CachePath})
			case "source_file":
				file = shared
			}
			_, err = f.st.CacheEvictionFile(f.cache.ID, f.n1.ID, file.ID, file.Path)
			if scenario == "safe" {
				if err != nil {
					t.Fatalf("unused backed cache should be deletable: %v", err)
				}
			} else if err == nil {
				t.Fatalf("protected condition %s was allowed", scenario)
			}
		})
	}
}
