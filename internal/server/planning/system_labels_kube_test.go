package planning

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func kubeStoreWithTwoClusters(t *testing.T) (st *store.Store, prod, lab *store.KubeCluster, nodes []store.Node) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if prod, err = st.CreateKubeCluster(store.KubeCluster{Name: "prod-gpu", Namespace: "gpuworks-system"}); err != nil {
		t.Fatal(err)
	}
	if lab, err = st.CreateKubeCluster(store.KubeCluster{Name: "lab", Namespace: "gpuworks-system"}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		cluster *store.KubeCluster
		uid     string
		node    string
	}{{prod, "uid-prod", "worker-1"}, {prod, "uid-prod", "worker-2"}, {lab, "uid-lab", "worker-1"}} {
		n, _, err := st.EnrollKubeNode(e.cluster.Token, e.uid, e.node)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, *n)
	}
	manual, err := st.RegisterNode("manual-uuid", store.HashToken("t"), "manual-host")
	if err != nil {
		t.Fatal(err)
	}
	nodes = append(nodes, *manual)
	return st, prod, lab, nodes
}

// A node that joined from a cluster names it by UUID (a rename keeps the
// value) with the cluster's name to show, and carries its Kubernetes node
// name; a node added any other way carries neither.
func TestSystemLabelsNameTheKubernetesPlacement(t *testing.T) {
	_, prod, _, nodes := kubeStoreWithTwoClusters(t)
	facts := NodeFacts{KubeClusters: KubeClusters{prod.ID: {UUID: prod.UUID, Name: "prod-gpu"}}}
	labels := SystemLabels(&nodes[0], facts)
	if display, code := displayOf(labels, nodelabel.KeyKubernetesCluster, prod.UUID); display != "prod-gpu" || code != "" {
		t.Fatalf("cluster display = %q/%q in\n%s", display, code, labelled(labels))
	}
	if display, _ := displayOf(labels, nodelabel.KeyKubernetesNode, "worker-1"); display != "worker-1" {
		t.Fatalf("node display = %q in\n%s", display, labelled(labels))
	}

	renamed := NodeFacts{KubeClusters: KubeClusters{prod.ID: {UUID: prod.UUID, Name: "renamed"}}}
	if display, _ := displayOf(SystemLabels(&nodes[0], renamed), nodelabel.KeyKubernetesCluster, prod.UUID); display != "renamed" {
		t.Fatalf("display after a rename = %q, want the new name under the same value", display)
	}

	if got := labelled(SystemLabels(&nodes[3], facts)); strings.Contains(got, "kubernetes") {
		t.Fatalf("a node that did not join from Kubernetes carries\n%s", got)
	}
	// Clusters that were not loaded leave the cluster label off, never wrong;
	// the node name needs nothing loaded.
	if got := labelled(SystemLabels(&nodes[0], NodeFacts{})); strings.Contains(got, nodelabel.KeyKubernetesCluster) || !strings.Contains(got, nodelabel.KeyKubernetesNode+"=worker-1") {
		t.Fatalf("labels without clusters loaded =\n%s", got)
	}
}

// LoadNodeFacts reads the clusters once, for the distinct ids of the nodes,
// and only when a requirement names the cluster label.
func TestLoadNodeFactsLoadsKubeClustersOnlyWhenRequired(t *testing.T) {
	st, prod, lab, nodes := kubeStoreWithTwoClusters(t)
	if facts := LoadNodeFacts(st, nil, nodes, nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}}); facts.KubeClusters != nil {
		t.Fatalf("clusters loaded for a requirement that does not read them: %v", facts.KubeClusters)
	}
	reqs := nodelabel.Requirements{nodelabel.KeyKubernetesCluster: {prod.UUID}}
	facts := LoadNodeFacts(st, nil, nodes, reqs)
	if len(facts.KubeClusters) != 2 || facts.KubeClusters[prod.ID] != (KubeClusterRef{UUID: prod.UUID, Name: "prod-gpu"}) || facts.KubeClusters[lab.ID].Name != "lab" {
		t.Fatalf("clusters = %v", facts.KubeClusters)
	}
	var kept []string
	for i := range nodes {
		if MatchRequirements(&nodes[i], facts, reqs) == nil {
			kept = append(kept, nodes[i].KubeNodeName)
		}
	}
	if strings.Join(kept, ",") != "worker-1,worker-2" || nodes[2].KubeClusterID == prod.ID {
		t.Fatalf("kubernetes-cluster=%s kept %v, want only prod-gpu's two nodes", prod.UUID, kept)
	}
	// Facts that could not be loaded never match.
	if err := MatchRequirements(&nodes[0], NodeFacts{}, reqs); err == nil {
		t.Fatal("a requirement on the cluster matched without the clusters loaded")
	}

	// The node name alone selects across clusters, with nothing loaded.
	nodeReqs := nodelabel.Requirements{nodelabel.KeyKubernetesNode: {"worker-1"}}
	nodeFacts := LoadNodeFacts(st, nil, nodes, nodeReqs)
	if nodeFacts.KubeClusters != nil {
		t.Fatal("the node name label loaded clusters it does not read")
	}
	kept = nil
	for i := range nodes {
		if MatchRequirements(&nodes[i], nodeFacts, nodeReqs) == nil {
			kept = append(kept, nodes[i].UUID)
		}
	}
	if len(kept) != 2 || kept[0] != nodes[0].UUID || kept[1] != nodes[2].UUID {
		t.Fatalf("kubernetes-node=worker-1 kept %v", kept)
	}

	labels := RequirementLabels(reqs, []*store.Node{&nodes[0], &nodes[1], &nodes[2], &nodes[3]}, facts)
	if len(labels) != 1 || labels[0].Display != "prod-gpu" || labels[0].Nodes != 2 {
		t.Fatalf("requirement labels = %+v", labels)
	}
}

func TestLoadKubeClustersSkipsNodesWithoutOne(t *testing.T) {
	st, _, _, nodes := kubeStoreWithTwoClusters(t)
	if got := LoadKubeClusters(st, nodes[3:]); got != nil {
		t.Fatalf("clusters of a manual node = %v", got)
	}
	if got := LoadKubeClusters(st, nil); got != nil {
		t.Fatalf("clusters of no node = %v", got)
	}
}
