package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func newNFSCacheStorage(t *testing.T, s *Store) *Storage {
	t.Helper()
	st, err := s.CreateStorage(&Storage{Name: "nfsc", Type: proto.StorageNFSCache, Export: "nfs.example.internal:/models", Enabled: true, AllNodes: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func newSwitchNodes(t *testing.T, s *Store, names ...string) []*Node {
	t.Helper()
	out := make([]*Node, 0, len(names))
	for _, name := range names {
		node, _, err := s.CreateNodePending(name)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, node)
	}
	return out
}

func switchToNFS(s *Store, id uint) (*Storage, error) {
	nfs := proto.StorageNFS
	return s.UpdateStorage(id, StoragePatch{Type: &nfs})
}

// Switching to nfs used to delete every node cache row while the files stayed
// on the nodes, where nothing could remove them any more, and even rows of
// copies and downloads still running. The switch now refuses while any entry
// in a node's current cache directory remains and leaves every row as it was.
// Only copies and downloads in progress count as transferring.
func TestSwitchNFSCacheToNFSRefusesWhileNodeCachesHoldEntries(t *testing.T) {
	cases := []struct {
		name         string
		rows         func(st *Storage, n []*Node) []ModelFile
		nodes        int
		transferring int
	}{
		{"cached copy", func(st *Storage, n []*Node) []ModelFile {
			return []ModelFile{{ModelID: 1, NodeID: n[0].ID, State: proto.MFPresent, Path: st.CachePath + "/m1", SizeBytes: 100 << 30}}
		}, 1, 0},
		{"copy running", func(st *Storage, n []*Node) []ModelFile {
			return []ModelFile{{ModelID: 1, NodeID: n[1].ID, State: proto.MFSyncing, Path: "m1", TransferID: "copy"}}
		}, 1, 1},
		{"download running", func(st *Storage, n []*Node) []ModelFile {
			return []ModelFile{{ModelID: 2, NodeID: n[0].ID, State: proto.MFDownloading, Path: "m2", TransferID: "dl"}}
		}, 1, 1},
		{"failed copy is not transferring", func(st *Storage, n []*Node) []ModelFile {
			return []ModelFile{{ModelID: 2, NodeID: n[0].ID, State: proto.MFFailed, Path: "m2", TransferID: "dl"}}
		}, 1, 0},
		{"several nodes", func(st *Storage, n []*Node) []ModelFile {
			return []ModelFile{
				{ModelID: 1, NodeID: n[0].ID, State: proto.MFPresent, Path: st.CachePath + "/m1"},
				{ModelID: 2, NodeID: n[0].ID, State: proto.MFDownloading, Path: "m2", TransferID: "dl"},
				{ModelID: 1, NodeID: n[1].ID, State: proto.MFSyncing, Path: "m1", TransferID: "copy"},
			}
		}, 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			st := newNFSCacheStorage(t, s)
			nodes := newSwitchNodes(t, s, "a", "b")
			shared := ModelFile{ModelID: 1, StorageID: st.ID, State: proto.MFPresent, Path: "m1"}
			if err := s.db.Create(&shared).Error; err != nil {
				t.Fatal(err)
			}
			rows := tc.rows(st, nodes)
			wantUUIDs := map[string]bool{}
			for i := range rows {
				rows[i].StorageID = st.ID
				for _, n := range nodes {
					if n.ID == rows[i].NodeID {
						wantUUIDs[n.UUID] = true
					}
				}
				if err := s.db.Create(&rows[i]).Error; err != nil {
					t.Fatal(err)
				}
			}
			_, err := switchToNFS(s, st.ID)
			var notEmpty *StorageCacheNotEmptyError
			if !errors.As(err, &notEmpty) {
				t.Fatalf("switch with node cache entries: err = %v", err)
			}
			if notEmpty.Entries != len(rows) || len(notEmpty.NodeUUIDs) != tc.nodes || notEmpty.Transferring != tc.transferring {
				t.Fatalf("refusal = %+v, want %d entries on %d nodes, %d transferring", notEmpty, len(rows), tc.nodes, tc.transferring)
			}
			for _, uuid := range notEmpty.NodeUUIDs {
				if !wantUUIDs[uuid] {
					t.Fatalf("refusal names node %s, which holds no entry", uuid)
				}
			}
			after, err := s.StorageByID(st.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Type != proto.StorageNFSCache || !after.UpdatedAt.Equal(st.UpdatedAt) {
				t.Fatalf("refused switch changed the storage: type=%s updated_at %v -> %v", after.Type, st.UpdatedAt, after.UpdatedAt)
			}
			var count int64
			s.db.Model(&ModelFile{}).Where("storage_id = ?", st.ID).Count(&count)
			if count != int64(len(rows)+1) {
				t.Fatalf("refused switch removed rows: %d left, want %d", count, len(rows)+1)
			}
		})
	}
}

func TestSwitchNFSCacheToNFSSucceedsOnceNodeCachesAreEmpty(t *testing.T) {
	s := newTestStore(t)
	st := newNFSCacheStorage(t, s)
	shared := ModelFile{ModelID: 1, StorageID: st.ID, State: proto.MFPresent, Path: "m1"}
	if err := s.db.Create(&shared).Error; err != nil {
		t.Fatal(err)
	}
	out, err := switchToNFS(s, st.ID)
	if err != nil {
		t.Fatalf("switch with empty node caches: %v", err)
	}
	if out.Type != proto.StorageNFS {
		t.Fatalf("type = %s", out.Type)
	}
	// The storage's own files on the NFS are not node caches and stay.
	if row, err := s.ModelFileByID(shared.ID); err != nil || row.State != proto.MFPresent {
		t.Fatalf("shared row after switch = %+v, %v", row, err)
	}
	// nfs does not send a cache directory to the agents.
	node := newSwitchNodes(t, s, "n1")[0]
	tasks, err := s.StorageTasksForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == st.ID && (task.Type != proto.StorageNFS || task.CachePath != "") {
			t.Fatalf("nfs task still carries cache: %+v", task)
		}
	}
	// Switching back is allowed and keeps the cache directory.
	cache := proto.StorageNFSCache
	back, err := s.UpdateStorage(st.ID, StoragePatch{Type: &cache})
	if err != nil || back.Type != proto.StorageNFSCache || back.CachePath != st.CachePath {
		t.Fatalf("switch back = %+v, %v", back, err)
	}
}

// Entries left in a node's previous cache directory are no longer managed
// and no cleanup can reach them, so they do not block the switch: their rows
// go with it. An entry in the current directory still blocks.
func TestSwitchNFSCacheToNFSDropsEntriesLeftInAnOldCacheDirectory(t *testing.T) {
	s := newTestStore(t)
	st := newNFSCacheStorage(t, s)
	node := newSwitchNodes(t, s, "a")[0]
	old := ModelFile{ModelID: 1, StorageID: st.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/old/cache/m1"}
	if err := s.db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	current := ModelFile{ModelID: 2, StorageID: st.ID, NodeID: node.ID, State: proto.MFFailed, Path: "m2", TransferID: "dl"}
	if err := s.db.Create(&current).Error; err != nil {
		t.Fatal(err)
	}
	var notEmpty *StorageCacheNotEmptyError
	if _, err := switchToNFS(s, st.ID); !errors.As(err, &notEmpty) || notEmpty.Entries != 1 {
		t.Fatalf("switch with a current entry = %v, want one blocking entry", err)
	}
	if _, err := s.ModelFileByID(old.ID); err != nil {
		t.Fatalf("a refused switch must not drop the old-directory row: %v", err)
	}
	if err := s.db.Delete(&current).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := switchToNFS(s, st.ID); err != nil {
		t.Fatalf("switch with only old-directory entries: %v", err)
	}
	if _, err := s.ModelFileByID(old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old-directory row after switch: %v", err)
	}
}

// A node the storage no longer applies to no longer gets the storage, so its
// cache rows are as unmanaged as an old directory's and go with the switch;
// a node still in scope blocks.
func TestSwitchNFSCacheToNFSDropsEntriesOfNodesOutOfScope(t *testing.T) {
	s := newTestStore(t)
	st := newNFSCacheStorage(t, s)
	nodes := newSwitchNodes(t, s, "kept", "dropped")
	kept, dropped := nodes[0], nodes[1]
	stale := ModelFile{ModelID: 1, StorageID: st.ID, NodeID: dropped.ID, State: proto.MFPresent, Path: st.CachePath + "/m1"}
	current := ModelFile{ModelID: 1, StorageID: st.ID, NodeID: kept.ID, State: proto.MFPresent, Path: st.CachePath + "/m1"}
	for _, row := range []*ModelFile{&stale, &current} {
		if err := s.db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	no := false
	if _, err := s.UpdateStorage(st.ID, StoragePatch{AllNodes: &no, Targets: &[]StorageNode{{NodeID: kept.ID}}}); err != nil {
		t.Fatal(err)
	}
	var notEmpty *StorageCacheNotEmptyError
	if _, err := switchToNFS(s, st.ID); !errors.As(err, &notEmpty) || notEmpty.Entries != 1 || len(notEmpty.NodeUUIDs) != 1 || notEmpty.NodeUUIDs[0] != kept.UUID {
		t.Fatalf("switch = %v, want only the in-scope node's entry to block", err)
	}
	if err := s.db.Delete(&current).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := switchToNFS(s, st.ID); err != nil {
		t.Fatalf("switch with only an out-of-scope node's entry: %v", err)
	}
	if _, err := s.ModelFileByID(stale.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope row after switch: %v", err)
	}
}

// A node's own cache directory override decides what is current for that node.
func TestSwitchNFSCacheToNFSUsesEachNodesCacheDirectory(t *testing.T) {
	s := newTestStore(t)
	st := newNFSCacheStorage(t, s)
	node := newSwitchNodes(t, s, "a")[0]
	if err := s.db.Create(&StorageNode{StorageID: st.ID, NodeID: node.ID, OverrideOnly: true, CachePath: "/data/own-cache"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&ModelFile{ModelID: 1, StorageID: st.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/data/own-cache/m1"}).Error; err != nil {
		t.Fatal(err)
	}
	var notEmpty *StorageCacheNotEmptyError
	if _, err := switchToNFS(s, st.ID); !errors.As(err, &notEmpty) || notEmpty.Entries != 1 {
		t.Fatalf("an entry in the node's own directory must block: %v", err)
	}
}

// A deletion still awaiting its node's confirmation is refused by the existing
// idle check before the cache check runs; either way nothing changes.
func TestSwitchNFSCacheToNFSRefusesWhileADeletionIsPending(t *testing.T) {
	s := newTestStore(t)
	st := newNFSCacheStorage(t, s)
	node := newSwitchNodes(t, s, "a")[0]
	if err := s.db.Create(&ModelFile{ModelID: 3, StorageID: st.ID, NodeID: node.ID, State: proto.MFDeleting, Path: st.CachePath + "/m3", TransferID: "evict"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := switchToNFS(s, st.ID); !errors.Is(err, ErrCacheDeletionPending) {
		t.Fatalf("switch during a pending deletion: %v", err)
	}
	var count int64
	s.db.Model(&ModelFile{}).Where("storage_id = ?", st.ID).Count(&count)
	if after, _ := s.StorageByID(st.ID); after.Type != proto.StorageNFSCache || count != 1 {
		t.Fatalf("refused switch changed state: type=%s rows=%d", after.Type, count)
	}
}

// A cache fill reads the storage as nfs_cache before it creates its node row;
// if the switch commits in between, the row must not be created under nfs,
// where no check sees it and no agent manages the directory.
func TestNodeRowIsRefusedOnStoragesWithoutNodeCopies(t *testing.T) {
	s := newTestStore(t)
	st := newNFSCacheStorage(t, s)
	node := newSwitchNodes(t, s, "a")[0]
	ref := FileRef{ModelID: 1, StorageID: st.ID, NodeID: node.ID}
	if _, err := switchToNFS(s, st.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareModelCopy(ref, "local/m1-main"); !errors.Is(err, ErrNoNodeCache) {
		t.Fatalf("cache copy under nfs = %v, want ErrNoNodeCache", err)
	}
	if _, err := s.RegisterModelFile(ref, "/x/m1", 1); !errors.Is(err, ErrNoNodeCache) {
		t.Fatalf("node registration under nfs = %v, want ErrNoNodeCache", err)
	}
	var count int64
	s.db.Model(&ModelFile{}).Where("storage_id = ? AND node_id <> 0", st.ID).Count(&count)
	if count != 0 {
		t.Fatalf("%d node rows created under nfs", count)
	}
	// The same refs are fine on nfs_cache and on the shared row of nfs.
	cache := proto.StorageNFSCache
	if _, err := s.UpdateStorage(st.ID, StoragePatch{Type: &cache}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareModelCopy(ref, "local/m1-main"); err != nil {
		t.Fatalf("cache copy under nfs_cache: %v", err)
	}
	if _, err := s.RegisterModelFile(FileRef{ModelID: 2, StorageID: st.ID}, "m2", 1); err != nil {
		t.Fatalf("shared registration: %v", err)
	}
}
