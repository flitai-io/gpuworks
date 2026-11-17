package store

import (
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestScaleDeploymentReplicasDrainsTheNamedInstancesAtomically(t *testing.T) {
	st, err := Open("file:store_scale_replicas_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("scale-node", HashToken("scale-token"), "scale")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active", Replicas: 3})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active", Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []Instance{
		{UUID: "keep", DeploymentID: dep.ID},
		{UUID: "drop", DeploymentID: dep.ID},
		{UUID: "elsewhere", DeploymentID: other.ID},
	} {
		row.NodeID, row.State = node.ID, proto.InstRunning
		if _, err := st.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
	}
	sub := st.Bus.Subscribe(64, "instance", "deployment")
	defer sub.Close()

	// An instance of another deployment writes nothing, not even the count.
	if err := st.ScaleDeploymentReplicas(dep.ID, 3, 2, []string{"drop", "elsewhere"}); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("foreign instance: err = %v", err)
	}
	// A count read before another scale landed writes nothing either.
	if err := st.ScaleDeploymentReplicas(dep.ID, 4, 2, []string{"drop"}); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("stale count: err = %v", err)
	}
	if got, _ := st.GetDeployment(dep.ID); got.Replicas != 3 {
		t.Fatalf("replicas after a refused scale = %d", got.Replicas)
	}
	if got, _ := st.InstanceByUUID("drop"); got.State != proto.InstRunning {
		t.Fatalf("drop after a refused scale = %s", got.State)
	}

	if err := st.ScaleDeploymentReplicas(dep.ID, 3, 2, []string{"drop"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(dep.ID); got.Replicas != 2 {
		t.Fatalf("replicas = %d", got.Replicas)
	}
	if got, _ := st.InstanceByUUID("drop"); got.State != proto.InstDraining || got.Message != replicaRemovedMessage {
		t.Fatalf("drop = %s %q", got.State, got.Message)
	}
	if got, _ := st.InstanceByUUID("keep"); got.State != proto.InstRunning {
		t.Fatalf("keep = %s", got.State)
	}
	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for !seen["instance/drop"] || !seen["deployment/"+uintKey(dep.ID)] {
		select {
		case ev := <-sub.C():
			seen[ev.Topic+"/"+ev.Key] = true
		case <-deadline:
			t.Fatalf("published = %v", seen)
		}
	}
	// The instance the controller failed meanwhile is not drained back
	// into the timeline of a live replica.
	if err := st.SetInstanceFailed("keep", proto.RuntimeFailure{Code: proto.ErrorContainerExited}, "engine exited"); err != nil {
		t.Fatal(err)
	}
	if err := st.ScaleDeploymentReplicas(dep.ID, 2, 1, []string{"keep"}); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("draining a failed instance: err = %v", err)
	}
	if got, _ := st.InstanceByUUID("keep"); got.State != proto.InstFailed {
		t.Fatalf("keep = %s, want failed kept", got.State)
	}
	if err := st.ScaleDeploymentReplicas(9999, 1, 1, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deployment: err = %v", err)
	}
}

func TestSetDeploymentReplicaUnitsWritesTheListAndCount(t *testing.T) {
	st, err := Open("file:store_replica_units_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: 1, NodeIDs: "[1,2]", NNodes: 2, State: "active", Replicas: 1, MasterAddr: "10.0.0.1", Placement: `{"tp":4,"pp":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentReplicaUnits(dep.ID, ReplicaUnitsPatch{Placement: `{"tp":4,"units":[{"node_uuid":"n1"}]}`, NodeIDs: "[1,2,3,4]", NodeID: 1, Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDeployment(dep.ID)
	if got.Replicas != 2 || got.NodeIDs != "[1,2,3,4]" || got.NodeID != 1 || got.Placement != `{"tp":4,"units":[{"node_uuid":"n1"}]}` {
		t.Fatalf("deployment = %+v", got)
	}
	if got.MasterAddr != "10.0.0.1" {
		t.Fatalf("master address = %q with the first leader unchanged, want it kept", got.MasterAddr)
	}
	// The first replica is now led from node 3: the address stored for node 1
	// is no rendezvous for it.
	if err := st.SetDeploymentReplicaUnits(dep.ID, ReplicaUnitsPatch{Placement: `{"tp":4,"units":[{"node_uuid":"n3"}]}`, NodeIDs: "[3,4]", NodeID: 3, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(dep.ID); got.NodeID != 3 || got.MasterAddr != "" {
		t.Fatalf("after the first leader moved: node %d master %q, want 3 and cleared", got.NodeID, got.MasterAddr)
	}
	if err := st.SetDeploymentReplicaUnits(9999, ReplicaUnitsPatch{Placement: "{}", NodeIDs: "[]", NodeID: 1, Replicas: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deployment: err = %v", err)
	}
}
