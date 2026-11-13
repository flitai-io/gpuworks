package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// platformWorld is three accounts and two clusters with the nodes that joined
// through them, beside a node added by hand:
//
//	ali (aliyun)   ecs-1 -> node; ecs-2 is a node of cluster c-1; ecs-3 idle
//	               cluster entry "ack" from c-1 -> two nodes
//	ali-empty      nothing
//	zs (zstack)    host-1, no node
//	pods (runpod)  pod-1 -> node
//	"lab"          a cluster connected on its own -> one node
type platformWorld struct {
	rig                      *tokenRig
	ali, aliEmpty, zs, pods  uint
	ack, lab                 *store.KubeCluster
	ecsNode, podNode, manual string
	ackNodes                 []string
	labNode                  string
}

func newPlatformWorld(t *testing.T) platformWorld {
	t.Helper()
	rig := newTokenRig(t)
	st := rig.st
	w := platformWorld{rig: rig}
	account := func(name, kind string) uint {
		p, err := st.CreateProvider(&store.Provider{Name: name, Kind: kind, CredsCipher: "cipher", State: "valid"})
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	w.ali, w.aliEmpty, w.zs, w.pods = account("ali", "aliyun"), account("ali-empty", "aliyun"), account("zs", "zstack"), account("pods", "runpod")
	instance := func(providerID uint, externalID, kind, cluster string) {
		if err := st.UpsertProviderInstance(&store.ProviderInstance{ProviderID: providerID, ExternalID: externalID, Name: externalID, Kind: kind, ManagedCluster: cluster}); err != nil {
			t.Fatal(err)
		}
	}
	instance(w.ali, "ecs-1", "vm", "")
	instance(w.ali, "ecs-2", "vm", "c-1")
	instance(w.ali, "ecs-3", "vm", "")
	instance(w.zs, "host-1", "", "")
	instance(w.pods, "pod-1", "container", "")
	onInstance := func(providerID uint, externalID, name string) string {
		node, _, err := st.CreateNodePending(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.LinkProviderInstanceNode(providerID, externalID, node.ID); err != nil {
			t.Fatal(err)
		}
		return node.UUID
	}
	w.ecsNode, w.podNode = onInstance(w.ali, "ecs-1", "on-ecs"), onInstance(w.pods, "pod-1", "on-pod")
	var err error
	if w.ack, err = st.CreateKubeCluster(store.KubeCluster{Name: "ack", ProviderID: &w.ali, ProviderClusterID: "c-1"}); err != nil {
		t.Fatal(err)
	}
	if w.lab, err = st.CreateKubeCluster(store.KubeCluster{Name: "lab"}); err != nil {
		t.Fatal(err)
	}
	enroll := func(c *store.KubeCluster, uid, name string) string {
		node, _, err := st.EnrollKubeNode(c.Token, uid, name)
		if err != nil {
			t.Fatal(err)
		}
		return node.UUID
	}
	w.ackNodes = []string{enroll(w.ack, "uid-ack", "worker-1"), enroll(w.ack, "uid-ack", "worker-2")}
	w.labNode = enroll(w.lab, "uid-lab", "worker-1")
	manual, _, err := st.CreateNodePending("by-hand")
	if err != nil {
		t.Fatal(err)
	}
	w.manual = manual.UUID
	return w
}

func (w platformWorld) get(t *testing.T, path string, want int) string {
	t.Helper()
	rec := w.rig.do(w.rig.session, http.MethodGet, path, "")
	if rec.Code != want {
		t.Fatalf("GET %s = %d %s, want %d", path, rec.Code, rec.Body, want)
	}
	return rec.Body.String()
}

func TestPlatformListNamesWhatEachAccountConnectsAndCountsTheNodesThatJoined(t *testing.T) {
	w := newPlatformWorld(t)
	body := w.get(t, "/api/v1/providers", http.StatusOK)
	for name, want := range map[string]string{
		// Two instances of its own (ecs-2 is its cluster's); a node on ecs-1 and the two of the cluster connected from the account.
		"ali":       fmt.Sprintf(`"id":%d,"name":"ali","kind":"aliyun","category":"public_cloud","state":"valid","instance_count":2,"resources":[{"type":"vm","product":"ECS"},{"type":"kubernetes","product":"ACK"}],"node_count":3,`, w.ali),
		"ali-empty": `"name":"ali-empty","kind":"aliyun","category":"public_cloud","state":"valid","instance_count":0,"resources":[{"type":"vm","product":"ECS"},{"type":"kubernetes","product":"ACK"}],"node_count":0,`,
		// One host, which has not joined yet.
		"zs":   `"name":"zs","kind":"zstack","category":"private_cloud","state":"valid","instance_count":1,"resources":[{"type":"host"}],"node_count":0,`,
		"pods": `"name":"pods","kind":"runpod","category":"public_cloud","state":"valid","instance_count":1,"resources":[{"type":"container","product":"Pod"}],"node_count":1,`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s lacks\n%s\nin\n%s", name, want, body)
		}
	}

	// The same machine is an instance of the account and a node of its cluster. Once the
	// two are tied together it is still one node, listed under the cluster.
	member, err := w.rig.st.NodeByUUID(w.ackNodes[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := w.rig.st.LinkProviderInstanceNode(w.ali, "ecs-2", member.ID); err != nil {
		t.Fatal(err)
	}
	if body := w.get(t, "/api/v1/providers", http.StatusOK); !strings.Contains(body, `"name":"ali","kind":"aliyun","category":"public_cloud","state":"valid","instance_count":2,"resources":[{"type":"vm","product":"ECS"},{"type":"kubernetes","product":"ACK"}],"node_count":3,`) {
		t.Fatalf("a cluster node's instance is counted twice: %s", body)
	}
	if page := w.get(t, fmt.Sprintf("/api/v1/nodes?source=provider:%d&page=1", w.ali), http.StatusOK); !strings.Contains(page, `"total":3,`) {
		t.Fatalf("the account's nodes after the tie: %s", page)
	}
	if node := w.get(t, "/api/v1/nodes/"+w.ackNodes[0], http.StatusOK); !strings.Contains(node, fmt.Sprintf(`"source":{"resource":"kubernetes","provider_id":%d,"provider_name":"ali","provider_kind":"aliyun","cluster_uuid":%q,"cluster_name":"ack"}`, w.ali, w.ack.UUID)) {
		t.Fatalf("the tied node is not listed under its cluster: %s", node)
	}
}

// The vendor lists a managed cluster's nodes among the account's instances;
// the account's instance list leaves them out, whether or not their cluster
// is connected, and names no cluster on the instances it keeps.
func TestInstanceListLeavesOutTheNodesOfManagedClusters(t *testing.T) {
	w := newPlatformWorld(t)
	if err := w.rig.st.UpsertProviderInstance(&store.ProviderInstance{ProviderID: w.ali, ExternalID: "ecs-4", Name: "ecs-4", Kind: "vm", ManagedCluster: "c-unconnected"}); err != nil {
		t.Fatal(err)
	}
	list := decodeKube[struct {
		Items []struct {
			ExternalID string `json:"external_id"`
		} `json:"items"`
		Total int `json:"total"`
	}](t, w.rig.do(w.rig.session, http.MethodGet, fmt.Sprintf("/api/v1/providers/%d/instances", w.ali), ""))
	got := []string{}
	for _, it := range list.Items {
		got = append(got, it.ExternalID)
	}
	if strings.Join(got, ",") != "ecs-1,ecs-3" || list.Total != 2 {
		t.Fatalf("instances = %v total=%d, want ecs-1 and ecs-3", got, list.Total)
	}
	if body := w.get(t, fmt.Sprintf("/api/v1/providers/%d/instances", w.ali), http.StatusOK); strings.Contains(body, "managed_cluster") {
		t.Fatalf("an instance names a cluster: %s", body)
	}
	// An account with nothing but cluster nodes lists nothing, as an empty array.
	if err := w.rig.st.UpsertProviderInstance(&store.ProviderInstance{ProviderID: w.aliEmpty, ExternalID: "ecs-9", Name: "ecs-9", Kind: "vm", ManagedCluster: "c-9"}); err != nil {
		t.Fatal(err)
	}
	if body := w.get(t, fmt.Sprintf("/api/v1/providers/%d/instances", w.aliEmpty), http.StatusOK); strings.TrimSpace(body) != `{"items":[],"total":0}` {
		t.Fatalf("an account of cluster nodes only = %s", body)
	}
	if body := w.get(t, "/api/v1/providers", http.StatusOK); !strings.Contains(body, `"name":"ali-empty","kind":"aliyun","category":"public_cloud","state":"valid","instance_count":0,`) {
		t.Fatalf("cluster nodes are counted as instances: %s", body)
	}
}

func TestNodesSayWhichPlatformTheyJoinedThrough(t *testing.T) {
	w := newPlatformWorld(t)
	nodes := decodeKube[struct {
		Items []struct {
			UUID   string         `json:"uuid"`
			Source map[string]any `json:"source"`
		} `json:"items"`
	}](t, w.rig.do(w.rig.session, http.MethodGet, "/api/v1/nodes", ""))
	got := map[string]string{}
	for _, n := range nodes.Items {
		got[n.UUID] = fmt.Sprint(n.Source)
	}
	ali := fmt.Sprint(float64(w.ali))
	want := map[string]string{
		w.ecsNode:     fmt.Sprintf("map[provider_id:%s provider_kind:aliyun provider_name:ali resource:vm]", ali),
		w.podNode:     fmt.Sprintf("map[provider_id:%v provider_kind:runpod provider_name:pods resource:container]", float64(w.pods)),
		w.ackNodes[0]: fmt.Sprintf("map[cluster_name:ack cluster_uuid:%s provider_id:%s provider_kind:aliyun provider_name:ali resource:kubernetes]", w.ack.UUID, ali),
		w.ackNodes[1]: fmt.Sprintf("map[cluster_name:ack cluster_uuid:%s provider_id:%s provider_kind:aliyun provider_name:ali resource:kubernetes]", w.ack.UUID, ali),
		// A cluster connected on its own names no account.
		w.labNode: fmt.Sprintf("map[cluster_name:lab cluster_uuid:%s resource:kubernetes]", w.lab.UUID),
		// A node added by hand has no source at all.
		w.manual: "map[]",
	}
	for uuid, source := range want {
		if got[uuid] != source {
			t.Errorf("node %s source = %s, want %s", uuid, got[uuid], source)
		}
	}
	if raw := w.get(t, "/api/v1/nodes/"+w.manual, http.StatusOK); strings.Contains(raw, `"source"`) {
		t.Fatalf("a node added by hand carries a source: %s", raw)
	}
	// On the wire the fields keep this order and omit what does not apply.
	if raw := w.get(t, "/api/v1/nodes", http.StatusOK); !strings.Contains(raw, fmt.Sprintf(`"source":{"resource":"kubernetes","cluster_uuid":%q,"cluster_name":"lab"}`, w.lab.UUID)) ||
		!strings.Contains(raw, fmt.Sprintf(`"source":{"resource":"vm","provider_id":%d,"provider_name":"ali","provider_kind":"aliyun"}`, w.ali)) {
		t.Fatalf("source wire form: %s", raw)
	}

	// Deleting the account leaves its cluster's nodes with the cluster alone, and its instance's node by hand.
	if err := w.rig.st.DeleteProvider(w.ali); err != nil {
		t.Fatal(err)
	}
	raw := w.get(t, "/api/v1/nodes", http.StatusOK)
	if !strings.Contains(raw, fmt.Sprintf(`"source":{"resource":"kubernetes","cluster_uuid":%q,"cluster_name":"ack"}`, w.ack.UUID)) || strings.Contains(raw, `"provider_name":"ali"`) {
		t.Fatalf("sources after the account is gone: %s", raw)
	}
}

func TestNodeListKeepsTheNodesOfTheSourcesAskedFor(t *testing.T) {
	w := newPlatformWorld(t)
	uuids := func(path string) ([]string, int, int) {
		page := decodeKube[struct {
			Items []struct {
				UUID string `json:"uuid"`
			} `json:"items"`
			Total int `json:"total"`
			All   int `json:"all"`
		}](t, w.rig.do(w.rig.session, http.MethodGet, path, ""))
		var out []string
		for _, n := range page.Items {
			out = append(out, n.UUID)
		}
		sort.Strings(out)
		return out, page.Total, page.All
	}
	sorted := func(in ...string) []string { sort.Strings(in); return in }
	same := func(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }
	provider := func(id uint) string { return fmt.Sprintf("provider:%d", id) }

	// An account: the nodes on its instances and the nodes of its clusters.
	got, total, all := uuids("/api/v1/nodes?page=1&source=" + provider(w.ali))
	if want := sorted(w.ecsNode, w.ackNodes[0], w.ackNodes[1]); !same(got, want) || total != 3 || all != 6 {
		t.Fatalf("ali's nodes = %v (total %d of %d), want %v (3 of 6)", got, total, all, want)
	}
	if got, _, _ := uuids("/api/v1/nodes?source=" + provider(w.pods)); !same(got, []string{w.podNode}) {
		t.Fatalf("pods' nodes without paging = %v", got)
	}
	// One cluster, whether it was connected from an account or on its own.
	if got, total, _ := uuids("/api/v1/nodes?page=1&source=cluster:" + w.ack.UUID); !same(got, sorted(w.ackNodes[0], w.ackNodes[1])) || total != 2 {
		t.Fatalf("ack's nodes = %v (total %d)", got, total)
	}
	if got, _, _ := uuids("/api/v1/nodes?page=1&source=cluster:" + w.lab.UUID); !same(got, []string{w.labNode}) {
		t.Fatalf("lab's nodes = %v", got)
	}
	// The nodes added by hand.
	if got, _, _ := uuids("/api/v1/nodes?page=1&source=manual"); !same(got, []string{w.manual}) {
		t.Fatalf("nodes added by hand = %v", got)
	}
	// Several values are alternatives, as a column filter with several boxes ticked asks.
	if got, total, _ := uuids("/api/v1/nodes?page=1&source=manual," + provider(w.pods) + ",cluster:" + w.lab.UUID); !same(got, sorted(w.manual, w.podNode, w.labNode)) || total != 3 {
		t.Fatalf("by hand, pods' or lab's = %v (total %d)", got, total)
	}
	// It combines with the other filters.
	if got, total, _ := uuids("/api/v1/nodes?page=1&q=on-ecs&source=" + provider(w.ali)); !same(got, []string{w.ecsNode}) || total != 1 {
		t.Fatalf("ali's nodes named on-ecs = %v (total %d)", got, total)
	}
	// An account with no node, and an account or cluster that does not exist, match nothing rather than everything.
	for _, source := range []string{provider(w.aliEmpty), provider(w.zs), "provider:99999", "cluster:no-such-cluster"} {
		if got, total, all := uuids("/api/v1/nodes?page=1&source=" + source); len(got) != 0 || total != 0 || all != 6 {
			t.Fatalf("source %s = %v (total %d of %d), want none of 6", source, got, total, all)
		}
	}
	// A value of no known form is refused, alone or among good ones.
	for _, bad := range []string{"", "abc", "provider:", "provider:0", "provider:-1", "provider:x", "cluster:", "manual,nope", "Manual"} {
		body := w.get(t, "/api/v1/nodes?page=1&source="+bad, http.StatusBadRequest)
		if !strings.Contains(body, ErrorListFilterInvalid) {
			t.Fatalf("source=%q = %s", bad, body)
		}
	}
	if got, _, _ := uuids("/api/v1/nodes?page=1"); len(got) != 6 {
		t.Fatalf("without the filter = %v", got)
	}
}

func TestNodePageOffersEverySourceWithItsNodeCount(t *testing.T) {
	w := newPlatformWorld(t)
	// Accounts by name, then clusters by name, then the nodes added by hand. An account's count takes in
	// its clusters' nodes; accounts with no node (ali-empty, zs) are not offered.
	want := fmt.Sprintf(`"sources":[`+
		`{"value":"provider:%d","provider_name":"ali","provider_kind":"aliyun","nodes":3},`+
		`{"value":"provider:%d","provider_name":"pods","provider_kind":"runpod","nodes":1},`+
		`{"value":"cluster:%s","provider_name":"ali","provider_kind":"aliyun","cluster_name":"ack","nodes":2},`+
		`{"value":"cluster:%s","cluster_name":"lab","nodes":1},`+
		`{"value":"manual","nodes":1}]`, w.ali, w.pods, w.ack.UUID, w.lab.UUID)
	if body := w.get(t, "/api/v1/nodes?page=1", http.StatusOK); !strings.Contains(body, want) {
		t.Fatalf("sources lack\n%s\nin\n%s", want, body)
	}
	// The choices are the whole fleet's, whatever the list is filtered to.
	if body := w.get(t, "/api/v1/nodes?page=1&q=on-pod&source=manual", http.StatusOK); !strings.Contains(body, want) || !strings.Contains(body, `"total":0,`) {
		t.Fatalf("sources of a filtered page: %s", body)
	}
	// An unpaged list carries no choices, like the labels.
	if body := w.get(t, "/api/v1/nodes", http.StatusOK); strings.Contains(body, `"sources"`) {
		t.Fatalf("an unpaged list carries sources: %s", body)
	}
}

func TestNodePageOfAnEmptyFleetOffersNoSource(t *testing.T) {
	rig := newTokenRig(t)
	// An empty list, not null: a client maps over it.
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/nodes?page=1", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sources":[]`) {
		t.Fatalf("an empty fleet's sources = %d %s", rec.Code, rec.Body)
	}
}
