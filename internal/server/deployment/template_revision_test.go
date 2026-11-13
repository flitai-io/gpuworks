package deployment

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestExplicitProviderPreservesKnownTemplateRevision(t *testing.T) {
	for _, tc := range []struct {
		name, exactState string
		withoutNode      bool
		wantError        bool
	}{
		{name: "exact revision absent"},
		{name: "exact revision pending", exactState: proto.MFPending},
		{name: "exact revision failed", exactState: proto.MFFailed, wantError: true},
		{name: "exact revision ready", exactState: proto.MFPresent},
		{name: "no target and exact revision absent", withoutNode: true},
		{name: "no target and exact revision failed", exactState: proto.MFFailed, withoutNode: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, true, false)
			st := f.h.d.Store
			const requestedRevision = "requested-exact-commit"
			var exactID uint
			if tc.exactState != "" {
				exact, err := st.UpsertModel(&store.Model{Source: f.shared.Source, SourceID: f.shared.SourceID, Revision: requestedRevision})
				if err != nil {
					t.Fatal(err)
				}
				exactID = exact.ID
				if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: exact.ID, StorageID: f.nfs.ID, Path: "weights/exact", State: tc.exactState}); err != nil {
					t.Fatal(err)
				}
			}
			nodeID := f.node.ID
			if tc.withoutNode {
				nodeID = 0
			}
			selection := templateSelection{NodeID: nodeID, Model: f.template, Spec: &store.InferenceSpecRow{Engine: "vllm", WeightBytes: 1024},
				Weight: weightSource{Provider: f.shared.Source, RepoID: f.shared.SourceID, Revision: requestedRevision}, ExplicitProvider: true,
				Components: []deploy.ComponentBinding{{Name: "main", Role: "primary", OriginalModelID: f.template.ModelID, MountPath: "/model"}}}
			before, err := st.ListModels()
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, f.template.ID)
			if tc.wantError {
				if err == nil || got != nil || selection.Weight.Revision != requestedRevision {
					t.Fatalf("failed revision must not become a different ready revision: model=%+v weight=%+v err=%v", got, selection.Weight, err)
				}
				after, err := st.ListModels()
				if err != nil || len(after) != len(before) {
					t.Fatalf("failure created another registration: before=%d after=%d err=%v", len(before), len(after), err)
				}
				return
			}
			if err != nil || got == nil {
				t.Fatalf("resolve: model=%+v err=%v", got, err)
			}
			primary, _ := deploy.PrimaryComponent(selection.Components)
			if got.ID == f.shared.ID || got.Source != f.shared.Source || got.Revision != requestedRevision ||
				selection.Weight.Revision != requestedRevision || primary.StoreModelID != got.ID || primary.Revision != requestedRevision {
				t.Fatalf("known revision replaced: model=%+v weight=%+v primary=%+v", got, selection.Weight, primary)
			}
			if exactID != 0 && got.ID != exactID {
				t.Fatalf("exact registration %d replaced by %d", exactID, got.ID)
			}
		})
	}
}

func TestExplicitProviderReusesUnknownRevisionAfterExactFailure(t *testing.T) {
	r := newUnknownRevisionRig(t, unknownRevisionSpecBytes)
	r.nfsState(t, proto.StorageStateReady, true)
	for _, version := range []struct{ revision, state string }{
		{unknownRevisionPin, proto.MFFailed},
		{"another-known-commit", proto.MFPresent},
	} {
		model, err := r.st.UpsertModel(&store.Model{Source: r.imported.Source, SourceID: r.imported.SourceID, Revision: version.revision, WeightBytes: unknownRevisionSpecBytes})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: r.nfs.ID, Path: "weights/" + version.revision, State: version.state, SizeBytes: unknownRevisionSpecBytes}); err != nil {
			t.Fatal(err)
		}
	}
	before := r.modelCount(t)
	got := r.resolve(t)
	if got.ID != r.imported.ID || r.selection.Weight.Revision != "" || r.modelCount(t) != before {
		t.Fatalf("valid unknown revision import lost to another known revision: model=%+v weight=%+v", got, r.selection.Weight)
	}
}

func TestExplicitProviderResolvesUnspecifiedRevision(t *testing.T) {
	f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, true, false)
	selection := templateSelection{NodeID: f.node.ID, Model: f.template, Spec: &store.InferenceSpecRow{Engine: "vllm"},
		Weight: weightSource{Provider: f.shared.Source, RepoID: f.shared.SourceID}, ExplicitProvider: true}
	got, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, f.template.ID)
	if err != nil || got == nil || got.ID != f.shared.ID || selection.Weight.Revision != f.shared.Revision {
		t.Fatalf("unspecified revision failed to resolve to the registered identity: model=%+v weight=%+v err=%v", got, selection.Weight, err)
	}
}

func TestExplicitProviderReusesCompleteRepositoryAtPinnedRevision(t *testing.T) {
	f := newTemplateStorageFixture(t, proto.StorageNFS, true, true, true, false)
	selection := templateSelection{NodeID: f.node.ID, Model: f.template, Spec: &store.InferenceSpecRow{Engine: "vllm"},
		Weight: weightSource{Provider: f.shared.Source, RepoID: f.shared.SourceID, Revision: f.shared.Revision, Subfolder: "weights"}, ExplicitProvider: true}
	got, err := f.h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, f.template.ID)
	if err != nil || got == nil || got.ID != f.shared.ID || selection.Weight.Revision != f.shared.Revision {
		t.Fatalf("complete repository at the pinned revision must satisfy its subfolder: model=%+v weight=%+v err=%v", got, selection.Weight, err)
	}
}
