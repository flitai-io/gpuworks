package store

import (
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// sourceFixture is the storage fixture plus a team NFS that applies to n1
// only, the shape of gpu3:8800's lab-nfs.
type sourceFixture struct {
	*storageFixture
	team *Storage
}

func newSourceFixture(t *testing.T) *sourceFixture {
	t.Helper()
	f := &sourceFixture{storageFixture: newStorageFixture(t)}
	var err error
	if f.team, err = f.st.CreateStorage(&Storage{Name: "lab-nfs", Type: proto.StorageNFS, Enabled: true,
		Path: "/mnt/gpuworks/lab-nfs", Export: "nfs.example.internal:/export/lab"}, []StorageNode{{NodeID: f.n1.ID}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *sourceFixture) file(t *testing.T, storage *Storage, node *Node, path, state string) *ModelFile {
	t.Helper()
	ref := ModelFile{ModelID: f.model.ID, StorageID: storage.ID, Path: path, State: state, SizeBytes: 15}
	if node != nil {
		ref.NodeID = node.ID
	}
	row, err := f.st.UpsertModelFile(&ref)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (f *sourceFixture) mount(t *testing.T, node *Node, storage *Storage, state string) {
	t.Helper()
	f.report(t, node, proto.StorageStatus{ID: storage.ID, State: state, Path: storage.Path, Writable: true, CachePath: "/opt/gpuworks/cache/" + storage.Name})
}

func (f *sourceFixture) source(t *testing.T, node *Node, rules SourceRules) ModelSource {
	t.Helper()
	src, err := f.st.ModelSourceOn(f.model.ID, node, rules)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// Deployment 267 on gpu3:8800: saved into lab-nfs, which compshare-3080 is
// not in, while compshare-3080's local storage holds the same model.
func TestModelSourceUsesAnyCopyTheNodeCanRead(t *testing.T) {
	f := newSourceFixture(t)
	f.file(t, f.team, nil, "Qwen/Qwen3-8B", proto.MFPresent)
	f.mount(t, f.n1, f.team, proto.StorageStateReady)
	f.file(t, f.local, f.n2, "/opt/gpuworks/models/q", proto.MFPresent)
	rules := SourceRules{SaveTo: f.team.ID}
	if src := f.source(t, f.n1, rules); src.Action != SourceReady || src.Location.HostPath != "/mnt/gpuworks/lab-nfs/Qwen/Qwen3-8B" {
		t.Fatalf("n1 = %+v", src)
	}
	if src := f.source(t, f.n2, rules); src.Action != SourceReady || src.Location.HostPath != "/opt/gpuworks/models/q" || src.Location.Storage.ID != f.local.ID {
		t.Fatalf("n2 = %+v, want its local copy", src)
	}
	// Limited to lab-nfs, n2 is not served at all.
	if src := f.source(t, f.n2, SourceRules{Pinned: []uint{f.team.ID}, SaveTo: f.team.ID}); src.Action != SourceRefused || src.Reason != LocationStorageNotOnNode || src.Location.Storage.ID != f.team.ID {
		t.Fatalf("pinned n2 = %+v", src)
	}
}

func TestModelSourcePrefersTheNodesOwnDisk(t *testing.T) {
	f := newSourceFixture(t)
	f.file(t, f.team, nil, "Qwen/Qwen3-8B", proto.MFPresent)
	f.mount(t, f.n1, f.team, proto.StorageStateReady)
	f.file(t, f.local, f.n1, "/opt/gpuworks/models/q", proto.MFPresent)
	// Both readable: the node's disk wins even over the storage it saves into.
	if src := f.source(t, f.n1, SourceRules{SaveTo: f.team.ID}); src.Action != SourceReady || src.Location.Storage.ID != f.local.ID {
		t.Fatalf("both readable = %+v", src)
	}
	// Limited to lab-nfs, the local copy is not used.
	if src := f.source(t, f.n1, SourceRules{Pinned: []uint{f.team.ID}}); src.Action != SourceReady || src.Location.Storage.ID != f.team.ID {
		t.Fatalf("pinned = %+v", src)
	}
	// A cached copy counts as the node's own disk too.
	f.file(t, f.cache, nil, "Qwen/Qwen3-8B", proto.MFPresent)
	f.mount(t, f.n1, f.cache, proto.StorageStateReady)
	f.file(t, f.cache, f.n1, "/opt/gpuworks/cache/archive-cache/q", proto.MFPresent)
	if src := f.source(t, f.n1, SourceRules{SaveTo: f.cache.ID, Pinned: []uint{f.team.ID, f.cache.ID}}); src.Action != SourceReady || src.Location.Storage.ID != f.cache.ID {
		t.Fatalf("cached vs NFS = %+v", src)
	}
}

func TestModelSourceFillsACacheBeforeDownloading(t *testing.T) {
	f := newSourceFixture(t)
	f.file(t, f.cache, nil, "Qwen/Qwen3-8B", proto.MFPresent)
	f.mount(t, f.n1, f.cache, proto.StorageStateReady)
	if src := f.source(t, f.n1, SourceRules{}); src.Action != SourceFillCache || src.Location.Storage.ID != f.cache.ID {
		t.Fatalf("n1 = %+v, want the cache filled", src)
	}
	// Where the cache storage is not mounted, the node acquires into the
	// default storage instead.
	if src := f.source(t, f.n2, SourceRules{}); src.Action != SourceAcquire || src.Location.Storage.ID != f.local.ID {
		t.Fatalf("n2 = %+v", src)
	}
}

func TestModelSourceSaveOrder(t *testing.T) {
	f := newSourceFixture(t)
	second, err := f.st.CreateStorage(&Storage{Name: "nvme", Type: proto.StorageLocal, Enabled: true, AllNodes: true, Path: "/nvme/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing anywhere: SaveTo when it applies, else the default.
	if src := f.source(t, f.n1, SourceRules{SaveTo: second.ID}); src.Action != SourceAcquire || src.Location.Storage.ID != second.ID {
		t.Fatalf("save to nvme = %+v", src)
	}
	if src := f.source(t, f.n1, SourceRules{}); src.Action != SourceAcquire || src.Location.Storage.ID != f.local.ID {
		t.Fatalf("no choice = %+v", src)
	}
	// A disabled default gives way to the next enabled local storage.
	disabled := false
	if _, err := f.st.UpdateStorage(f.local.ID, StoragePatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if src := f.source(t, f.n1, SourceRules{SaveTo: f.local.ID}); src.Action != SourceAcquire || src.Location.Storage.ID != second.ID {
		t.Fatalf("disabled save-to = %+v", src)
	}
	if _, err := f.st.UpdateStorage(second.ID, StoragePatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if src := f.source(t, f.n1, SourceRules{}); src.Action != SourceRefused || src.Reason != LocationStorageDisabled {
		t.Fatalf("every local storage disabled = %+v", src)
	}
	// A download already under way in a disabled storage is still followed.
	f.file(t, f.local, f.n1, "/opt/gpuworks/models/q", proto.MFDownloading)
	if src := f.source(t, f.n1, SourceRules{}); src.Action != SourceAcquire || src.Location.Storage.ID != f.local.ID {
		t.Fatalf("download under way = %+v", src)
	}
}

// A shared storage chosen to save into is never swapped for a node copy: a
// missing or failed write there is reported, not downloaded around.
func TestModelSourceNeverDownloadsAroundTheChosenSharedStorage(t *testing.T) {
	f := newSourceFixture(t)
	f.mount(t, f.n1, f.team, proto.StorageStateReady)
	rules := SourceRules{SaveTo: f.team.ID}
	if src := f.source(t, f.n1, rules); src.Action != SourceRefused || src.Reason != LocationModelMissingInStore || src.Location.Storage.ID != f.team.ID {
		t.Fatalf("missing = %+v", src)
	}
	row := f.file(t, f.team, nil, "Qwen/Qwen3-8B", proto.MFDownloading)
	if src := f.source(t, f.n1, rules); src.Action != SourceWaitShared {
		t.Fatalf("download under way = %+v", src)
	}
	row.State, row.Message = proto.MFFailed, "403 from the hub"
	if _, err := f.st.UpsertModelFile(row); err != nil {
		t.Fatal(err)
	}
	if src := f.source(t, f.n1, rules); src.Action != SourceRefused || src.Location.Shared == nil || src.Location.Shared.Message != "403 from the hub" {
		t.Fatalf("failed = %+v", src)
	}
	// In the storage but not mounted on the node: refused with that reason,
	// no local copy.
	row.State, row.Message = proto.MFPresent, ""
	if _, err := f.st.UpsertModelFile(row); err != nil {
		t.Fatal(err)
	}
	f.mount(t, f.n1, f.team, proto.StorageStateFailed)
	if src := f.source(t, f.n1, rules); src.Action != SourceRefused || src.Reason != LocationStorageUnavailable {
		t.Fatalf("unmounted = %+v", src)
	}
	// A node the storage does not apply to saves by the default order.
	if src := f.source(t, f.n2, rules); src.Action != SourceAcquire || src.Location.Storage.ID != f.local.ID {
		t.Fatalf("n2 = %+v", src)
	}
}

func TestStoragePinsReadTheRequirement(t *testing.T) {
	got := StoragePins(map[string][]string{"gpuworks.ai/storage": {"7", "x", "2", "0"}, "pool": {"3"}})
	if len(got) != 2 || got[0] != 2 || got[1] != 7 {
		t.Fatalf("pins = %v", got)
	}
	if StoragePins(nil) != nil {
		t.Fatal("no requirement pinned something")
	}
}

// A cached copy is readable even when the authoritative one is gone: it is
// then the last copy, not a reason to fetch the model again.
func TestModelSourceReadsACacheWhoseAuthoritativeCopyIsMissing(t *testing.T) {
	f := newSourceFixture(t)
	f.mount(t, f.n1, f.cache, proto.StorageStateReady)
	f.file(t, f.cache, nil, "Qwen/Qwen3-8B", proto.MFMissing)
	f.file(t, f.cache, f.n1, "/opt/gpuworks/cache/archive-cache/q", proto.MFPresent)
	if src := f.source(t, f.n1, SourceRules{}); src.Action != SourceReady || src.Location.HostPath != "/opt/gpuworks/cache/archive-cache/q" {
		t.Fatalf("cache with a missing authoritative copy = %+v", src)
	}
	// Without the cache the missing copy is the reason, not "not cached".
	loc, err := f.st.ModelLocation(f.model.ID, f.cache.ID, f.n2.ID)
	if err != nil || loc.Ready || loc.Reason != LocationModelMissingInStore {
		t.Fatalf("n2 location = %+v err=%v", loc, err)
	}
}

// A read-only local storage is not chosen to acquire into because an old
// failed attempt left a row there.
func TestModelSourceSkipsAFailedRowInADisabledStorage(t *testing.T) {
	f := newSourceFixture(t)
	second, err := f.st.CreateStorage(&Storage{Name: "nvme", Type: proto.StorageLocal, Enabled: true, AllNodes: true, Path: "/nvme/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.file(t, f.local, f.n1, "/opt/gpuworks/models/q", proto.MFFailed)
	disabled := false
	if _, err := f.st.UpdateStorage(f.local.ID, StoragePatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if src := f.source(t, f.n1, SourceRules{}); src.Action != SourceAcquire || src.Location.Storage.ID != second.ID {
		t.Fatalf("failed row in a read-only storage = %+v, want the next enabled storage", src)
	}
}

// Locating a model on a node costs a fixed number of queries however many
// storages there are: every launch, admission check and preview asks it.
func TestModelSourceQueryCountDoesNotGrowWithTheStorages(t *testing.T) {
	count := func(extra int) (n int64) {
		t.Run(fmt.Sprintf("extra%d", extra), func(t *testing.T) { n = countSourceQueries(t, extra) })
		return n
	}
	few, many := count(0), count(10)
	if few == 0 || few != many {
		t.Fatalf("ModelSourceOn: %d queries with 4 storages, %d with 14", few, many)
	}
	if few > 5 {
		t.Fatalf("ModelSourceOn issues %d queries; it reads the storages, targets, node states and model rows once each", few)
	}
}

func countSourceQueries(t *testing.T, extra int) int64 {
	t.Helper()
	f := newSourceFixture(t)
	for i := 0; i < extra; i++ {
		if _, err := f.st.CreateStorage(&Storage{Name: fmt.Sprintf("extra-%d", i), Type: proto.StorageNFS, Enabled: true, AllNodes: true,
			Path: fmt.Sprintf("/mnt/gpuworks/extra-%d", i), Export: fmt.Sprintf("nfs.example.internal:/export/extra-%d", i)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.file(t, f.team, nil, "Qwen/Qwen3-8B", proto.MFPresent)
	f.mount(t, f.n1, f.team, proto.StorageStateReady)
	var queries atomic.Int64
	tally := func(*gorm.DB) { queries.Add(1) }
	if err := f.st.DB().Callback().Query().Before("gorm:query").Register("test:count_query", tally); err != nil {
		t.Fatal(err)
	}
	if src := f.source(t, f.n1, SourceRules{SaveTo: f.team.ID}); src.Action != SourceReady {
		t.Fatalf("source = %+v", src)
	}
	return queries.Load()
}

func TestNodeNeedsOwnDirectory(t *testing.T) {
	for _, c := range []struct {
		name  string
		typ   string
		state *StorageNodeState
		want  bool
	}{
		{"no report", proto.StorageLocal, nil, false},
		{"ready and writable", proto.StorageLocal, &StorageNodeState{State: proto.StorageStateReady, Writable: true}, false},
		{"ready read-only local", proto.StorageLocal, &StorageNodeState{State: proto.StorageStateReady}, true},
		{"ready read-only cache", proto.StorageNFSCache, &StorageNodeState{State: proto.StorageStateReady}, false},
		{"permission denied", proto.StorageLocal, &StorageNodeState{State: proto.StorageStateFailed, Reason: proto.StorageReasonPermissionDenied}, true},
		{"no directory set", proto.StorageNFSCache, &StorageNodeState{State: proto.StorageStateFailed, Reason: proto.StorageReasonPathMissing}, true},
		{"unreachable nfs", proto.StorageNFS, &StorageNodeState{State: proto.StorageStateFailed, Reason: proto.StorageReasonPathMissing}, false},
		{"other failure", proto.StorageLocal, &StorageNodeState{State: proto.StorageStateFailed, Reason: "other"}, false},
	} {
		if got := NodeNeedsOwnDirectory(c.typ, c.state); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A create that fetches a template component into the shared storage its
// primary sits on starts one write there, owned by the deployment; with no
// storage chosen, that deployment's nodes that read the storage wait for it
// instead of each downloading into local storage. Another deployment, a
// preview, a node the storage does not serve, a finished or failed write, or
// a chosen storage leave the usual order in charge.
func TestModelSourceWaitsOnlyForASharedWriteItsDeploymentStarted(t *testing.T) {
	f := newSourceFixture(t)
	f.mount(t, f.n1, f.team, proto.StorageStateReady)
	own, other := SourceRules{Deployment: 7}, SourceRules{Deployment: 8}
	if src := f.source(t, f.n1, own); src.Action != SourceAcquire || src.Location.Storage.ID != f.local.ID {
		t.Fatalf("nothing under way = %+v, want the default local storage", src)
	}
	row := f.file(t, f.team, nil, "MiniMax/MiniMax-H3-text_encoder", proto.MFDownloading)
	row.DownloadDeploymentID = 7
	if _, err := f.st.UpsertModelFile(row); err != nil {
		t.Fatal(err)
	}
	if src := f.source(t, f.n1, own); src.Action != SourceWaitShared || src.Location.Storage.ID != f.team.ID {
		t.Fatalf("own write under way = %+v, want to wait for lab-nfs", src)
	}
	// A write another deployment started, or a preview with no deployment,
	// does not hold the node up: a stuck writer stalls only its own service.
	for name, rules := range map[string]SourceRules{"another deployment": other, "preview": {}} {
		if src := f.source(t, f.n1, rules); src.Action != SourceAcquire || src.Location.Storage.ID != f.local.ID {
			t.Fatalf("%s = %+v, want the default local storage", name, src)
		}
	}
	if src := f.source(t, f.n2, own); src.Action != SourceAcquire || src.Location.Storage.ID != f.local.ID {
		t.Fatalf("n2 outside lab-nfs = %+v, want its local storage", src)
	}
	if src := f.source(t, f.n1, SourceRules{SaveTo: f.local.ID, Deployment: 7}); src.Action != SourceAcquire || src.Location.Storage.ID != f.local.ID {
		t.Fatalf("local chosen = %+v, want the chosen storage", src)
	}
	row.State, row.Message = proto.MFFailed, "403 from the hub"
	if _, err := f.st.UpsertModelFile(row); err != nil {
		t.Fatal(err)
	}
	if src := f.source(t, f.n1, own); src.Action != SourceAcquire || src.Location.Storage.ID != f.local.ID {
		t.Fatalf("failed write = %+v, want the default local storage", src)
	}
	row.State, row.Message = proto.MFPresent, ""
	if _, err := f.st.UpsertModelFile(row); err != nil {
		t.Fatal(err)
	}
	if src := f.source(t, f.n1, other); src.Action != SourceReady || src.Location.Storage.ID != f.team.ID {
		t.Fatalf("finished write = %+v, want any deployment to read it from lab-nfs", src)
	}
}
