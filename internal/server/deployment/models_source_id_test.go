package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Correcting a local model's source id to the repository id (what PATCH
// /models/{id} stores) is what makes a template's weight source see it as
// registered.
func TestRenamedLocalModelServesTheTemplateWeightSource(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "local", SourceID: "Qwen3-0.6B", Revision: "local"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	selection := templateSelection{Spec: &store.InferenceSpecRow{WeightSources: `[{"provider":"modelscope","repo_id":"Qwen/Qwen3-0.6B"}]`}}
	if options := h.evaluateWeightSourceOptions(selection); len(options) != 1 || options[0].Registered {
		t.Fatalf("before rename: %+v", options)
	}
	if _, _, err := st.SetModelSourceID(model.ID, "Qwen/Qwen3-0.6B"); err != nil {
		t.Fatal(err)
	}
	if options := h.evaluateWeightSourceOptions(selection); len(options) != 1 || !options[0].Registered {
		t.Fatalf("after rename: %+v", options)
	}
}
