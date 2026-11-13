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
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The forecast through the evaluate handler, for a manual placement of a
// template with a draft component: first nowhere, then with the primary on
// an NFS the node reads (the component follows it there), then with the
// component already on the node.
func TestEvaluateForecastsTemplateComponents(t *testing.T) {
	st := newAcquisitionStore(t)
	fixture := deploymentFlatFixture()
	fixture.ModelID = "acme/target"
	fixture.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"Acme/Target","revision":"target-rev"}]`
	fixture.Specs[0].Components = `[
		{"name":"target","role":"primary","weights":{"quantization":"fp8","sources":[{"provider":"huggingface","repo_id":"Acme/Target","revision":"target-rev"}]},"weight_bytes":17179869184,"mount_path":"/models/target"},
		{"name":"draft","role":"speculative_draft","model_id":"acme/draft","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"Acme/Draft","revision":"draft-rev"}]},"weight_bytes":2147483648,"mount_path":"/models/draft"}
	]`
	draft := store.InferenceModel{ModelID: "acme/draft", Source: "upstream", Architecture: "draft", Specs: []store.InferenceSpecRow{{
		Name: "bf16", Engine: "vllm", Image: "image", Quantization: "bf16", WeightBytes: 2 << 30,
		WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Draft","revision":"draft-rev"}]`, GPUs: `{"vendor":"nvidia","cc":">=8.0"}`,
	}}}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture, draft}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("gpu-h", store.HashToken("h"), "gpu-h")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "GPU-h-0", Name: "NVIDIA H100", Vendor: "nvidia",
		ComputeCapability: "9.0", ArchID: "sm_90", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }}}
	evaluate := func() []dto.AcquisitionComponent {
		t.Helper()
		body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","node_uuid":%q,"quick_params":{},"placement":{}}`, template.ID, node.UUID)
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Acquisition *struct {
				Components []dto.AcquisitionComponent `json:"components"`
			} `json:"acquisition"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Acquisition == nil {
			t.Fatalf("acquisition missing: %v %s", err, rec.Body.String())
		}
		return resp.Acquisition.Components
	}
	modelBySource := func(repo string) *store.Model {
		t.Helper()
		models, err := st.ListModels()
		if err != nil {
			t.Fatal(err)
		}
		for i := range models {
			if models[i].SourceID == repo {
				return &models[i]
			}
		}
		t.Fatalf("no model row for %s in %+v", repo, models)
		return nil
	}

	got := evaluate()
	if len(got) != 1 || got[0].Name != "draft" || got[0].Role != "speculative_draft" || got[0].SizeBytes != 2<<30 ||
		got[0].Plan != deployment.ComponentPlanNodeDownload || got[0].Nodes != 1 || got[0].ReadyNodes != 0 || got[0].Storage != "" {
		t.Fatalf("nothing anywhere: components = %+v", got)
	}

	nfs, err := st.CreateStorage(&store.Storage{Name: "models-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/models-nfs", Export: "nfs.example.internal:/export/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: nfs.ID, State: proto.StorageStateReady, Path: nfs.Path, Writable: true},
	}}); err != nil {
		t.Fatal(err)
	}
	primary := modelBySource("Acme/Target")
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: primary.ID, StorageID: nfs.ID, Path: store.SharedModelRelPath(primary), State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
	if got := evaluate(); len(got) != 1 || got[0].Plan != deployment.ComponentPlanSharedDownload || !got[0].FollowsPrimary || got[0].Storage != "models-nfs" || got[0].StorageID != nfs.ID {
		t.Fatalf("primary on the NFS: components = %+v", got)
	}

	component := modelBySource("Acme/Draft")
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: component.ID, StorageID: st.DefaultLocalStorageID(), NodeID: node.ID, Path: store.SharedModelRelPath(component), State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
	if got := evaluate(); len(got) != 1 || got[0].Plan != deployment.ComponentPlanReady || got[0].ReadyNodes != 1 {
		t.Fatalf("component on the node: components = %+v", got)
	}
}
