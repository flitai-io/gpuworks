package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type kubeLabelWorld struct {
	rig       *tokenRig
	prod, lab *store.KubeCluster
	// enrolled maps "<cluster name>/<kube node name>" to the node's UUID.
	enrolled map[string]string
	manual   string
}

// kubeLabelFixture joins two nodes from prod-gpu and one from lab (both have
// a worker-1), beside a node that was added by hand.
func kubeLabelFixture(t *testing.T) kubeLabelWorld {
	t.Helper()
	rig := newTokenRig(t)
	world := kubeLabelWorld{rig: rig, enrolled: map[string]string{}}
	var err error
	if world.prod, err = rig.st.CreateKubeCluster(store.KubeCluster{Name: "prod-gpu"}); err != nil {
		t.Fatal(err)
	}
	if world.lab, err = rig.st.CreateKubeCluster(store.KubeCluster{Name: "lab"}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		cluster *store.KubeCluster
		uid     string
		node    string
	}{{world.prod, "uid-prod", "worker-1"}, {world.prod, "uid-prod", "worker-2"}, {world.lab, "uid-lab", "worker-1"}} {
		node, _, err := rig.st.EnrollKubeNode(e.cluster.Token, e.uid, e.node)
		if err != nil {
			t.Fatal(err)
		}
		world.enrolled[e.cluster.Name+"/"+e.node] = node.UUID
	}
	manual, _, err := rig.st.CreateNodePending("by-hand")
	if err != nil {
		t.Fatal(err)
	}
	world.manual = manual.UUID
	return world
}

func (w kubeLabelWorld) uuids(names ...string) []string {
	var out []string
	for _, name := range names {
		out = append(out, w.enrolled[name])
	}
	sort.Strings(out)
	return out
}

func (w kubeLabelWorld) listed(t *testing.T, query string) []string {
	t.Helper()
	rec := w.rig.do(w.rig.session, "GET", "/api/v1/nodes?page=1&page_size=100&"+query, "")
	if rec.Code != 200 {
		t.Fatalf("%s = %d %s", query, rec.Code, rec.Body)
	}
	var page struct {
		Items []dto.Node `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, item := range page.Items {
		out = append(out, item.UUID)
	}
	sort.Strings(out)
	return out
}

func labelQuery(key, value string) string {
	return "label=" + url.QueryEscape(key+"="+value)
}

// A node that joined from a cluster carries the cluster (by UUID, shown by
// name) and its Kubernetes node name as system labels, on the list row and
// on the detail alike, as literal wire JSON; the node list filters by them
// and the facets offer them with the server's display. The retired `kube`
// object is gone from the node.
func TestKubeNodesCarryTheirPlacementAsSystemLabels(t *testing.T) {
	w := kubeLabelFixture(t)
	prodNode := w.enrolled["prod-gpu/worker-1"]

	wantClusterLabel := fmt.Sprintf(`{"key":"gpuworks.ai/kubernetes-cluster","value":%q,"display":"prod-gpu"}`, w.prod.UUID)
	wantNodeLabel := `{"key":"gpuworks.ai/kubernetes-node","value":"worker-1","display":"worker-1"}`
	for _, path := range []string{"/api/v1/nodes?page=1&page_size=100", "/api/v1/nodes/" + prodNode, "/api/v1/nodes"} {
		rec := w.rig.do(w.rig.session, "GET", path, "")
		body := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(body, wantClusterLabel) || !strings.Contains(body, wantNodeLabel) {
			t.Fatalf("GET %s = %d, lacks the placement labels\n%s", path, rec.Code, body)
		}
		if strings.Contains(body, `"kube":`) {
			t.Fatalf("GET %s still carries the kube object: %s", path, body)
		}
	}
	// The label on a node that did not join from a cluster is absent.
	rec := w.rig.do(w.rig.session, "GET", "/api/v1/nodes/"+w.manual, "")
	if strings.Contains(rec.Body.String(), "kubernetes") {
		t.Fatalf("a hand-added node carries Kubernetes labels: %s", rec.Body)
	}

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"cluster by UUID", labelQuery(nodelabel.KeyKubernetesCluster, w.prod.UUID), w.uuids("prod-gpu/worker-1", "prod-gpu/worker-2")},
		{"other cluster", labelQuery(nodelabel.KeyKubernetesCluster, w.lab.UUID), w.uuids("lab/worker-1")},
		{"either cluster", labelQuery(nodelabel.KeyKubernetesCluster, w.prod.UUID) + "&" + labelQuery(nodelabel.KeyKubernetesCluster, w.lab.UUID), w.uuids("prod-gpu/worker-1", "prod-gpu/worker-2", "lab/worker-1")},
		{"node name across clusters", labelQuery(nodelabel.KeyKubernetesNode, "worker-1"), w.uuids("prod-gpu/worker-1", "lab/worker-1")},
		{"cluster and node name", labelQuery(nodelabel.KeyKubernetesCluster, w.prod.UUID) + "&" + labelQuery(nodelabel.KeyKubernetesNode, "worker-1"), w.uuids("prod-gpu/worker-1")},
		{"an unknown cluster", labelQuery(nodelabel.KeyKubernetesCluster, "no-such-cluster"), []string{}},
	} {
		if got := w.listed(t, tc.query); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %s matched %v, want %v", tc.name, tc.query, got, tc.want)
		}
	}

	rec = w.rig.do(w.rig.session, "GET", "/api/v1/nodes/facets", "")
	if rec.Code != 200 {
		t.Fatalf("facets = %d %s", rec.Code, rec.Body)
	}
	var facets dto.NodeFacets
	if err := json.Unmarshal(rec.Body.Bytes(), &facets); err != nil {
		t.Fatal(err)
	}
	system := map[string][]dto.SystemLabelValueCount{}
	for _, key := range facets.SystemLabels {
		system[key.Key] = key.Values
	}
	wantClusters := []dto.SystemLabelValueCount{{Value: w.lab.UUID, Display: "lab", Nodes: 1}, {Value: w.prod.UUID, Display: "prod-gpu", Nodes: 2}}
	sort.Slice(wantClusters, func(i, j int) bool { return wantClusters[i].Value < wantClusters[j].Value })
	if got := system[nodelabel.KeyKubernetesCluster]; !reflect.DeepEqual(got, wantClusters) {
		t.Errorf("cluster facets = %+v, want %+v", got, wantClusters)
	}
	wantNames := []dto.SystemLabelValueCount{{Value: "worker-1", Display: "worker-1", Nodes: 2}, {Value: "worker-2", Display: "worker-2", Nodes: 1}}
	if got := system[nodelabel.KeyKubernetesNode]; !reflect.DeepEqual(got, wantNames) {
		t.Errorf("node facets = %+v, want %+v", got, wantNames)
	}
}

// A deployment's node requirements name the cluster's UUID: evaluation keeps
// only that cluster's nodes, and the edit response shows the value by the
// cluster's name with the number of nodes carrying it.
func TestEvaluateEligibleNodesMatchTheKubernetesCluster(t *testing.T) {
	h, templateID := eligibleNodeFixture(t)
	st := h.d.Store
	prod, err := st.CreateKubeCluster(store.KubeCluster{Name: "prod-gpu"})
	if err != nil {
		t.Fatal(err)
	}
	lab, err := st.CreateKubeCluster(store.KubeCluster{Name: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(c *store.KubeCluster, uid, name string) string {
		t.Helper()
		node, _, err := st.EnrollKubeNode(c.Token, uid, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: name, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
			GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
		}); err != nil {
			t.Fatal(err)
		}
		return node.UUID
	}
	inProd := enroll(prod, "uid-prod", "worker-1")
	inLab := enroll(lab, "uid-lab", "worker-1")

	body := fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","placement_mode":"auto","node_requirements":{"gpuworks.ai/kubernetes-cluster":[%q]}}`, templateID, prod.UUID)
	nodes := eligibleNodesOf(t, h, body)
	for uuid, entry := range nodes {
		want := uuid == inProd
		if unmet := hasReason(entry, deployment.ReasonRequirementsUnmet); unmet == want {
			t.Errorf("%s: requirement unmet = %v, want %v (%+v)", uuid, unmet, !want, entry)
		}
	}
	if _, ok := nodes[inLab]; !ok || !hasReason(nodes[inLab], deployment.ReasonRequirementsUnmet) {
		t.Errorf("the node of the other cluster = %+v, want the requirement unmet", nodes[inLab])
	}
	if !nodes[inProd].Eligible {
		t.Errorf("the cluster's node = %+v, want eligible", nodes[inProd])
	}

	// The Kubernetes node name alone selects across clusters.
	body = fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","placement_mode":"auto","node_requirements":{"gpuworks.ai/kubernetes-node":["worker-1"]}}`, templateID)
	for uuid, entry := range eligibleNodesOf(t, h, body) {
		want := uuid == inProd || uuid == inLab
		if unmet := hasReason(entry, deployment.ReasonRequirementsUnmet); unmet == want {
			t.Errorf("by node name %s: requirement unmet = %v, want %v", uuid, unmet, !want)
		}
	}
}
