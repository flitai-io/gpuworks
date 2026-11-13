package hub

import (
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInstanceStatusRequiresCurrentSessionAndOwningNode(t *testing.T) {
	st, err := store.Open("file:hub-instance-status-session?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node := store.Node{UUID: "session-node", Name: "session-node"}
	other := store.Node{UUID: "other-node", Name: "other-node"}
	if err := st.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	instance, err := st.CreateInstance(&store.Instance{UUID: "session-instance", NodeID: node.ID, State: proto.InstRunning})
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	oldSession := &Session{hub: h, NodeUUID: node.UUID, nodeID: node.ID}
	currentSession := &Session{hub: h, NodeUUID: node.UUID, nodeID: node.ID}
	h.sessions[node.UUID] = currentSession
	report := proto.InstanceStatus{UUID: instance.UUID, RuntimeObservation: &proto.InstanceRuntimeObservation{Runtime: "docker"}}
	if accepted, err := h.applyInstanceStatus(oldSession, report); err != nil || accepted {
		t.Fatalf("stale session accepted=%v err=%v", accepted, err)
	}
	if got, _ := st.InstanceByUUID(instance.UUID); got.RuntimeObservation != "" {
		t.Fatal("stale session changed observation")
	}
	if accepted, err := h.applyInstanceStatus(currentSession, report); err != nil || !accepted {
		t.Fatalf("current session accepted=%v err=%v", accepted, err)
	}

	wrongNodeSession := &Session{hub: h, NodeUUID: other.UUID, nodeID: other.ID}
	h.sessions[other.UUID] = wrongNodeSession
	if accepted, err := h.applyInstanceStatus(wrongNodeSession, report); err == nil || accepted {
		t.Fatalf("wrong-node session accepted=%v err=%v", accepted, err)
	}
}
