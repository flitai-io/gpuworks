package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Cover model selection x template selection x all public-source shapes.
// A registered model is exercised separately for local, HF and ModelScope.
// With no template selected, the catalog's sources must have no effect.
// Empty sources are invalid catalog input; seed that row directly to also
// verify the evaluate/create boundary refuses an incomplete stored template.
func TestAcquisitionProviderDeploymentMatrix(t *testing.T) {
	for _, modelSource := range []string{"none", "local", "huggingface", "modelscope"} {
		for _, useTemplate := range []bool{false, true} {
			for _, sources := range []struct {
				name      string
				providers []string
			}{
				{name: "zero"},
				{name: "modelscope", providers: []string{"modelscope"}},
				{name: "huggingface", providers: []string{"huggingface"}},
				{name: "both", providers: []string{"huggingface", "modelscope"}},
			} {
				name := fmt.Sprintf("model=%s/template=%t/sources=%s", modelSource, useTemplate, sources.name)
				t.Run(name, func(t *testing.T) {
					st := newAcquisitionStore(t)
					catalog := deploymentFlatFixture()
					weights := []inferencecatalog.WeightSource{}
					for _, provider := range sources.providers {
						weights = append(weights, inferencecatalog.WeightSource{Provider: provider, RepoID: "Acme/Deploy-FP8", Revision: provider + "-pin"})
					}
					encoded, err := json.Marshal(weights)
					if err != nil {
						t.Fatal(err)
					}
					catalog.Specs[0].WeightSources = string(encoded)
					if err := st.ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
						t.Fatal(err)
					}
					template, err := st.InferenceModelByModelID(catalog.ModelID)
					if err != nil {
						t.Fatal(err)
					}
					node, err := st.RegisterNode("matrix-node", store.HashToken("matrix-token"), "matrix-node")
					if err != nil {
						t.Fatal(err)
					}
					if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "GPU-matrix", Name: "NVIDIA H100",
						Vendor: "nvidia", ComputeCapability: "9.0", ArchID: "sm_90", MemoryTotal: 80 << 30}).Error; err != nil {
						t.Fatal(err)
					}
					h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }}}
					body := map[string]any{"name": "matrix", "engine": "vllm", "image": catalog.Specs[0].Image,
						"node_uuid": node.UUID, "quick_params": map[string]any{}, "placement": map[string]any{}}
					var selected *store.Model
					if modelSource != "none" {
						selected, err = st.UpsertModel(&store.Model{Source: modelSource, SourceID: "Acme/Deploy-FP8",
							Revision: modelSource + "-pin", Name: "Deploy-FP8", WeightBytes: 1 << 30})
						if err != nil {
							t.Fatal(err)
						}
						if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: selected.ID, NodeID: node.ID,
							State: proto.MFPresent, Path: "/models/deploy-fp8", SizeBytes: 1 << 30}); err != nil {
							t.Fatal(err)
						}
						body["model_id"] = selected.ID
					}
					if useTemplate {
						body["template_id"] = template.ID
						body["template_spec_source"] = "upstream"
						body["template_spec_name"] = "fp8-balanced"
					}
					call := func(path string, handler http.HandlerFunc) *httptest.ResponseRecorder {
						t.Helper()
						raw, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						rec := httptest.NewRecorder()
						handler(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)))
						return rec
					}
					evaluated := call("/api/v1/evaluate", h.evaluate)
					if useTemplate && len(sources.providers) == 0 {
						for _, rec := range []*httptest.ResponseRecorder{evaluated, call("/api/v1/deployments", h.createDeployment)} {
							if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "has no weight source") {
								t.Fatalf("empty template sources: status=%d body=%s", rec.Code, rec.Body.String())
							}
						}
						return
					}
					if !useTemplate && selected == nil {
						var wire map[string]json.RawMessage
						if err := json.Unmarshal(evaluated.Body.Bytes(), &wire); err != nil {
							t.Fatal(err)
						}
						if evaluated.Code != http.StatusOK || string(wire["ok"]) != "false" || !bytes.Contains(wire["errors"], []byte(`"field":"model_id"`)) {
							t.Fatalf("missing model must be a field error: status=%d body=%s", evaluated.Code, evaluated.Body.String())
						}
						created := call("/api/v1/deployments", h.createDeployment)
						if created.Code != http.StatusBadRequest || !strings.Contains(created.Body.String(), "not found") {
							t.Fatalf("missing model create: status=%d body=%s", created.Code, created.Body.String())
						}
						return
					}
					var wantProviders []string
					if !useTemplate && modelSource != "local" {
						wantProviders = []string{modelSource}
					} else if useTemplate && (selected == nil || slices.Contains(sources.providers, modelSource)) {
						wantProviders = sources.providers
					}
					checkEvaluation := func(rec *httptest.ResponseRecorder) {
						t.Helper()
						var resp dto.EvaluateResponse
						if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
							t.Fatal(err)
						}
						if rec.Code != http.StatusOK || !resp.OK || resp.Acquisition == nil {
							t.Fatalf("evaluate: status=%d body=%s", rec.Code, rec.Body.String())
						}
						var got []string
						for _, candidate := range resp.Acquisition.Providers {
							got = append(got, candidate.Label)
						}
						if !reflect.DeepEqual(got, wantProviders) {
							t.Fatalf("providers=%v want=%v", got, wantProviders)
						}
					}
					checkEvaluation(evaluated)
					// The form locks a registered source only when it is offered;
					// with no model chosen, two sources offer a user choice. UI tests
					// separately exercise the real form and payload serializer.
					choice := map[string]any{"storage_id": st.DefaultLocalStorageID()}
					if selected != nil && slices.Contains(wantProviders, modelSource) {
						choice["provider"] = modelSource
					} else if selected == nil && len(wantProviders) > 1 {
						choice["provider"] = wantProviders[0]
					}
					body["model_source"] = choice
					checkEvaluation(call("/api/v1/evaluate", h.evaluate))
					created := call("/api/v1/deployments", h.createDeployment)
					if created.Code != http.StatusCreated {
						t.Fatalf("create: status=%d body=%s", created.Code, created.Body.String())
					}
					var result dto.Deployment
					if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					stored, err := st.GetDeployment(result.ID)
					if err != nil {
						t.Fatal(err)
					}
					if stored.SaveToStorageID != st.DefaultLocalStorageID() {
						t.Fatalf("save-to storage changed: %+v", stored)
					}
					model, err := st.GetModel(stored.ModelID)
					if err != nil {
						t.Fatal(err)
					}
					if selected != nil {
						if model.ID != selected.ID || model.Source != selected.Source || model.Revision != selected.Revision {
							t.Fatalf("selected model identity changed: got=%+v want=%+v", model, selected)
						}
					} else if model.Source != sources.providers[0] || model.Revision != sources.providers[0]+"-pin" {
						t.Fatalf("automatic model selection: %+v", model)
					}
					if useTemplate {
						snapshot, err := deploy.DecodeCatalogSelection([]byte(stored.CatalogSelectionSnapshot))
						if err != nil || snapshot.WeightProvider != model.Source || snapshot.WeightRevision != model.Revision {
							t.Fatalf("snapshot differs from selected model: snapshot=%+v err=%v", snapshot, err)
						}
					}
				})
			}
		}
	}
}
