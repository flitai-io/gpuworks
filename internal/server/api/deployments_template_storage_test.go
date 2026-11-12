package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type templateStorageFixture struct {
	h          *apiHandlers
	node       *store.Node
	hf, shared *store.Model
	nfs        *store.Storage
	template   *store.InferenceModel
}

func newTemplateStorageFixture(t *testing.T, storageType string, scoped, mounted, complete, cached bool) *templateStorageFixture {
	t.Helper()
	st := newAcquisitionStore(t)
	f := &templateStorageFixture{h: &apiHandlers{d: Deps{Store: st}}}
	var err error
	f.node, err = st.RegisterNode("target", store.HashToken("template-storage"), "target")
	if err != nil {
		t.Fatal(err)
	}
	f.hf, err = st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Deploy-FP8", Revision: "hf-rev"})
	if err != nil {
		t.Fatal(err)
	}
	f.shared, err = st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-FP8", Revision: "ms-rev"})
	if err != nil {
		t.Fatal(err)
	}
	f.nfs, err = st.CreateStorage(&store.Storage{Name: "team-models", Type: storageType, Enabled: true, AllNodes: scoped,
		Path: "/mnt/models", Export: "nfs.example.internal:/models", CachePath: "/cache/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := proto.MFMissing
	if complete {
		state = proto.MFPresent
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: f.shared.ID, StorageID: f.nfs.ID,
		Path: "Acme/Deploy-FP8", State: state, SizeBytes: 1024}); err != nil {
		t.Fatal(err)
	}
	if mounted {
		if err := st.ApplyStorageStatus(f.node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
			{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/models", CachePath: "/cache/models", Writable: true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if cached {
		f.file(t, f.shared, f.nfs.ID)
	}
	catalog := deploymentFlatFixture()
	catalog.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"Acme/Deploy-FP8","revision":"hf-rev"},{"provider":"modelscope","repo_id":"Acme/Deploy-FP8","revision":"ms-rev"}]`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	f.template, err = st.InferenceModelByModelID(catalog.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *templateStorageFixture) file(t *testing.T, model *store.Model, storageID uint) {
	t.Helper()
	if _, err := f.h.d.Store.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: storageID, NodeID: f.node.ID,
		Path: "/models/weights", State: proto.MFPresent, SizeBytes: 1024}); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluateTemplateUsesNFSInsteadOfPublicDownload(t *testing.T) {
	f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, true, false)
	if err := f.h.d.Store.DB().Create(&store.GPU{NodeID: f.node.ID, Index: 0, UUID: "GPU-template-nfs", Name: "NVIDIA H100", Vendor: "nvidia", ComputeCapability: "9.0", ArchID: "sm_90", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	f.h.d.Online = func(uuid string) bool { return uuid == f.node.UUID }
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","node_uuid":%q,"model_id":%d,"quick_params":{},"placement":{}}`, f.template.ID, f.node.UUID, uint(0))
	rec := httptest.NewRecorder()
	f.h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK          bool `json:"ok"`
		Acquisition *struct {
			Present []struct {
				StorageID uint `json:"storage_id"`
			} `json:"present"`
			Nodes []struct {
				WeightsPlan    string `json:"weights_plan"`
				WeightsPresent bool   `json:"weights_present"`
			} `json:"nodes"`
		} `json:"acquisition"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Acquisition == nil {
		t.Fatalf("evaluation did not succeed: %s", rec.Body.String())
	}
	if len(resp.Acquisition.Present) != 1 || resp.Acquisition.Present[0].StorageID != f.nfs.ID {
		t.Fatalf("existing NFS copy not offered: %s", rec.Body.String())
	}
	if len(resp.Acquisition.Nodes) != 1 || !resp.Acquisition.Nodes[0].WeightsPresent || resp.Acquisition.Nodes[0].WeightsPlan != "present" {
		t.Fatalf("NFS-backed weights must be present, not a public download: %s", rec.Body.String())
	}
}
