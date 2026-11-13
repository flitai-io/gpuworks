package api

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
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
	h     *apiHandlers
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
		h: &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }}}}
}

func (r *saveToRig) nfsState(t *testing.T, state string, writable bool) {
	t.Helper()
	if err := r.st.ApplyStorageStatus(r.node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: r.nfs.ID, State: state, Path: "/mnt/gpuworks/team-nfs", Writable: writable},
	}}); err != nil {
		t.Fatal(err)
	}
}
