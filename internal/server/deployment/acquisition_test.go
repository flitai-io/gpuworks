package deployment

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/acquisition"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func newAcquisitionStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("file:api_acquisition_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// saveToRig is a store with one model, a node and an NFS storage whose ready
// state on the node the test sets.
type saveToRig struct {
	st    *store.Store
	h     *Service
	node  *store.Node
	model *store.Model
	nfs   *store.Storage
}

func newSaveToRig(t *testing.T) *saveToRig {
	t.Helper()
	st := newAcquisitionStore(t)
	node, err := st.RegisterNode("gpu-a", store.HashToken("a"), "gpu-a")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main"})
	if err != nil {
		t.Fatal(err)
	}
	nfs, err := st.CreateStorage(&store.Storage{Name: "team-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/team-nfs", Export: "nfs.example.internal:/export/team"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &saveToRig{st: st, node: node, model: model, nfs: nfs,
		h: New(Deps{Store: st, Online: func(string) bool { return true }})}
}

func (r *saveToRig) nfsState(t *testing.T, state string, writable bool) {
	t.Helper()
	if err := r.st.ApplyStorageStatus(r.node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: r.nfs.ID, State: state, Path: "/mnt/gpuworks/team-nfs", Writable: writable},
	}}); err != nil {
		t.Fatal(err)
	}
}

func (r *saveToRig) nfsHoldsModel(t *testing.T) {
	t.Helper()
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: r.model.ID, StorageID: r.nfs.ID, Path: "org/model", State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
}

// A create names at most a storage. Naming none leaves every node to the
// default order of store.ModelSourceOn; naming one is validated against every
// target, never substituted (ADR 0018).
func TestResolveSaveToStorage(t *testing.T) {
	t.Run("no choice returns no storage", func(t *testing.T) {
		r := newSaveToRig(t)
		for name, source := range map[string]*dto.ModelSource{
			"nil source":        nil,
			"empty source":      {},
			"provider only":     {Provider: "huggingface"},
			"explicit zero ids": {StorageID: 0, Provider: "modelscope"},
		} {
			got, err := r.h.resolveSaveToStorage(source, r.model, []*store.Node{r.node})
			if err != nil || got != nil {
				t.Fatalf("%s: storage=%v err=%v, want nil/nil", name, got, err)
			}
		}
	})
	t.Run("a storage that no longer exists is stale", func(t *testing.T) {
		r := newSaveToRig(t)
		_, err := r.h.resolveSaveToStorage(&dto.ModelSource{StorageID: 9999}, r.model, []*store.Node{r.node})
		if !containsProductCode(err, messages.ErrorAcquisitionCandidateStale) {
			t.Fatalf("err=%v, want %s", err, messages.ErrorAcquisitionCandidateStale)
		}
	})
	t.Run("the local storage always accepts a fetch", func(t *testing.T) {
		r := newSaveToRig(t)
		local := r.st.DefaultLocalStorageID()
		if err := r.st.ApplyStorageStatus(r.node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
			{ID: local, State: proto.StorageStateReady, Path: "/data/models", Writable: true},
		}}); err != nil {
			t.Fatal(err)
		}
		got, err := r.h.resolveSaveToStorage(&dto.ModelSource{StorageID: local}, r.model, []*store.Node{r.node})
		if err != nil || got == nil || got.ID != local {
			t.Fatalf("storage=%v err=%v", got, err)
		}
	})
	t.Run("a shared storage with a writer accepts a fetch", func(t *testing.T) {
		r := newSaveToRig(t)
		r.nfsState(t, proto.StorageStateReady, true)
		got, err := r.h.resolveSaveToStorage(&dto.ModelSource{StorageID: r.nfs.ID}, r.model, []*store.Node{r.node, r.node, nil})
		if err != nil || got == nil || got.ID != r.nfs.ID {
			t.Fatalf("storage=%v err=%v", got, err)
		}
	})
	t.Run("a shared storage nobody can write refuses a fetch", func(t *testing.T) {
		r := newSaveToRig(t)
		r.nfsState(t, proto.StorageStateReady, false)
		_, err := r.h.resolveSaveToStorage(&dto.ModelSource{StorageID: r.nfs.ID}, r.model, []*store.Node{r.node})
		if !containsProductCode(err, messages.ErrorStorageNotWritable) {
			t.Fatalf("err=%v, want %s", err, messages.ErrorStorageNotWritable)
		}
	})
	t.Run("a read-only storage that already holds the model is accepted", func(t *testing.T) {
		r := newSaveToRig(t)
		r.nfsState(t, proto.StorageStateReady, false)
		r.nfsHoldsModel(t)
		got, err := r.h.resolveSaveToStorage(&dto.ModelSource{StorageID: r.nfs.ID}, r.model, []*store.Node{r.node})
		if err != nil || got == nil || got.ID != r.nfs.ID {
			t.Fatalf("storage=%v err=%v", got, err)
		}
	})
	t.Run("a disabled storage refuses a fetch", func(t *testing.T) {
		r := newSaveToRig(t)
		r.nfsState(t, proto.StorageStateReady, true)
		if err := r.st.DB().Model(&store.Storage{}).Where("id = ?", r.nfs.ID).Update("enabled", false).Error; err != nil {
			t.Fatal(err)
		}
		_, err := r.h.resolveSaveToStorage(&dto.ModelSource{StorageID: r.nfs.ID}, r.model, []*store.Node{r.node})
		if err == nil {
			t.Fatal("disabled storage accepted a fetch")
		}
		if !containsProductCode(err, messages.ErrorStorageDisabled) && !containsProductCode(err, messages.ErrorStorageUnavailableOnNode) {
			t.Fatalf("err=%v, want a storage refusal", err)
		}
	})
	t.Run("a storage that does not apply to a target node is a preference, not a refusal", func(t *testing.T) {
		r := newSaveToRig(t)
		other, err := r.st.RegisterNode("gpu-b", store.HashToken("b"), "gpu-b")
		if err != nil {
			t.Fatal(err)
		}
		scoped, err := r.st.CreateStorage(&store.Storage{Name: "b-only", Type: proto.StorageNFS, Enabled: true,
			Path: "/mnt/gpuworks/b-only", Export: "nfs.example.internal:/export/b"}, []store.StorageNode{{NodeID: other.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.st.ApplyStorageStatus(other.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
			{ID: scoped.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/b-only", Writable: true},
		}}); err != nil {
			t.Fatal(err)
		}
		// The storage is a preference for the nodes in its scope: gpu-b saves
		// there, gpu-a (outside the scope) falls back to its local storage, as
		// a launch does (ADR 0034). Neither refuses the create.
		if got, err := r.h.resolveSaveToStorage(&dto.ModelSource{StorageID: scoped.ID}, r.model, []*store.Node{other}); err != nil || got == nil {
			t.Fatalf("in-scope target: storage=%v err=%v", got, err)
		}
		if got, err := r.h.resolveSaveToStorage(&dto.ModelSource{StorageID: scoped.ID}, r.model, []*store.Node{other, r.node}); err != nil || got == nil {
			t.Fatalf("a target outside the scope: storage=%v err=%v, want it to fall back", got, err)
		}
	})
	t.Run("no targets or no model only checks the storage exists", func(t *testing.T) {
		r := newSaveToRig(t)
		if got, err := r.h.resolveSaveToStorage(&dto.ModelSource{StorageID: r.nfs.ID}, r.model, nil); err != nil || got == nil {
			t.Fatalf("no targets: storage=%v err=%v", got, err)
		}
		if got, err := r.h.resolveSaveToStorage(&dto.ModelSource{StorageID: r.nfs.ID}, nil, []*store.Node{r.node}); err != nil || got == nil {
			t.Fatalf("no model: storage=%v err=%v", got, err)
		}
	})
}

// A create that saves into a shared storage which does not hold the model
// starts one download through a writer node; every other case needs nothing
// at create time (a local storage is fetched per node by the controller).
func TestStartSharedModelDownload(t *testing.T) {
	t.Run("no storage or a local storage starts nothing", func(t *testing.T) {
		r := newSaveToRig(t)
		local, err := r.st.StorageByID(r.st.DefaultLocalStorageID())
		if err != nil {
			t.Fatal(err)
		}
		for name, storage := range map[string]*store.Storage{"nil": nil, "local": local} {
			if err := r.h.startSharedModelDownload(r.model, storage, []*store.Node{r.node}); err != nil {
				t.Fatalf("%s: err=%v", name, err)
			}
		}
		if err := r.h.startSharedModelDownload(nil, r.nfs, []*store.Node{r.node}); err != nil {
			t.Fatalf("nil model: err=%v", err)
		}
		if _, err := r.st.ModelFileAt(store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a download row exists though nothing should start: %v", err)
		}
	})
	t.Run("a shared storage without the model downloads once through a writer", func(t *testing.T) {
		r := newSaveToRig(t)
		r.nfsState(t, proto.StorageStateReady, true)
		var mu sync.Mutex
		var synced []string
		r.h.d.NodeSync = nodeSyncCallbacks{full: func(uuid string) error {
			mu.Lock()
			defer mu.Unlock()
			synced = append(synced, uuid)
			return nil
		}}
		for i := 0; i < 2; i++ { // the second call finds the transfer under way
			if err := r.h.startSharedModelDownload(r.model, r.nfs, []*store.Node{r.node}); err != nil {
				t.Fatalf("call %d: err=%v", i, err)
			}
		}
		row, err := r.st.ModelFileAt(store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID})
		if err != nil {
			t.Fatal(err)
		}
		if row.State != proto.MFPending || row.Acquisition != proto.ModelAcquireDownload || row.WriterNodeID != r.node.ID {
			t.Fatalf("shared download row = %+v", row)
		}
		if row.Path != store.SharedModelRelPath(r.model) {
			t.Fatalf("path=%q want %q", row.Path, store.SharedModelRelPath(r.model))
		}
		if fmt.Sprint(synced) != fmt.Sprint([]string{r.node.UUID}) {
			t.Fatalf("writer syncs=%v, want exactly one push to %s", synced, r.node.UUID)
		}
	})
	t.Run("a shared storage that holds the model starts nothing", func(t *testing.T) {
		r := newSaveToRig(t)
		r.nfsState(t, proto.StorageStateReady, true)
		r.nfsHoldsModel(t)
		r.h.d.NodeSync = nodeSyncCallbacks{full: func(string) error { t.Fatal("a node was pushed though the storage holds the model"); return nil }}
		if err := r.h.startSharedModelDownload(r.model, r.nfs, []*store.Node{r.node}); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("no online writer is a conflict", func(t *testing.T) {
		r := newSaveToRig(t)
		r.nfsState(t, proto.StorageStateReady, true)
		r.h.d.Online = func(string) bool { return false }
		err := r.h.startSharedModelDownload(r.model, r.nfs, []*store.Node{r.node})
		if KindOf(err) != KindConflict || !containsProductCode(err, messages.ErrorStorageNotWritable) {
			t.Fatalf("kind=%d err=%v", KindOf(err), err)
		}
		if _, err := r.st.ModelFileAt(store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a row was left behind by a refused download: %v", err)
		}
	})
}

// An evaluation plans each node with the storage a create would keep: the
// one the request names, else none (store.ModelSourceOn then decides per node
// where the model is read from and where a fetch saves). It never derives one
// from where the model happens to be.
func TestPreviewSaveToStorageIDMatchesTheCreate(t *testing.T) {
	st := newAcquisitionStore(t)
	node, err := st.RegisterNode("gpu3", store.HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-8B", Revision: "master"})
	if err != nil {
		t.Fatal(err)
	}
	nfs, err := st.CreateStorage(&store.Storage{Name: "team-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/team-nfs", Export: "nfs.example.internal:/export/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st, Online: func(string) bool { return true }})
	local := st.DefaultLocalStorageID()

	if got := h.previewSaveToStorageID(nil, model, node); got != 0 {
		t.Fatalf("no choice: storage %d, want 0", got)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: nfs.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent, SizeBytes: 16 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/team-nfs", Writable: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := h.previewSaveToStorageID(nil, model, node); got != 0 {
		t.Fatalf("model on the NFS storage, no choice: storage %d, want 0", got)
	}
	// With no choice the node still reads the NFS copy: the rule, not the
	// save-to storage, decides that.
	if src, err := st.ModelSourceOn(model.ID, node, store.SourceRules{}); err != nil || src.Action != store.SourceReady || src.Location.Storage.ID != nfs.ID {
		t.Fatalf("source with no choice = %+v err=%v", src, err)
	}
	if got := h.previewSaveToStorageID(&dto.ModelSource{StorageID: local}, model, node); got != local {
		t.Fatalf("explicit save-to storage: %d, want %d", got, local)
	}
	// An explicit choice is kept even where it cannot serve, so planning
	// reports the reason instead of evaluating some other storage.
	if got := h.previewSaveToStorageID(&dto.ModelSource{StorageID: 99}, model, node); got != 99 {
		t.Fatalf("explicit unknown storage: %d, want 99", got)
	}
}

// The deploy form's "save to" lists only storages a download or copy would
// add the model to; one that already holds it is listed as present instead.
func TestStorageFactsDoNotOfferSavingWhereTheModelIs(t *testing.T) {
	st := newAcquisitionStore(t)
	node, err := st.RegisterNode("gpu3", store.HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Revision: "master"})
	if err != nil {
		t.Fatal(err)
	}
	holding, err := st.CreateStorage(&store.Storage{Name: "team-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/team-nfs", Export: "nfs.example.internal:/export/team"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := st.CreateStorage(&store.Storage{Name: "scratch-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/scratch-nfs", Export: "nfs.example.internal:/export/scratch"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	local := st.DefaultLocalStorageID()
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: local, State: proto.StorageStateReady, Path: "/data/models", Writable: true},
		{ID: holding.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/team-nfs", Writable: true},
		{ID: empty.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/scratch-nfs", Writable: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: holding.ID, Path: "Qwen/Qwen3-0.6B", State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	ids := func(present []acquisition.StoragePresence, targets []acquisition.StorageTarget) (p, tg []uint) {
		for _, x := range present {
			p = append(p, x.StorageID)
		}
		for _, x := range targets {
			tg = append(tg, x.StorageID)
		}
		return
	}
	present, targets, err := h.storageFacts(model, node)
	if err != nil {
		t.Fatal(err)
	}
	p, tg := ids(present, targets)
	if fmt.Sprint(p) != fmt.Sprint([]uint{holding.ID}) || fmt.Sprint(tg) != fmt.Sprint([]uint{local, empty.ID}) {
		t.Fatalf("present=%v targets=%v, want present [%d] and targets [%d %d]", p, tg, holding.ID, local, empty.ID)
	}
	// Once the node has its own copy, the local storage stops being a target too.
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: local, NodeID: node.ID, Path: "/data/models/q", State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
	present, targets, err = h.storageFacts(model, node)
	if err != nil {
		t.Fatal(err)
	}
	if _, tg = ids(present, targets); fmt.Sprint(tg) != fmt.Sprint([]uint{empty.ID}) {
		t.Fatalf("targets with a local copy = %v, want [%d]", tg, empty.ID)
	}
}
