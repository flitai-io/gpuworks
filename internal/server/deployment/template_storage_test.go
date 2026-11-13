package deployment

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type templateStorageFixture struct {
	h          *Service
	node       *store.Node
	hf, shared *store.Model
	nfs        *store.Storage
	template   *store.InferenceModel
}

func newTemplateStorageFixture(t *testing.T, storageType string, scoped, mounted, complete, cached bool) *templateStorageFixture {
	t.Helper()
	st := newAcquisitionStore(t)
	f := &templateStorageFixture{h: New(Deps{Store: st})}
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

func (f *templateStorageFixture) request() dto.DeployRequest {
	return dto.DeployRequest{ModelID: f.hf.ID, TemplateID: f.template.ID, TemplateSpecSource: "upstream", TemplateSpecName: "fp8-balanced"}
}

func TestTemplateModelSelectionStorageAvailability(t *testing.T) {
	for _, tc := range []struct {
		name, storageType                                                     string
		scoped, mounted, complete, cached, localHF, requestShared, explicitHF bool
		pin                                                                   string
		wantShared                                                            bool
	}{
		{name: "NFS beats empty HF registration", storageType: proto.StorageNFS, scoped: true, mounted: true, complete: true, wantShared: true},
		{name: "requested NFS model stays selected", storageType: proto.StorageNFS, scoped: true, mounted: true, complete: true, requestShared: true, wantShared: true},
		{name: "unmounted NFS is unavailable", storageType: proto.StorageNFS, scoped: true, complete: true},
		{name: "NFS outside node scope is unavailable", storageType: proto.StorageNFS, mounted: true, complete: true},
		{name: "incomplete NFS is unavailable", storageType: proto.StorageNFS, scoped: true, mounted: true},
		{name: "storage pin excludes NFS", storageType: proto.StorageNFS, scoped: true, mounted: true, complete: true, pin: "local"},
		{name: "storage pin excludes local HF", storageType: proto.StorageNFS, scoped: true, mounted: true, complete: true, localHF: true, pin: "nfs", wantShared: true},
		{name: "ready requested local model is retained", storageType: proto.StorageNFS, scoped: true, mounted: true, complete: true, localHF: true},
		{name: "explicit provider is retained", storageType: proto.StorageNFS, scoped: true, mounted: true, complete: true, explicitHF: true},
		{name: "completed NFS cache is ready", storageType: proto.StorageNFSCache, scoped: true, mounted: true, complete: true, cached: true, wantShared: true},
		{name: "unfilled NFS cache is not ready", storageType: proto.StorageNFSCache, scoped: true, mounted: true, complete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTemplateStorageFixture(t, tc.storageType, tc.scoped, tc.mounted, tc.complete, tc.cached)
			if tc.localHF {
				f.file(t, f.hf, f.h.d.Store.DefaultLocalStorageID())
			}
			req := f.request()
			if !tc.requestShared && !(tc.localHF && tc.pin == "") && !tc.explicitHF {
				req.ModelID = 0
			}
			if tc.requestShared {
				req.ModelID = f.shared.ID
			}
			if tc.explicitHF {
				req.ModelSource = &dto.ModelSource{Provider: "huggingface"}
			}
			if tc.pin != "" {
				id := f.h.d.Store.DefaultLocalStorageID()
				if tc.pin == "nfs" {
					id = f.nfs.ID
				}
				req.NodeRequirements = map[string][]string{nodelabel.KeyStorage: {fmt.Sprint(id)}}
			}
			selection, err := f.h.applyTemplateDefaults(&req, templateHardware{NodeID: f.node.ID})
			if err != nil {
				t.Fatal(err)
			}
			want := f.hf
			if tc.wantShared {
				want = f.shared
			}
			if (!tc.localHF || tc.pin != "") && selection.Weight.Provider != want.Source {
				t.Errorf("template source = %s, want %s", selection.Weight.Provider, want.Source)
			}
			model, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), req.ModelID, &selection, req.TemplateID)
			if err != nil {
				t.Fatal(err)
			}
			if model.ID != want.ID || selection.Weight.Provider != want.Source || selection.Weight.Revision != want.Revision {
				t.Fatalf("model = %+v, weight = %+v; want model %d (%s)", model, selection.Weight, want.ID, want.Source)
			}
			if tc.wantShared {
				src, err := f.h.d.Store.ModelSourceOn(model.ID, f.node, store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements)})
				if err != nil || src.Action != store.SourceReady || src.Location.Storage.ID != f.nfs.ID {
					t.Fatalf("selected model must use the existing shared artifact: source=%+v err=%v", src, err)
				}
			}
		})
	}
}

func TestTemplateModelResolutionPrefersNFSWithOnlyHFDeclared(t *testing.T) {
	f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, true, false)
	selection := &templateSelection{NodeID: f.node.ID, Model: f.template,
		Spec: &store.InferenceSpecRow{Engine: "vllm"}, Weight: weightSource{Provider: f.hf.Source, RepoID: f.hf.SourceID, Revision: f.hf.Revision}}
	model, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), 0, selection, f.template.ID)
	if err != nil {
		t.Fatal(err)
	}
	if model.ID != f.shared.ID {
		t.Fatalf("selected model %d; want readable NFS model %d", model.ID, f.shared.ID)
	}
}

func TestTemplateModelAvailabilityPropagatesQueryFailure(t *testing.T) {
	f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, true, false)
	failure := errors.New("storage observations unavailable")
	const callback = "test:template-storage-query-failure"
	err := f.h.d.Store.DB().Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "storage_node_states" {
			tx.AddError(failure)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.h.d.Store.DB().Callback().Query().Remove(callback) })
	if ready, err := f.h.modelHasReadyArtifact(f.shared, f.node.ID, nil); ready || !errors.Is(err, failure) {
		t.Fatalf("ready=%t err=%v; must not turn a failed read into permission to download", ready, err)
	}
	req := f.request()
	if _, err := f.h.applyTemplateDefaults(&req, templateHardware{NodeID: f.node.ID}); !errors.Is(err, failure) {
		t.Fatalf("template source selection swallowed query failure: %v", err)
	}
	selection := &templateSelection{NodeID: f.node.ID, Model: f.template,
		Spec: &store.InferenceSpecRow{Engine: "vllm"}, Weight: weightSource{Provider: f.hf.Source, RepoID: f.hf.SourceID, Revision: f.hf.Revision}}
	if _, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), 0, selection, req.TemplateID); !errors.Is(err, failure) {
		t.Fatalf("model resolution swallowed query failure: %v", err)
	}
}

func TestTemplateModelSelectionWithoutTargetKeepsDeclaredSource(t *testing.T) {
	f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, true, false)
	req := f.request()
	selection, err := f.h.applyTemplateDefaults(&req, templateHardware{})
	if err != nil {
		t.Fatal(err)
	}
	model, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), req.ModelID, &selection, req.TemplateID)
	if err != nil {
		t.Fatal(err)
	}
	if model.ID != f.hf.ID {
		t.Fatalf("without a target, selected model %d based on another node's NFS", model.ID)
	}
}
