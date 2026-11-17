package controller

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A Kubernetes node's instance ports come from its entry's engine port range,
// read when a port is picked, so editing the range takes effect on the next
// instance; the range an agent reports (an older manifest still hands it
// one) and the server's both lose to it, and a node outside Kubernetes keeps
// the range its agent reports, or the server's.
func TestPickPortUsesTheKubeEntrysRange(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	entry, err := st.CreateKubeCluster(store.KubeCluster{Name: "a", Namespace: "a", ServerURL: "https://gw", EnginePorts: "40000-40002"})
	if err != nil {
		t.Fatal(err)
	}
	kubeNode, _, err := st.EnrollKubeNode(entry.Token, "uid-a", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := st.RegisterNode("plain", store.HashToken("plain-token"), "plain")
	if err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil)
	c.SetEnginePorts(30000, 30010)

	// The entry's range, whatever the agent reports: an older agent reports
	// the range its manifest named, a newer one none.
	for name, inventory := range map[string]string{
		"no report":        ``,
		"stale own range":  `{"interfaces":[],"rdma_ports":[],"engine_port_range":{"start":50000,"end":50010}}`,
		"skips a listener": `{"interfaces":[],"rdma_ports":[],"engine_port_listeners":[40000]}`,
	} {
		kubeNode.NetworkInventory = inventory
		want := 40000
		if name == "skips a listener" {
			want = 40001
		}
		if got, rejected := c.pickPort(kubeNode); rejected != nil || got != want {
			t.Fatalf("%s: pickPort = %d, %+v; want %d from the entry's range", name, got, rejected, want)
		}
	}

	// Editing the entry applies to the next pick.
	edited := *entry
	edited.EnginePorts = "41000-41000"
	if _, err := st.UpdateKubeCluster(edited); err != nil {
		t.Fatal(err)
	}
	kubeNode.NetworkInventory = ``
	if got, rejected := c.pickPort(kubeNode); rejected != nil || got != 41000 {
		t.Fatalf("after the edit: pickPort = %d, %+v; want 41000", got, rejected)
	}
	// Exhausting the entry's range says so, with that range.
	kubeNode.NetworkInventory = `{"interfaces":[],"rdma_ports":[],"engine_port_listeners":[41000]}`
	if got, rejected := c.pickPort(kubeNode); got != 0 || rejected == nil || rejected.Code != scheduler.ReasonEnginePortsExhausted || rejected.Params["start"] != 41000 || rejected.Params["end"] != 41000 {
		t.Fatalf("exhausted: pickPort = %d, %+v", got, rejected)
	}

	// Nodes outside Kubernetes are untouched: the server's range, or the
	// range their agent reports.
	if got, rejected := c.pickPort(plain); rejected != nil || got != 30000 {
		t.Fatalf("plain node: pickPort = %d, %+v; want the server's 30000", got, rejected)
	}
	plain.NetworkInventory = `{"interfaces":[],"rdma_ports":[],"engine_port_range":{"start":50000,"end":50010}}`
	if got, rejected := c.pickPort(plain); rejected != nil || got != 50000 {
		t.Fatalf("plain node with its own range: pickPort = %d, %+v; want 50000", got, rejected)
	}

	// An entry that is gone leaves the node on the range it had before.
	orphan := *kubeNode
	orphan.KubeClusterID = entry.ID + 1000
	orphan.NetworkInventory = ``
	if got, rejected := c.pickPort(&orphan); rejected != nil || got != 30000 {
		t.Fatalf("missing entry: pickPort = %d, %+v; want the server's 30000", got, rejected)
	}
}
