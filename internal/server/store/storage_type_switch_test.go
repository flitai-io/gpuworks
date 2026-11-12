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

// Switching to nfs used to delete every node cache row while the files stayed
// on the nodes, where nothing could remove them any more, and even rows of
// copies and downloads still running. The switch now refuses while any node
// cache entry remains and leaves every row as it was.
func TestSwitchNFSCacheToNFSRefusesWhileNodeCachesHoldEntries(t *testing.T) {
	cases := []struct {
		name         string
		rows         []ModelFile
		transferring int
	}{
		{"cached copy", []ModelFile{{ModelID: 1, NodeID: 7, State: proto.MFPresent, Path: "m1", SizeBytes: 100 << 30}}, 0},
		{"copy running", []ModelFile{{ModelID: 1, NodeID: 8, State: proto.MFSyncing, Path: "m1", TransferID: "copy"}}, 1},
		{"download running", []ModelFile{{ModelID: 2, NodeID: 7, State: proto.MFDownloading, Path: "m2", TransferID: "dl"}}, 1},
		{"several nodes", []ModelFile{
			{ModelID: 1, NodeID: 7, State: proto.MFPresent, Path: "m1"},
			{ModelID: 2, NodeID: 7, State: proto.MFDownloading, Path: "m2", TransferID: "dl"},
			{ModelID: 1, NodeID: 8, State: proto.MFSyncing, Path: "m1", TransferID: "copy"},
		}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			st := newNFSCacheStorage(t, s)
			shared := ModelFile{ModelID: 1, StorageID: st.ID, State: proto.MFPresent, Path: "m1"}
			if err := s.db.Create(&shared).Error; err != nil {
				t.Fatal(err)
			}
			nodes := map[uint]bool{}
			for i := range tc.rows {
				tc.rows[i].StorageID = st.ID
				nodes[tc.rows[i].NodeID] = true
				if err := s.db.Create(&tc.rows[i]).Error; err != nil {
					t.Fatal(err)
				}
			}
			nfs := proto.StorageNFS
			_, err := s.UpdateStorage(st.ID, StoragePatch{Type: &nfs})
			var notEmpty *StorageCacheNotEmptyError
			if !errors.As(err, &notEmpty) {
				t.Fatalf("switch with node cache entries: err = %v", err)
			}
			if notEmpty.Entries != len(tc.rows) || len(notEmpty.NodeIDs) != len(nodes) || notEmpty.Transferring != tc.transferring {
				t.Fatalf("refusal = %+v, want %d entries on %d nodes, %d transferring", notEmpty, len(tc.rows), len(nodes), tc.transferring)
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
			if count != int64(len(tc.rows)+1) {
				t.Fatalf("refused switch removed rows: %d left, want %d", count, len(tc.rows)+1)
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
	nfs := proto.StorageNFS
	out, err := s.UpdateStorage(st.ID, StoragePatch{Type: &nfs})
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
	node, _, err := s.CreateNodePending("n1")
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.StorageTasksForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == st.ID && (task.Type != proto.StorageNFS || task.CachePath != "") {
			t.Fatalf("nfs task still carries cache: %+v", task)
		}
	}
	// Switching back is allowed and keeps a cache directory.
	cache := proto.StorageNFSCache
	back, err := s.UpdateStorage(st.ID, StoragePatch{Type: &cache})
	if err != nil || back.Type != proto.StorageNFSCache || back.CachePath == "" {
		t.Fatalf("switch back = %+v, %v", back, err)
	}
}

// A deletion still awaiting its node's confirmation is refused by the existing
// idle check before the emptiness check runs; either way nothing changes.
func TestSwitchNFSCacheToNFSRefusesWhileADeletionIsPending(t *testing.T) {
	s := newTestStore(t)
	st := newNFSCacheStorage(t, s)
	if err := s.db.Create(&ModelFile{ModelID: 3, StorageID: st.ID, NodeID: 9, State: proto.MFDeleting, Path: "m3", TransferID: "evict"}).Error; err != nil {
		t.Fatal(err)
	}
	nfs := proto.StorageNFS
	if _, err := s.UpdateStorage(st.ID, StoragePatch{Type: &nfs}); !errors.Is(err, ErrCacheDeletionPending) {
		t.Fatalf("switch during a pending deletion: %v", err)
	}
	var count int64
	s.db.Model(&ModelFile{}).Where("storage_id = ?", st.ID).Count(&count)
	if after, _ := s.StorageByID(st.ID); after.Type != proto.StorageNFSCache || count != 1 {
		t.Fatalf("refused switch changed state: type=%s rows=%d", after.Type, count)
	}
}

// The check and the type change share one transaction: an entry created for
// the storage cannot slip in between them.
func TestSwitchNFSCacheToNFSCountsEntriesInsideTheTransaction(t *testing.T) {
	s := newTestStore(t)
	st := newNFSCacheStorage(t, s)
	// A row the transaction sees but a pre-check outside it would also see;
	// what matters is that the refusal is raised by UpdateStorage itself and
	// leaves the type unchanged, so a caller cannot race a check and a patch.
	if err := s.db.Create(&ModelFile{ModelID: 1, StorageID: st.ID, NodeID: 5, State: proto.MFPending, Path: "m1"}).Error; err != nil {
		t.Fatal(err)
	}
	nfs := proto.StorageNFS
	if _, err := s.UpdateStorage(st.ID, StoragePatch{Type: &nfs}); err == nil {
		t.Fatal("pending cache entry did not block the switch")
	}
	if after, _ := s.StorageByID(st.ID); after.Type != proto.StorageNFSCache {
		t.Fatalf("type = %s after refusal", after.Type)
	}
}
