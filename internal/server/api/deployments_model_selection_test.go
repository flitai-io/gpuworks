package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func templateFailedRevisionFixture(t *testing.T, ready bool) (*templateStorageFixture, *store.Model) {
	t.Helper()
	f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, ready, false)
	catalog := deploymentFlatFixture()
	catalog.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"Acme/Deploy-FP8","revision":"template-rev"},{"provider":"modelscope","repo_id":"Acme/Deploy-FP8","revision":"template-rev"}]`
	if err := f.h.d.Store.ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	bad, err := f.h.d.Store.UpsertModel(&store.Model{Source: "modelscope", SourceID: f.shared.SourceID, Revision: "template-rev"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.d.Store.UpsertModelFile(&store.ModelFile{ModelID: bad.ID, StorageID: f.nfs.ID, Path: "Acme/failed-revision", State: proto.MFFailed, Message: "revision returned HTTP 404"}); err != nil {
		t.Fatal(err)
	}
	return f, bad
}

func TestTemplateModelSelectionHTTP(t *testing.T) {
	for _, tc := range []struct{ modelSource, provider string }{
		{"modelscope", "modelscope"},
		{"modelscope", "huggingface"},
		{"huggingface", "modelscope"},
		{"huggingface", "huggingface"},
	} {
		t.Run(tc.modelSource+"/"+tc.provider, func(t *testing.T) {
			f, _ := templateFailedRevisionFixture(t, true)
			if err := f.h.d.Store.DB().Create(&store.GPU{NodeID: f.node.ID, Index: 0, UUID: "GPU-selection", Name: "NVIDIA H100", Vendor: "nvidia", ComputeCapability: "9.0", ArchID: "sm_90", MemoryTotal: 80 << 30}).Error; err != nil {
				t.Fatal(err)
			}
			f.h.d.Online = func(uuid string) bool { return uuid == f.node.UUID }
			selected := f.shared
			if tc.modelSource == f.hf.Source {
				selected = f.hf
				if _, err := f.h.d.Store.UpsertModelFile(&store.ModelFile{ModelID: selected.ID, StorageID: f.nfs.ID, Path: "Acme/hf-ready", State: proto.MFPresent, SizeBytes: 1024}); err != nil {
					t.Fatal(err)
				}
			}
			body := fmt.Sprintf(`{"name":"selected-model","model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","node_uuid":%q,"model_source":{"provider":%q,"storage_id":%d},"quick_params":{},"placement":{}}`, selected.ID, f.template.ID, f.node.UUID, tc.provider, f.nfs.ID)
			status, raw := evaluateForFieldErrors(t, f.h, body)
			if status != http.StatusOK {
				t.Fatalf("evaluate status=%d body=%v", status, raw)
			}
			if string(raw["ok"]) != "true" || !strings.Contains(string(raw["acquisition"]), `"weights_present":true`) {
				t.Fatalf("evaluation lost the readable selected artifact: ok=%s acquisition=%s", raw["ok"], raw["acquisition"])
			}
			rec := httptest.NewRecorder()
			f.h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if rec.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
			}
			var dep dto.Deployment
			if err := json.Unmarshal(rec.Body.Bytes(), &dep); err != nil {
				t.Fatal(err)
			}
			stored, err := f.h.d.Store.GetDeployment(dep.ID)
			if err != nil || stored.ModelID != selected.ID || stored.SaveToStorageID != f.nfs.ID {
				t.Fatalf("create persisted a substituted model: deployment=%+v err=%v", stored, err)
			}
			snapshot, err := deploy.DecodeCatalogSelection([]byte(stored.CatalogSelectionSnapshot))
			if err != nil || snapshot.WeightProvider != selected.Source || snapshot.WeightRevision != selected.Revision {
				t.Fatalf("snapshot differs from selected model: snapshot=%+v model=%+v err=%v", snapshot, selected, err)
			}
		})
	}
}
