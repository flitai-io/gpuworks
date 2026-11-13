package deployment

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEmptyRevisionResolvesPinnedLocalIdentity(t *testing.T) {
	st := newAcquisitionStore(t)
	node, err := st.RegisterNode("pinned", store.HashToken("token"), "pinned")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Model", Revision: "commit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/pinned"}); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	source := weightSource{Provider: "huggingface", RepoID: "Acme/Model"}
	if !TemplateWeightMatchesModel(source, model) {
		t.Fatal("a registered copy of the same repository must satisfy an unpinned source")
	}
	selected, err := h.selectWeightSource(store.InferenceSpecRow{WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Model"}]`}, node.ID, nil)
	if err != nil || selected.Revision != "commit" {
		t.Fatalf("selection=%+v %v", selected, err)
	}
	got, err := h.resolveTemplateWeightModelForNode(source, &store.InferenceModel{ModelID: "acme/model"}, "vllm", "", 0, node.ID, nil)
	if err != nil || got.ID != model.ID {
		t.Fatalf("model=%+v %v", got, err)
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%v %v", models, err)
	}
}

func TestExplicitProviderConstrainsPublicDownloadIdentity(t *testing.T) {
	st := newAcquisitionStore(t)
	hf, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Model", Revision: "hf"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	selection := templateSelection{Model: &store.InferenceModel{ModelID: "acme/model"}, Spec: &store.InferenceSpecRow{Engine: "vllm", WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Model","revision":"hf"},{"provider":"modelscope","repo_id":"Acme/Model","revision":"ms"}]`}}
	if err := h.selectExplicitWeight(&selection, &dto.ModelSource{Provider: "modelscope"}); err != nil {
		t.Fatal(err)
	}
	model, err := h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, 1)
	if err != nil || model.Source != "modelscope" || model.Revision != "ms" {
		t.Fatalf("model=%+v %v", model, err)
	}
	model, err = h.resolveDeploymentModelFromTemplate(context.Background(), hf.ID, &selection, 1)
	if err != nil || model.ID != hf.ID || selection.Weight.Provider != hf.Source || selection.Weight.Revision != hf.Revision {
		t.Fatalf("explicit model identity was not retained: model=%+v selection=%+v err=%v", model, selection.Weight, err)
	}
	unrelated, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Other/Model", Revision: "hf"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.resolveDeploymentModelFromTemplate(context.Background(), unrelated.ID, &selection, 1); err == nil {
		t.Fatal("unrelated model_id accepted")
	}
}

// A request that chooses a provider moves the draft along with the primary:
// a node that reaches only ModelScope would otherwise download the primary
// from it and then stall on a draft still bound to Hugging Face. A component
// that declares nothing at the chosen provider keeps the source it had.
func TestExplicitProviderAlsoSelectsComponentSources(t *testing.T) {
	st := newAcquisitionStore(t)
	h := New(Deps{Store: st})
	spec := store.InferenceSpecRow{Name: "glm-tp2", Engine: "callable",
		WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Main","revision":"hf-main"},{"provider":"modelscope","repo_id":"Acme/Main","revision":"ms-main"}]`,
		Components: `[
		{"name":"primary","role":"primary","weights":{"quantization":"q4","sources":[{"provider":"huggingface","repo_id":"Acme/Main","revision":"hf-main"},{"provider":"modelscope","repo_id":"Acme/Main","revision":"ms-main"}]},"weight_bytes":100,"mount_path":"/model"},
		{"name":"draft","role":"speculative_draft","model_id":"acme/draft","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"Acme/Draft","revision":"hf-draft"},{"provider":"modelscope","repo_id":"Mirror/Draft","revision":"ms-draft"}]},"weight_bytes":10,"mount_path":"/draft"},
		{"name":"verifier","role":"verifier","model_id":"acme/verifier","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"Acme/Verifier","revision":"hf-verifier"}]},"weight_bytes":10,"mount_path":"/verifier"}]`}
	selection := templateSelection{Model: &store.InferenceModel{ModelID: "acme/main"}, Spec: &spec}
	if err := h.selectTemplateComponents(&selection, spec, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.selectExplicitWeight(&selection, &dto.ModelSource{Provider: "modelscope"}); err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{
		"primary":  {"modelscope", "Acme/Main", "ms-main"},
		"draft":    {"modelscope", "Mirror/Draft", "ms-draft"},
		"verifier": {"huggingface", "Acme/Verifier", "hf-verifier"},
	}
	for _, c := range selection.Components {
		if got := [3]string{c.Provider, c.RepoID, c.Revision}; got != want[c.Name] {
			t.Fatalf("component %s source = %v, want %v", c.Name, got, want[c.Name])
		}
	}
	if len(selection.Components) != len(want) {
		t.Fatalf("components = %+v", selection.Components)
	}
}

// A draft published under the same repository id at both providers resolves to
// the provider the request chose even when a mirror row of that repository is
// already registered. Matching by repository alone would hand back the
// Hugging Face row, so the draft would still be fetched from the provider the
// node cannot reach.
func TestExplicitProviderComponentIgnoresARegisteredMirrorRow(t *testing.T) {
	st := newAcquisitionStore(t)
	hfDraft, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Draft", Revision: "hf-draft"})
	if err != nil {
		t.Fatal(err)
	}
	primary, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Main", Revision: "ms-main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acme/main", "acme/draft"} {
		fixture := deploymentFlatFixture()
		fixture.ModelID = id
		if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
			t.Fatal(err)
		}
	}
	h := New(Deps{Store: st})
	spec := store.InferenceSpecRow{Name: "glm-tp2", Engine: "callable",
		WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Main","revision":"hf-main"},{"provider":"modelscope","repo_id":"Acme/Main","revision":"ms-main"}]`,
		Components: `[
		{"name":"primary","role":"primary","weights":{"quantization":"q4","sources":[{"provider":"huggingface","repo_id":"Acme/Main","revision":"hf-main"},{"provider":"modelscope","repo_id":"Acme/Main","revision":"ms-main"}]},"weight_bytes":100,"mount_path":"/model"},
		{"name":"draft","role":"speculative_draft","model_id":"acme/draft","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"Acme/Draft","revision":"hf-draft"},{"provider":"modelscope","repo_id":"Acme/Draft","revision":"ms-draft"}]},"weight_bytes":10,"mount_path":"/draft"}]`}
	selection := templateSelection{Model: &store.InferenceModel{ModelID: "acme/main"}, Spec: &spec}
	if err := h.selectTemplateComponents(&selection, spec, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.selectExplicitWeight(&selection, &dto.ModelSource{Provider: "modelscope"}); err != nil {
		t.Fatal(err)
	}
	if err := h.resolveTemplateComponentModels(&selection, primary); err != nil {
		t.Fatal(err)
	}
	for _, c := range selection.Components {
		model, err := st.GetModel(c.StoreModelID)
		if err != nil {
			t.Fatalf("component %s: %v", c.Name, err)
		}
		if model.Source != "modelscope" {
			t.Fatalf("component %s bound to %s row %d, want modelscope", c.Name, model.Source, model.ID)
		}
		if c.Name == "draft" && (model.ID == hfDraft.ID || model.Revision != "ms-draft") {
			t.Fatalf("draft model = %+v, want a new modelscope row at ms-draft", model)
		}
	}
	hfPrimary, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Main", Revision: "hf-main"})
	if err != nil {
		t.Fatal(err)
	}
	model, err := h.resolveDeploymentModelFromTemplate(context.Background(), hfPrimary.ID, &selection, 1)
	if err != nil || model.ID != hfPrimary.ID {
		t.Fatalf("selected primary model was not retained: model=%+v err=%v", model, err)
	}
	for _, c := range selection.Components {
		if c.Role == "primary" {
			if c.StoreModelID != hfPrimary.ID || c.Provider != hfPrimary.Source {
				t.Fatalf("primary component lost the selected model: %+v", c)
			}
		} else if c.Name == "draft" && (c.Provider != "modelscope" || c.Revision != "ms-draft") {
			t.Fatalf("explicit draft source changed with the primary: %+v", c)
		}
	}
}
