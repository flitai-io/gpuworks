package deployment

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func modelEvictionStore(t *testing.T, suffix string) *store.Store {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name() + "_" + suffix)
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func modelEvictionFixture(t *testing.T, st *store.Store) (*store.Model, *store.Node) {
	t.Helper()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("eviction-node", store.HashToken("token"), "eviction-node")
	if err != nil {
		t.Fatal(err)
	}
	return model, node
}
