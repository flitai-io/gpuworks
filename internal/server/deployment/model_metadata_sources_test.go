package deployment

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func metadataSourcesStore(t *testing.T, name string) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

type metadataReads struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *metadataReads) hook(meta *modelmeta.Meta) func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
	return func(_ context.Context, _, _, _, _ string, candidates []string) (*modelmeta.Meta, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, candidates)
		copied := *meta
		return &copied, nil
	}
}

// Admission reads the source once per model, however many nodes will have to
// fetch it.
func TestAdmissionReadsTheSourceOncePerModel(t *testing.T) {
	st := metadataSourcesStore(t, "api_admission_once")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main"})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*store.Node
	for i := 0; i < 3; i++ {
		node, err := st.RegisterNode(fmt.Sprintf("admit-once-%d", i), store.HashToken("t"), fmt.Sprintf("admit-once-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	reads := &metadataReads{}
	h := New(Deps{Store: st, FetchModelMeta: reads.hook(&modelmeta.Meta{})})
	if err := h.admitDeploymentModel(context.Background(), model, store.SourceRules{}, nodes); err != nil {
		t.Fatal(err)
	}
	if len(reads.calls) != 1 {
		t.Fatalf("source read %d times for %d nodes, want once", len(reads.calls), len(nodes))
	}
}
