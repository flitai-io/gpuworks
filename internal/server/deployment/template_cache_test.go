package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestTemplateAutomaticModelScopesCachedCopiesToTarget(t *testing.T) {
	for _, tc := range []struct {
		name, owner, state string
		withoutTarget      bool
		wantAccepted       bool
	}{
		{name: "failed shared only"},
		{name: "other node ready cache", owner: "other", state: proto.MFPresent},
		{name: "other node pending cache", owner: "other", state: proto.MFPending},
		{name: "other node downloading cache", owner: "other", state: proto.MFDownloading},
		{name: "target ready cache", owner: "target", state: proto.MFPresent, wantAccepted: true},
		{name: "target pending cache", owner: "target", state: proto.MFPending, wantAccepted: true},
		{name: "cache before target selection", owner: "other", state: proto.MFPresent, withoutTarget: true, wantAccepted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTemplateStorageFixture(t, proto.StorageNFSCache, true, true, false, false)
			st := f.h.d.Store
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: f.shared.ID, StorageID: f.nfs.ID,
				Path: "Acme/Deploy-FP8", State: proto.MFFailed}); err != nil {
				t.Fatal(err)
			}
			if tc.owner != "" {
				owner := f.node.ID
				if tc.owner == "other" {
					other, err := st.RegisterNode("other", store.HashToken("other-cache"), "other")
					if err != nil {
						t.Fatal(err)
					}
					owner = other.ID
				}
				if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: f.shared.ID, StorageID: f.nfs.ID,
					NodeID: owner, Path: "/cache/models/weights", State: tc.state, SizeBytes: 1024}); err != nil {
					t.Fatal(err)
				}
			}
			pins := []uint{f.nfs.ID}
			if !tc.wantAccepted {
				source, err := st.ModelSourceOn(f.shared.ID, f.node, store.SourceRules{Pinned: pins})
				if err != nil || source.Action != store.SourceRefused || source.Reason != store.LocationModelMissingInStore {
					t.Fatalf("target must have no usable source: source=%+v err=%v", source, err)
				}
			}
			nodeID := f.node.ID
			if tc.withoutTarget {
				nodeID = 0
			}
			got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: f.shared.Source,
				RepoID: f.shared.SourceID, Revision: f.shared.Revision}, f.template, "vllm", "", 0, nodeID, pins, true)
			if tc.wantAccepted {
				if err != nil || got == nil || got.ID != f.shared.ID {
					t.Fatalf("usable copy or preparation rejected: model=%+v err=%v", got, err)
				}
			} else if err == nil || got != nil {
				t.Fatalf("foreign cache admitted failed artifact: model=%+v err=%v", got, err)
			}
		})
	}
}

func TestTemplateAutomaticModelPreservesLocalPeerCopy(t *testing.T) {
	f := newTemplateStorageFixture(t, proto.StorageNFSCache, true, true, false, false)
	st := f.h.d.Store
	other, err := st.RegisterNode("peer", store.HashToken("local-peer"), "peer")
	if err != nil {
		t.Fatal(err)
	}
	local := st.DefaultLocalStorageID()
	for _, file := range []store.ModelFile{
		{ModelID: f.shared.ID, StorageID: local, NodeID: f.node.ID, Path: "/models/weights", State: proto.MFFailed},
		{ModelID: f.shared.ID, StorageID: local, NodeID: other.ID, Path: "/models/weights", State: proto.MFPresent},
	} {
		if _, err := st.UpsertModelFile(&file); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.h.resolveTemplateWeightModelForNode(weightSource{Provider: f.shared.Source,
		RepoID: f.shared.SourceID, Revision: f.shared.Revision}, f.template, "vllm", "", 0, f.node.ID, []uint{local}, true)
	if err != nil || got == nil || got.ID != f.shared.ID {
		t.Fatalf("local peer copy must remain a candidate: model=%+v err=%v", got, err)
	}
}
