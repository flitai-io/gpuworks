package store

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func seedOpsInstance(t *testing.T, st *Store, d *Deployment, n *Node, uuid, state string, apiPort int, ports string) {
	t.Helper()
	if _, err := st.CreateInstance(&Instance{UUID: uuid, DeploymentID: d.ID, NodeID: n.ID, State: state, APIPort: apiPort, Ports: ports, Message: "before", RuntimeFailure: "{}"}); err != nil {
		t.Fatal(err)
	}
}

// StopInstancesOnNodes moves the instances that still run or are on their way
// out (draining) on the named nodes to removing, writes the reason and
// announces each on its timeline, and touches nothing else: finished
// instances, ones already removing, other nodes and other deployments' replicas
// elsewhere.
func TestStopInstancesOnNodes(t *testing.T) {
	st := openKubeStore(t)
	c, err := st.CreateKubeCluster(KubeCluster{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	leaving, _, err := st.EnrollKubeNode(c.Token, "uid", "leaving")
	if err != nil {
		t.Fatal(err)
	}
	staying, _, err := st.EnrollKubeNode(c.Token, "uid", "staying")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := st.CreateDeployment(&Deployment{Name: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	embed, err := st.CreateDeployment(&Deployment{Name: "embed"})
	if err != nil {
		t.Fatal(err)
	}
	seedOpsInstance(t, st, chat, leaving, "run", proto.InstRunning, 0, "")
	seedOpsInstance(t, st, embed, leaving, "pull", proto.InstPulling, 0, "")
	seedOpsInstance(t, st, embed, leaving, "drain", proto.InstDraining, 0, "")
	seedOpsInstance(t, st, chat, leaving, "gone", proto.InstRemoving, 0, "")
	seedOpsInstance(t, st, chat, leaving, "done", proto.InstStopped, 0, "")
	seedOpsInstance(t, st, chat, leaving, "bad", proto.InstFailed, 0, "")
	seedOpsInstance(t, st, chat, staying, "elsewhere", proto.InstRunning, 0, "")

	got, err := st.StopInstancesOnNodes([]uint{leaving.ID}, "node leaves")
	if err != nil {
		t.Fatal(err)
	}
	want := []StoppedInstance{
		{InstanceUUID: "run", DeploymentID: chat.ID, Deployment: "chat", NodeUUID: leaving.UUID},
		{InstanceUUID: "pull", DeploymentID: embed.ID, Deployment: "embed", NodeUUID: leaving.UUID},
		{InstanceUUID: "drain", DeploymentID: embed.ID, Deployment: "embed", NodeUUID: leaving.UUID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stopped = %+v\nwant %+v", got, want)
	}
	for uuid, state := range map[string]string{"run": proto.InstRemoving, "pull": proto.InstRemoving, "drain": proto.InstRemoving,
		"gone": proto.InstRemoving, "done": proto.InstStopped, "bad": proto.InstFailed, "elsewhere": proto.InstRunning} {
		inst, err := st.InstanceByUUID(uuid)
		if err != nil || inst.State != state {
			t.Errorf("%s = %v, %v; want %s", uuid, inst, err, state)
		}
	}
	if inst, _ := st.InstanceByUUID("run"); inst.Message != "node leaves" || inst.RuntimeFailure != "" {
		t.Errorf("run: message %q, failure %q", inst.Message, inst.RuntimeFailure)
	}
	if inst, _ := st.InstanceByUUID("gone"); inst.Message != "before" {
		t.Errorf("an instance already removing was rewritten: %q", inst.Message)
	}
	events, err := st.InstanceEventsByUUID("run", 10)
	if err != nil || len(events) != 2 || events[1].Code != instanceEventStateChanged || events[1].Data != `{"detail":"node leaves","state":"removing"}` {
		t.Fatalf("events = %+v, %v", events, err)
	}

	// Asking again finds nothing left to stop, and no nodes asks nothing.
	if again, err := st.StopInstancesOnNodes([]uint{leaving.ID}, "node leaves"); err != nil || len(again) != 0 {
		t.Fatalf("second stop = %+v, %v", again, err)
	}
	if none, err := st.StopInstancesOnNodes(nil, "x"); err != nil || none != nil {
		t.Fatalf("no nodes = %+v, %v", none, err)
	}
}

// KubeClusterInstancePorts lists the ports of the entry's unfinished
// instances only, the API port included once, ascending.
func TestKubeClusterInstancePorts(t *testing.T) {
	st := openKubeStore(t)
	a, err := st.CreateKubeCluster(KubeCluster{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateKubeCluster(KubeCluster{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	na, _, _ := st.EnrollKubeNode(a.Token, "uid-a", "na")
	nb, _, _ := st.EnrollKubeNode(b.Token, "uid-b", "nb")
	d, err := st.CreateDeployment(&Deployment{Name: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	seedOpsInstance(t, st, d, na, "block", proto.InstRunning, 40001, "[40001,40003,40002]")
	seedOpsInstance(t, st, d, na, "api-only", proto.InstStarting, 40010, "")
	seedOpsInstance(t, st, d, na, "none", proto.InstPending, 0, "")
	seedOpsInstance(t, st, d, na, "done", proto.InstStopped, 40020, "[40020]")
	seedOpsInstance(t, st, d, nb, "other", proto.InstRunning, 40030, "[40030]")

	got, err := st.KubeClusterInstancePorts(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []KubeInstancePorts{
		{InstanceUUID: "block", Deployment: "chat", NodeUUID: na.UUID, Ports: []int{40001, 40002, 40003}},
		{InstanceUUID: "api-only", Deployment: "chat", NodeUUID: na.UUID, Ports: []int{40010}},
		{InstanceUUID: "none", Deployment: "chat", NodeUUID: na.UUID, Ports: []int{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %+v\nwant %+v", got, want)
	}
	if got, err := st.KubeClusterInstancePorts(999); err != nil || len(got) != 0 {
		t.Fatalf("no such entry = %+v, %v", got, err)
	}
}
