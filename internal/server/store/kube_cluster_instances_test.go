package store

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A node of an entry is listed when an instance that is not finished runs on
// it, with its deployments' names; stopped, failed and pending-removal
// instances, nodes of other entries and nodes of none are not.
func TestKubeNodesWithInstances(t *testing.T) {
	st := openKubeStore(t)
	a, err := st.CreateKubeCluster(KubeCluster{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateKubeCluster(KubeCluster{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(c *KubeCluster, uid, node string) *Node {
		t.Helper()
		n, _, err := st.EnrollKubeNode(c.Token, uid, node)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	busy := enroll(a, "uid-a", "busy")
	idle := enroll(a, "uid-a", "idle")
	done := enroll(a, "uid-a", "done")
	other := enroll(b, "uid-b", "elsewhere")
	if _, err := st.RegisterNode("uuid-plain", HashToken("t"), "plain"); err != nil {
		t.Fatal(err)
	}
	plain, err := st.NodeByUUID("uuid-plain")
	if err != nil {
		t.Fatal(err)
	}
	deployment := func(name string) *Deployment {
		t.Helper()
		d, err := st.CreateDeployment(&Deployment{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	chat, embed, old := deployment("chat"), deployment("embed"), deployment("old")
	put := func(d *Deployment, n *Node, uuid, state string) {
		t.Helper()
		if _, err := st.CreateInstance(&Instance{UUID: uuid, DeploymentID: d.ID, NodeID: n.ID, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	put(embed, busy, "i-1", proto.InstRunning)
	put(chat, busy, "i-2", proto.InstStarting)
	put(chat, busy, "i-3", proto.InstPending)
	put(old, done, "i-4", proto.InstStopped)
	put(old, done, "i-5", proto.InstFailed)
	put(old, done, "i-6", proto.InstPendingRemoval)
	put(chat, other, "i-7", proto.InstRunning)
	put(chat, plain, "i-8", proto.InstRunning)
	_ = idle

	got, err := st.KubeNodesWithInstances(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Node.UUID != busy.UUID || got[0].Node.KubeNodeName != "busy" ||
		!reflect.DeepEqual(got[0].Deployments, []string{"chat", "embed"}) {
		t.Fatalf("entry a = %+v", got)
	}
	if got, err := st.KubeNodesWithInstances(b.ID); err != nil || len(got) != 1 || got[0].Node.KubeNodeName != "elsewhere" {
		t.Fatalf("entry b = %+v, %v", got, err)
	}
	if got, err := st.KubeNodesWithInstances(999); err != nil || len(got) != 0 {
		t.Fatalf("no such entry = %+v, %v", got, err)
	}
}
