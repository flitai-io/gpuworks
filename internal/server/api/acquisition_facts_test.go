package api

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func jsonUint(id uint) string {
	encoded, _ := json.Marshal(id)
	return string(encoded)
}

// A peer candidate is addressed by uuid but must be presentable to a person,
// so the display name falls back through the fields most likely to be set.
func TestNodeDisplayNamePrefersTheNameAPersonGaveTheNode(t *testing.T) {
	for _, tc := range []struct {
		name string
		node *store.Node
		want string
	}{
		{"an operator-given name wins", &store.Node{Name: "gpu3-turing", Hostname: "host-1", UUID: "abcdef0123456789"}, "gpu3-turing"},
		{"the reported hostname is next", &store.Node{Hostname: "host-1", UUID: "abcdef0123456789"}, "host-1"},
		{"a short uuid prefix is the last resort", &store.Node{UUID: "abcdef0123456789"}, "abcdef01"},
		{"a short uuid is used whole", &store.Node{UUID: "abc"}, "abc"},
		{"blank fields are skipped", &store.Node{Name: "   ", Hostname: "  ", UUID: "abcdef0123456789"}, "abcdef01"},
		{"a node with nothing yields nothing", &store.Node{}, ""},
		{"a nil node yields nothing", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeDisplayName(tc.node); got != tc.want {
				t.Fatalf("nodeDisplayName = %q, want %q", got, tc.want)
			}
		})
	}
}

// Deployment read APIs must carry a human-readable node name alongside the
// uuid, so no client has to render a raw identifier to an operator.
func TestDeploymentAndPresenceCarryNodeNames(t *testing.T) {
	st, err := store.Open("file:api_node_names?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	target, err := st.RegisterNode("uuid-target", store.HashToken("t1"), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	source, err := st.RegisterNode("uuid-source", store.HashToken("t2"), "host-b")
	if err != nil {
		t.Fatal(err)
	}
	// An operator-given name must win over the reported hostname.
	if _, err := st.RenameNode(target.UUID, "gpu3-turing"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RenameNode(source.UUID, "gpu4-5090"); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: target.ID, Path: "/models/m", State: proto.MFSyncing,
		SizeBytes: 10, Downloaded: 5, TransferSourceNodeID: source.ID,
	}); err != nil {
		t.Fatal(err)
	}
	deployment, err := st.CreateDeployment(&store.Deployment{
		Name: "d", ModelID: model.ID, NodeID: target.ID, Engine: "vllm", ServedName: "m", State: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}

	out := h.deploymentDTO(deployment, true)
	if out.NodeName != "gpu3-turing" {
		t.Fatalf("deployment node name = %q, want the node's name", out.NodeName)
	}
	if out.NodeUUID != target.UUID {
		t.Fatalf("the uuid must remain authoritative: %q", out.NodeUUID)
	}
	if len(out.ModelProgress) != 1 || out.ModelProgress[0].NodeName != "gpu3-turing" {
		t.Fatalf("model progress = %+v, want the node named", out.ModelProgress)
	}

	// The model read path names both the holder and the sync source.
	dto := h.modelDTO(model, true)
	if len(dto.Nodes) != 1 {
		t.Fatalf("model nodes = %+v", dto.Nodes)
	}
	if dto.Nodes[0].NodeName != "gpu3-turing" || dto.Nodes[0].SourceNodeName != "gpu4-5090" {
		t.Fatalf("presence names = %+v", dto.Nodes[0])
	}
	if dto.Nodes[0].SourceNodeUUID != source.UUID {
		t.Fatalf("source uuid lost: %+v", dto.Nodes[0])
	}
}
