package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// storageHub records pushes and the RPCs the controller makes on nodes.
type storageHub struct {
	distributedTestHub
	mu    sync.Mutex
	calls []string
}

func (h *storageHub) CallNode(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, _ := json.Marshal(params)
	h.calls = append(h.calls, uuid+" "+method+" "+string(b))
	if method == proto.RPCCacheEvict {
		return json.Marshal(map[string]any{"removed": true, "path": params.(map[string]string)["path"], "eviction_id": params.(map[string]string)["eviction_id"]})
	}
	return json.RawMessage(`{}`), nil
}

type storageControllerFixture struct {
	st     *store.Store
	hub    *storageHub
	c      *Controller
	node   *store.Node
	other  *store.Node
	model  *store.Model
	nfs    *store.Storage
	cached *store.Storage
	// deployments numbers the fixture's deployments' served names.
	deployments int
}

func newStorageControllerFixture(t *testing.T) *storageControllerFixture {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &storageControllerFixture{st: st}
	if f.node, err = st.RegisterNode("n1", store.HashToken("t1"), "n1"); err != nil {
		t.Fatal(err)
	}
	if f.other, err = st.RegisterNode("n2", store.HashToken("t2"), "n2"); err != nil {
		t.Fatal(err)
	}
	if f.model, err = st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-8B", Name: "Qwen3-8B", WeightBytes: 15 << 30}); err != nil {
		t.Fatal(err)
	}
	if f.nfs, err = st.CreateStorage(&store.Storage{Name: "model-archive", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: "/mnt/gpuworks/model-archive", Export: "h:/a"}, nil); err != nil {
		t.Fatal(err)
	}
	if f.cached, err = st.CreateStorage(&store.Storage{Name: "archive-cache", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true, Path: "/mnt/gpuworks/archive-cache", Export: "h:/b", CacheMaxBytes: 20 << 30, CachePath: "/var/cache/gw"}, nil); err != nil {
		t.Fatal(err)
	}
	f.hub = &storageHub{distributedTestHub: distributedTestHub{online: map[string]bool{f.node.UUID: true, f.other.UUID: true}, updates: map[string][]proto.StateUpdate{}}}
	f.c = New(st, f.hub, nil)
	return f
}

func (f *storageControllerFixture) ready(t *testing.T, node *store.Node, statuses ...proto.StorageStatus) {
	t.Helper()
	if err := f.st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: statuses}); err != nil {
		t.Fatal(err)
	}
}

func (f *storageControllerFixture) deployment(t *testing.T, storage *store.Storage) *store.Deployment {
	t.Helper()
	f.deployments++
	dep, err := createTestDeployment(t, f.st, &store.Deployment{ModelID: f.model.ID, NodeID: f.node.ID, ServedName: fmt.Sprintf("q-%d", f.deployments), Engine: "vllm", State: store.DeployActive, SaveToStorageID: storage.ID})
	if err != nil {
		t.Fatal(err)
	}
	return dep
}

func TestNFSStorageLaunchesFromTheMountWithoutANodeRow(t *testing.T) {
	f := newStorageControllerFixture(t)
	dep := f.deployment(t, f.nfs)
	// Not mounted on the node yet: nothing to start, nothing pushed.
	if _, ready, err := f.c.ensureModelReady(context.Background(), f.node, dep, f.model, store.SourceRules{SaveTo: dep.SaveToStorageID}); err != nil || ready {
		t.Fatalf("ready=%v err=%v before the storage is ready", ready, err)
	}
	f.ready(t, f.node, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true})
	// Mounted, but the storage does not hold the model: still nothing to start.
	if _, ready, _ := f.c.ensureModelReady(context.Background(), f.node, dep, f.model, store.SourceRules{SaveTo: dep.SaveToStorageID}); ready {
		t.Fatal("ready without the model in the storage")
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent, SizeBytes: 15 << 30}); err != nil {
		t.Fatal(err)
	}
	loc, ready, err := f.c.ensureModelReady(context.Background(), f.node, dep, f.model, store.SourceRules{SaveTo: dep.SaveToStorageID})
	if err != nil || !ready || loc.HostPath != "/mnt/gpuworks/model-archive/Qwen/Qwen3-8B" {
		t.Fatalf("location = %+v ready=%v err=%v", loc, ready, err)
	}
	if rows, _ := f.st.ModelFilesForNode(f.node.ID); len(rows) != 0 {
		t.Fatalf("an NFS launch created node rows: %+v", rows)
	}
	if len(f.hub.updates[f.node.UUID]) != 0 {
		t.Fatalf("an NFS launch pushed model tasks: %+v", f.hub.updates)
	}
}

func TestNFSCacheEvictsLeastRecentlyUsedBeforeFilling(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			f := newStorageControllerFixture(t)
			var err error
			f.cached, err = f.st.UpdateStorage(f.cached.ID, store.StoragePatch{Enabled: &enabled})
			if err != nil {
				t.Fatal(err)
			}
			small, err := f.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Name: "small", WeightBytes: 10 << 30})
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range []*store.Model{f.model, small} {
				if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: m.ID, StorageID: f.cached.ID, Path: m.SourceID, State: proto.MFPresent, SizeBytes: m.WeightBytes}); err != nil {
					t.Fatal(err)
				}
			}
			f.ready(t, f.node, proto.StorageStatus{ID: f.cached.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/archive-cache", Writable: enabled, CachePath: "/var/cache/gw"})
			old := time.Now().Add(-48 * time.Hour)
			evictable, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: small.ID, StorageID: f.cached.ID, NodeID: f.node.ID, Path: "/var/cache/gw/small", State: proto.MFPresent, SizeBytes: 10 << 30, LastUsedAt: &old})
			if err != nil {
				t.Fatal(err)
			}
			dep := f.deployment(t, f.cached)
			// A rank may discover a newly absent model while its generation owns a
			// launch pin. Admission must defer instead of waiting on its own pin;
			// generationReady can make room on the next pass before launch pins it.
			release, pinned := f.st.TryPinNodeCaches([]uint{f.node.ID})
			if !pinned {
				t.Fatal("pin launch")
			}
			defer release()
			if _, ready, err := f.c.ensureModelReady(context.Background(), f.node, dep, f.model, store.SourceRules{SaveTo: dep.SaveToStorageID}); err != nil || ready {
				t.Fatalf("busy admission: ready=%v err=%v", ready, err)
			}
			if len(f.hub.calls) != 0 {
				t.Fatal("busy admission called eviction RPC")
			}
			f.c.q.mu.Lock()
			_, retry := f.c.q.pending[depKey(dep.ID)]
			f.c.q.mu.Unlock()
			if !retry {
				t.Fatal("busy admission was not requeued")
			}
			release()
			// 10 GiB cached + 15 GiB to add exceeds the 20 GiB cap: the unused small
			// model goes first, then the copy is pushed.
			if _, ready, err := f.c.ensureModelReady(context.Background(), f.node, dep, f.model, store.SourceRules{SaveTo: dep.SaveToStorageID}); err != nil || ready {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
			if _, err := f.st.ModelFileByID(evictable.ID); err == nil {
				t.Fatal("the least recently used cache entry was not evicted")
			}
			if len(f.hub.calls) != 1 || !strings.HasPrefix(f.hub.calls[0], f.node.UUID+" "+proto.RPCCacheEvict+` {"eviction_id":"`) || !strings.HasSuffix(f.hub.calls[0], `","path":"/var/cache/gw/small"}`) {
				t.Fatalf("node calls = %v", f.hub.calls)
			}
			row, err := f.st.ModelFileAt(store.FileRef{ModelID: f.model.ID, StorageID: f.cached.ID, NodeID: f.node.ID})
			if err != nil || row.Acquisition != proto.ModelAcquireCopy || row.State != proto.MFPending {
				t.Fatalf("cache row = %+v err=%v", row, err)
			}
			pushed := f.hub.updates[f.node.UUID]
			if len(pushed) != 1 || len(pushed[0].ModelFiles) != 1 || !pushed[0].ModelFiles[0].Cache ||
				pushed[0].ModelFiles[0].SourcePath != "/mnt/gpuworks/archive-cache/Qwen/Qwen3-8B" {
				t.Fatalf("pushed = %+v", pushed)
			}
		})
	}
}
func TestNFSCacheRefusesAModelLargerThanTheCap(t *testing.T) {
	f := newStorageControllerFixture(t)
	huge, err := f.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-235B-A22B", Name: "huge", WeightBytes: 438 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: huge.ID, StorageID: f.cached.ID, Path: huge.SourceID, State: proto.MFPresent, SizeBytes: 438 << 30}); err != nil {
		t.Fatal(err)
	}
	f.ready(t, f.node, proto.StorageStatus{ID: f.cached.ID, State: proto.StorageStateReady, Path: "/mnt/c", CachePath: "/var/cache/gw"})
	dep, err := createTestDeployment(t, f.st, &store.Deployment{ModelID: huge.ID, NodeID: f.node.ID, ServedName: "h", Engine: "vllm", State: store.DeployActive, SaveToStorageID: f.cached.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ready, err := f.c.ensureModelReady(context.Background(), f.node, dep, huge, store.SourceRules{SaveTo: dep.SaveToStorageID}); err != nil || ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	if _, err := f.st.ModelFileAt(store.FileRef{ModelID: huge.ID, StorageID: f.cached.ID, NodeID: f.node.ID}); err == nil {
		t.Fatal("a cache copy was started for a model over the cap")
	}
	events, err := f.st.DeploymentEvents(dep.ID, 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("no unschedulable reason recorded: %v", err)
	}
}

func TestDisabledLocalStorageTakesNoNewDownload(t *testing.T) {
	f := newStorageControllerFixture(t)
	local, err := f.st.StorageByID(f.st.DefaultLocalStorageID())
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	if _, err := f.st.UpdateStorage(local.ID, store.StoragePatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	dep := f.deployment(t, local)
	if _, ready, err := f.c.ensureModelReady(context.Background(), f.node, dep, f.model, store.SourceRules{SaveTo: dep.SaveToStorageID}); err != nil || ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	if rows, _ := f.st.ModelFilesForNode(f.node.ID); len(rows) != 0 {
		t.Fatalf("a read-only storage got a download row: %+v", rows)
	}
}

func TestCopyModelFilePicksTheTransfer(t *testing.T) {
	f := newStorageControllerFixture(t)
	local := f.st.DefaultLocalStorageID()
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent, SizeBytes: 15}); err != nil {
		t.Fatal(err)
	}
	f.ready(t, f.node, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/a", Writable: true})
	// NFS to the local storage of a node that mounts it: a copy on that node.
	row, err := f.c.Transfers().Copy(context.Background(), f.model.ID, store.FileRef{StorageID: f.nfs.ID}, store.FileRef{StorageID: local, NodeID: f.node.ID}, -1)
	if err != nil || row.Acquisition != proto.ModelAcquireCopy || row.TransferSourceStorageID != f.nfs.ID {
		t.Fatalf("copy on the node = %+v err=%v", row, err)
	}
	// NFS to a node that does not mount it: a peer transfer from one that does.
	row, err = f.c.Transfers().Copy(context.Background(), f.model.ID, store.FileRef{StorageID: f.nfs.ID}, store.FileRef{StorageID: local, NodeID: f.other.ID}, 0)
	if err != nil || row.TransferSourceNodeID != f.node.ID {
		t.Fatalf("peer copy = %+v err=%v", row, err)
	}
	// Into a read-only storage: refused.
	disabled := false
	if _, err := f.st.UpdateStorage(local, store.StoragePatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Transfers().Copy(context.Background(), f.model.ID, store.FileRef{StorageID: f.nfs.ID}, store.FileRef{StorageID: local, NodeID: f.other.ID}, 0); err == nil {
		t.Fatal("copy into a disabled storage accepted")
	}
	// Local storage on n1 into the NFS: the node that holds it and mounts the
	// NFS writes it.
	enabled := true
	if _, err := f.st.UpdateStorage(local, store.StoragePatch{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	other, err := f.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "org/other", Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: other.ID, StorageID: local, NodeID: f.node.ID, Path: "/data/other", State: proto.MFPresent}); err != nil {
		t.Fatal(err)
	}
	row, err = f.c.Transfers().Copy(context.Background(), other.ID, store.FileRef{StorageID: local, NodeID: f.node.ID}, store.FileRef{StorageID: f.nfs.ID}, -1)
	if err != nil || row.NodeID != 0 || row.WriterNodeID != f.node.ID || row.Path != "org/other" || row.Acquisition != proto.ModelAcquireCopy {
		t.Fatalf("copy into the NFS = %+v err=%v", row, err)
	}
	// A source that does not hold the model is refused.
	if _, err := f.c.Transfers().Copy(context.Background(), other.ID, store.FileRef{StorageID: local, NodeID: f.other.ID}, store.FileRef{StorageID: f.nfs.ID}, -1); err == nil {
		t.Fatal("copy from a node without the model accepted")
	}
}

// A multi-node deployment limited to a storage is planned with that limit:
// its launch request carries the same source rules its model is fetched by.
func TestMultiNodeLaunchCarriesTheStorageLimit(t *testing.T) {
	f := newStorageControllerFixture(t)
	pin := fmt.Sprint(f.nfs.ID)
	dep, err := createTestDeploymentWithRequirements(t, f.st, &store.Deployment{ModelID: f.model.ID, NodeID: f.node.ID, NNodes: 2, ServedName: "q", Engine: "vllm", State: store.DeployActive, SaveToStorageID: f.nfs.ID},
		map[string][]string{"gpuworks.ai/storage": {pin}})
	if err != nil {
		t.Fatal(err)
	}
	req, _, _, err := f.c.launchRequest(dep, f.node, f.model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.ModelRules.Pinned) != 1 || req.ModelRules.Pinned[0] != f.nfs.ID || req.ModelRules.SaveTo != f.nfs.ID {
		t.Fatalf("launch model rules = %+v", req.ModelRules)
	}
}

// A node the deployment's storage refuses is refused at the pool's trial,
// with the same reason the controller records, so the pool does not pick it
// and then wait there.
func TestTrialRefusesANodeTheStorageCannotServe(t *testing.T) {
	f := newStorageControllerFixture(t)
	f.ready(t, f.node, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true})
	dep := f.deployment(t, f.nfs) // saves into the NFS, which lacks the model
	refusal, err := f.c.modelUnavailable(dep, f.node, f.model)
	if err != nil || refusal == nil || refusal.Code != scheduler.ReasonModelMissingInStorage || refusal.Params["storage"] != "model-archive" {
		t.Fatalf("trial refusal = %+v err=%v", refusal, err)
	}
	if _, ready, err := f.c.ensureModelReady(context.Background(), f.node, dep, f.model, store.SourceRules{SaveTo: dep.SaveToStorageID}); err != nil || ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	events, err := f.st.DeploymentEvents(dep.ID, 10)
	if err != nil || len(events) == 0 || !strings.Contains(events[len(events)-1].Data, scheduler.ReasonModelMissingInStorage) {
		t.Fatalf("recorded reason = %+v err=%v", events, err)
	}
}

// A deployment limited to the local storage fetches its model without
// copying from a shared storage it was kept off - even one mounted on the
// node that holds the model.
func TestAcquisitionDoesNotCopyFromAStorageTheDeploymentExcludes(t *testing.T) {
	f := newStorageControllerFixture(t)
	f.ready(t, f.node, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true})
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent, SizeBytes: 15 << 30}); err != nil {
		t.Fatal(err)
	}
	local := f.st.DefaultLocalStorageID()
	dep := f.deployment(t, f.nfs)
	rules := store.SourceRules{Pinned: []uint{local}}
	if _, ready, err := f.c.ensureModelReady(context.Background(), f.node, dep, f.model, rules); err != nil || ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	row, err := f.st.ModelFileAt(store.FileRef{ModelID: f.model.ID, StorageID: local, NodeID: f.node.ID})
	if err != nil {
		t.Fatal(err)
	}
	if row.Acquisition == proto.ModelAcquireCopy || row.TransferSourceStorageID == f.nfs.ID {
		t.Fatalf("copied from the excluded storage: %+v", row)
	}
	// Without the limit the mounted copy is the cheapest source.
	other := f.deployment(t, f.nfs)
	if err := f.st.DeleteModelFileRow(row.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.c.acquireLocalModel(context.Background(), f.node, other, mustStorage(t, f.st, local), f.model, nil, nil); err != nil {
		t.Fatal(err)
	}
	row, err = f.st.ModelFileAt(store.FileRef{ModelID: f.model.ID, StorageID: local, NodeID: f.node.ID})
	if err != nil || row.TransferSourceStorageID != f.nfs.ID {
		t.Fatalf("unlimited acquisition = %+v err=%v, want a copy from the mounted NFS", row, err)
	}
}

func mustStorage(t *testing.T, st *store.Store, id uint) *store.Storage {
	t.Helper()
	storage, err := st.StorageByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return storage
}
