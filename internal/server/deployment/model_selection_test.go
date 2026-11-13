package deployment

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"

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

func TestTemplateExplicitModelIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ready    bool
		provider string
	}{
		{"ready with provider", true, "modelscope"},
		{"ready without provider", true, ""},
		{"unready with provider", false, "modelscope"},
		{"unready without provider", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := templateFailedRevisionFixture(t, tc.ready)
			req := f.request()
			req.ModelID = f.shared.ID
			if tc.provider != "" {
				req.ModelSource = &dto.ModelSource{Provider: tc.provider}
			}
			selection, err := f.h.applyTemplateDefaults(&req, templateHardware{NodeID: f.node.ID})
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), req.ModelID, &selection, req.TemplateID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != f.shared.ID || selection.Weight.Revision != f.shared.Revision {
				t.Fatalf("selected model %d@%s became %d@%s", f.shared.ID, f.shared.Revision, got.ID, selection.Weight.Revision)
			}
		})
	}
}

func TestTemplateExplicitPrimaryUsesSelectedMirror(t *testing.T) {
	f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, true, false)
	selection := templateSelection{
		NodeID: f.node.ID, Model: f.template,
		Spec:   &store.InferenceSpecRow{Engine: "vllm", WeightSources: `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"},{"provider":"huggingface","repo_id":"Mirror/Deploy-FP8"}]`},
		Weight: weightSource{Provider: "huggingface", RepoID: "Mirror/Deploy-FP8", Revision: "template-rev"},
		Components: []deploy.ComponentBinding{{Name: "main", Role: "primary", OriginalModelID: f.template.ModelID,
			Provider: "huggingface", RepoID: "Mirror/Deploy-FP8", Revision: "template-rev", MountPath: "/model"}},
	}
	got, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), f.shared.ID, &selection, f.template.ID)
	if err != nil {
		t.Fatal(err)
	}
	primary, _ := deploy.PrimaryComponent(selection.Components)
	if got.ID != f.shared.ID || primary.StoreModelID != got.ID || primary.Provider != got.Source || primary.RepoID != got.SourceID || primary.Revision != got.Revision {
		t.Fatalf("primary component replaced selected artifact: model=%+v primary=%+v", got, primary)
	}
}

func TestTemplateAutomaticModelRejectsFailedArtifact(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, withNode := range []bool{false, true} {
			t.Run(fmt.Sprintf("provider=%t/node=%t", explicit, withNode), func(t *testing.T) {
				f, bad := templateFailedRevisionFixture(t, false)
				// Remove the other registrations so a failed exact match cannot hide
				// behind another candidate, or be upserted again as a new download.
				for _, model := range []*store.Model{f.hf, f.shared} {
					if err := f.h.d.Store.DeleteModel(model.ID); err != nil {
						t.Fatal(err)
					}
				}
				nodeID := uint(0)
				if withNode {
					nodeID = f.node.ID
				}
				got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: bad.Source, RepoID: bad.SourceID, Revision: bad.Revision}, f.template, "vllm", "", 0, nodeID, nil, explicit)
				if err == nil || got != nil {
					t.Fatalf("failed artifact accepted: model=%+v err=%v", got, err)
				}
				models, err := f.h.d.Store.ListModels()
				if err != nil || len(models) != 1 || models[0].ID != bad.ID {
					t.Fatalf("failure must not recreate a model: models=%+v err=%v", models, err)
				}
			})
		}
	}
}

func TestTemplateAutomaticModelPrefersReadableRevision(t *testing.T) {
	f, bad := templateFailedRevisionFixture(t, true)
	got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: bad.Source, RepoID: bad.SourceID, Revision: bad.Revision}, f.template, "vllm", "", 0, f.node.ID, nil)
	if err != nil || got == nil || got.ID != f.shared.ID {
		t.Fatalf("readable revision lost to failed exact match: model=%+v err=%v", got, err)
	}
}

func TestTemplateAutomaticModelPreservesAcquisitionStates(t *testing.T) {
	for _, state := range []string{"", proto.MFPending, proto.MFDownloading, proto.MFPresent} {
		t.Run("state="+state, func(t *testing.T) {
			f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, false, false)
			if state != "" {
				if _, err := f.h.d.Store.UpsertModelFile(&store.ModelFile{ModelID: f.hf.ID, StorageID: f.nfs.ID, Path: "Acme/hf", State: state}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: f.hf.Source, RepoID: f.hf.SourceID, Revision: f.hf.Revision}, f.template, "vllm", "", 0, f.node.ID, nil, true)
			if err != nil || got.ID != f.hf.ID {
				t.Fatalf("normal acquisition state %q rejected: model=%+v err=%v", state, got, err)
			}
		})
	}
}

func TestTemplateAutomaticModelUsesOtherViableCopy(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		withNode    bool
		pinFailed   bool
	}{
		{"ready local copy", proto.MFPresent, true, false},
		{"pending local copy", proto.MFPending, true, false},
		{"ready copy before node selection", proto.MFPresent, false, false},
		{"pin excludes healthy copy", proto.MFPresent, true, true},
		{"pin before node selection", proto.MFPresent, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, model := templateFailedRevisionFixture(t, false)
			if _, err := f.h.d.Store.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: f.h.d.Store.DefaultLocalStorageID(), NodeID: f.node.ID, Path: "/models/other-copy", State: tc.state}); err != nil {
				t.Fatal(err)
			}
			nodeID := uint(0)
			if tc.withNode {
				nodeID = f.node.ID
			}
			var pins []uint
			if tc.pinFailed {
				pins = []uint{f.nfs.ID}
			}
			got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: model.Source, RepoID: model.SourceID, Revision: model.Revision}, f.template, "vllm", "", 0, nodeID, pins, true)
			if tc.pinFailed {
				if err == nil || got != nil {
					t.Fatalf("excluded copy admitted failed artifact: model=%+v err=%v", got, err)
				}
			} else if err != nil || got.ID != model.ID {
				t.Fatalf("one failed location rejected a viable model: model=%+v err=%v", got, err)
			}
		})
	}
}

func TestTemplateAutomaticModelReadFailureDoesNotRegister(t *testing.T) {
	f, model := templateFailedRevisionFixture(t, true)
	failure := errors.New("model-file observation unavailable")
	const callback = "test:model-selection-file-read-failure"
	if err := f.h.d.Store.DB().Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "model_files" {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.h.d.Store.DB().Callback().Query().Remove(callback) })
	before, err := f.h.d.Store.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []uint{0, f.node.ID} {
		got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: model.Source, RepoID: model.SourceID, Revision: "not-registered"}, f.template, "vllm", "", 0, nodeID, nil, true)
		if !errors.Is(err, failure) || got != nil {
			t.Fatalf("failed observation became a new registration: model=%+v err=%v", got, err)
		}
	}
	after, err := f.h.d.Store.ListModels()
	if err != nil || len(after) != len(before) {
		t.Fatalf("query failure changed model registrations: before=%d after=%d err=%v", len(before), len(after), err)
	}
}
