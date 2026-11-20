package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestManagementNetworkIsListedReadOnly(t *testing.T) {
	n := newNetworkAPI(t)
	for _, uuid := range []string{"mgmt-a", "mgmt-b", "old-agent"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	for uuid, ip := range map[string]string{"mgmt-a": "172.25.0.11", "mgmt-b": "172.25.0.12"} {
		if err := n.store.UpdateNodeStatus(uuid, proto.NodeStatus{Network: testManagement(ip)}); err != nil {
			t.Fatal(err)
		}
	}
	// A dedicated storage network on mgmt-a takes that role off its
	// management network; mgmt-b switched compute traffic off.
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"store","roles":["storage_sync"],"type":"host_ethernet","cidr":"172.25.0.0/24"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"node_uuid":"mgmt-a"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPut, "/api/v1/nodes/mgmt-b/management-roles", `{"roles_off":["compute"]}`, http.StatusOK)

	items := n.expect(n.member, http.MethodGet, "/api/v1/networks", "", http.StatusOK)["items"].([]any)
	mgmt := items[0].(map[string]any)
	if mgmt["builtin"] != true || mgmt["resolved_node_count"] != float64(2) || !reflect.DeepEqual(mgmt["attachments"], []any{}) {
		t.Fatalf("listed management network = %v", mgmt)
	}
	if user := items[1].(map[string]any); user["builtin"] != false {
		t.Fatalf("a user network is marked builtin: %v", user)
	}

	detail := n.expect(n.member, http.MethodGet, "/api/v1/networks/0", "", http.StatusOK)
	if detail["test_supported"] != true || detail["name"] != "management" || detail["latest_test"] != nil {
		t.Fatalf("management detail = %v", detail)
	}
	want := []any{
		map[string]any{
			"node_uuid": "mgmt-a", "node_name": "mgmt-a", "labels": map[string]any{}, "roles": []any{"compute", "serving"}, "attachment_id": float64(0),
			"source": "management", "interface": "eth0", "ip": "172.25.0.11", "hcas": []any{}, "tools": []any{}, "warnings": []any{},
		},
		map[string]any{
			"node_uuid": "mgmt-b", "node_name": "mgmt-b", "labels": map[string]any{}, "roles": []any{"storage_sync", "serving"}, "attachment_id": float64(0),
			"source": "management", "interface": "eth0", "ip": "172.25.0.12", "hcas": []any{}, "tools": []any{}, "warnings": []any{},
			"roles_off": []any{"compute"},
		},
	}
	if got := detail["nodes"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("management nodes = %v\nwant %v", got, want)
	}

	ids := n.expect(n.member, http.MethodGet, "/api/v1/networks/0/search?kind=nodes&q=mgmt-B", "", http.StatusOK)["ids"]
	if !reflect.DeepEqual(ids, []any{"mgmt-b"}) {
		t.Fatalf("management node search = %v", ids)
	}

	// It cannot be edited, deleted or attached.
	n.expectCode(http.MethodPut, "/api/v1/networks/0", `{"name":"x","roles":["serving"],"type":"host_ethernet"}`, http.StatusNotFound, ErrorNetworkNotFound)
	n.expectCode(http.MethodDelete, "/api/v1/networks/0", "", http.StatusNotFound, ErrorNetworkNotFound)
	n.expectCode(http.MethodPost, "/api/v1/networks/0/attachments", `{"node_uuid":"mgmt-a"}`, http.StatusNotFound, ErrorNetworkNotFound)
}

func TestManagementNetworkCanBeTested(t *testing.T) {
	n := newNetworkAPI(t)
	for _, uuid := range []string{"mgmt-a", "mgmt-b", "old-agent"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	for uuid, ip := range map[string]string{"mgmt-a": "172.25.0.11", "mgmt-b": "172.25.0.12"} {
		if err := n.store.UpdateNodeStatus(uuid, proto.NodeStatus{Network: testManagement(ip)}); err != nil {
			t.Fatal(err)
		}
	}
	// A dedicated network carrying every role on mgmt-a does not take it off
	// the management network for testing: the test probes the address itself.
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"all","roles":["storage_sync","compute","serving"],"type":"host_ethernet","cidr":"172.25.0.0/24"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"node_uuid":"mgmt-a"}`, http.StatusCreated)
	const path = "/api/v1/networks/0/tests"

	n.expectCode(http.MethodPost, path, `{"scope":"all","kind":"rdma_tool"}`, http.StatusBadRequest, ErrorNetworkTestKindInvalid)
	n.expectCode(http.MethodPost, path, `{"scope":"all","kind":"nccl_image"}`, http.StatusBadRequest, ErrorNetworkTestKindInvalid)
	n.expectCode(http.MethodPost, path, `{"scope":"nodes","node_uuids":["mgmt-a","old-agent"]}`, http.StatusBadRequest, ErrorNetworkTestNodeInvalid)
	n.expectCode(http.MethodPost, path, `{"scope":"nodes","node_uuids":["mgmt-a"]}`, http.StatusBadRequest, ErrorNetworkTestNodeCount)
	n.expectCode(http.MethodPost, path, `{}`, http.StatusBadRequest, ErrorNetworkTestScopeInvalid)

	run := n.expect(n.admin, http.MethodPost, path, `{"scope":"all"}`, http.StatusAccepted)
	if run["kind"] != "tcp" || run["status"] != "running" || run["node_count"] != float64(2) || run["scope"] != "all" {
		t.Fatalf("run = %v", run)
	}
	n.expectCode(http.MethodPost, path, `{"scope":"all"}`, http.StatusConflict, ErrorNetworkTestRunning)
	latest := n.expect(n.member, http.MethodGet, "/api/v1/networks/0", "", http.StatusOK)["latest_test"].(map[string]any)
	if latest["id"] != run["id"] || latest["status"] != "running" {
		t.Fatalf("latest test = %v", latest)
	}
	// Another network's run is its own.
	if other := n.expect(n.member, http.MethodGet, "/api/v1/networks/1", "", http.StatusOK)["latest_test"]; other != nil {
		t.Fatalf("network 1 latest_test = %v", other)
	}
	if out := n.expect(n.member, http.MethodPost, path, `{"scope":"all"}`, http.StatusForbidden); out["code"] != ErrorAdministratorRequired {
		t.Fatalf("member code = %v", out["code"])
	}
}

func TestNodeManagementRoles(t *testing.T) {
	n := newNetworkAPI(t)
	for _, uuid := range []string{"mgmt-a", "old-agent"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.store.UpdateNodeStatus("mgmt-a", proto.NodeStatus{Network: testManagement("172.25.0.11")}); err != nil {
		t.Fatal(err)
	}
	if err := n.store.SetNodeAgentInfo("mgmt-a", "dev", "203.0.113.7:51234"); err != nil {
		t.Fatal(err)
	}

	view := n.expect(n.member, http.MethodGet, "/api/v1/nodes/mgmt-a/networks", "", http.StatusOK)
	want := map[string]any{"interface": "eth0", "ip": "172.25.0.11", "roles_off": []any{}, "remote_addr": "203.0.113.7", "behind_nat": true}
	if !reflect.DeepEqual(view["management"], want) {
		t.Fatalf("management = %v\nwant %v", view["management"], want)
	}
	if roles := loadedRoles(view); !reflect.DeepEqual(roles, []string{"compute", "serving", "storage_sync"}) {
		t.Fatalf("loaded roles = %v", roles)
	}
	// Serving over the management network rides the tunnel: no direct forward.
	if view["serving_forward"] != nil {
		t.Fatalf("management serving has a forward address: %v", view["serving_forward"])
	}

	view = n.expect(n.admin, http.MethodPut, "/api/v1/nodes/mgmt-a/management-roles", `{"roles_off":["serving","storage_sync","serving"]}`, http.StatusOK)
	// Stored once each, in the roles' display order.
	if got := view["management"].(map[string]any)["roles_off"]; !reflect.DeepEqual(got, []any{"storage_sync", "serving"}) {
		t.Fatalf("roles_off = %v", got)
	}
	if roles := loadedRoles(view); !reflect.DeepEqual(roles, []string{"compute"}) {
		t.Fatalf("loaded roles after switching off = %v", roles)
	}
	view = n.expect(n.admin, http.MethodPut, "/api/v1/nodes/mgmt-a/management-roles", `{"roles_off":[]}`, http.StatusOK)
	if len(loadedRoles(view)) != 3 {
		t.Fatalf("roles not restored: %v", view["networks"])
	}

	// An agent that has not reported its address has no management address
	// yet, but its roles can still be switched off ahead of time.
	view = n.expect(n.admin, http.MethodPut, "/api/v1/nodes/old-agent/management-roles", `{"roles_off":["compute"]}`, http.StatusOK)
	if want := map[string]any{"interface": "", "ip": "", "roles_off": []any{"compute"}, "behind_nat": false}; !reflect.DeepEqual(view["management"], want) {
		t.Fatalf("old agent management = %v", view["management"])
	}

	n.expectCode(http.MethodPut, "/api/v1/nodes/mgmt-a/management-roles", `{"roles_off":["management"]}`, http.StatusBadRequest, ErrorNetworkRoleInvalid)
	n.expect(n.admin, http.MethodPut, "/api/v1/nodes/missing/management-roles", `{"roles_off":[]}`, http.StatusNotFound)
	n.expect(n.admin, http.MethodPut, "/api/v1/nodes/mgmt-a/management-roles", `{`, http.StatusBadRequest)
	if out := n.expect(n.member, http.MethodPut, "/api/v1/nodes/mgmt-a/management-roles", `{"roles_off":[]}`, http.StatusForbidden); out["code"] != ErrorAdministratorRequired {
		t.Fatalf("member code = %v", out["code"])
	}

	// A connection relayed on the server host says nothing about NAT.
	if err := n.store.SetNodeAgentInfo("mgmt-a", "dev", "127.0.0.1:40000"); err != nil {
		t.Fatal(err)
	}
	view = n.expect(n.member, http.MethodGet, "/api/v1/nodes/mgmt-a/networks", "", http.StatusOK)
	if mgmt := view["management"].(map[string]any); mgmt["behind_nat"] != false {
		t.Fatalf("loopback remote reported as NAT: %v", mgmt)
	}
}

func loadedRoles(view map[string]any) []string {
	out := []string{}
	for _, raw := range view["networks"].([]any) {
		n := raw.(map[string]any)
		if n["source"] == "management" {
			out = append(out, n["role"].(string))
		}
	}
	sort.Strings(out)
	return out
}

func TestNodeNetworksCarryTheLatestLinkRates(t *testing.T) {
	n := newNetworkAPI(t)
	if _, err := n.store.RegisterNode("rates", store.HashToken("rates"), "rates"); err != nil {
		t.Fatal(err)
	}
	inv := proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "bond0", State: "up"}, {Name: "eth9", State: "down"}},
		RDMAPorts:  []proto.RDMAPort{{Device: "mlx5_0", Port: 1, State: "ACTIVE"}, {Device: "mlx5_1", Port: 1, State: "DOWN"}},
	}
	if err := n.store.UpdateNodeStatus("rates", proto.NodeStatus{Network: &inv}); err != nil {
		t.Fatal(err)
	}
	// The metrics store answers only for the node and window asked about,
	// the way tsdb.QueryLatest filters by matchers and since.
	points := map[string][]MetricLatest{
		"gpuworks_node_nic_rx_bytes_per_sec":  {{Labels: map[string]string{"node": "rates", "iface": "bond0"}, Value: 1.5e9}, {Labels: map[string]string{"node": "other", "iface": "eth9"}, Value: 7}},
		"gpuworks_node_nic_tx_bytes_per_sec":  {{Labels: map[string]string{"node": "rates", "iface": "bond0"}, Value: 2e8}},
		"gpuworks_node_rdma_rx_bytes_per_sec": {{Labels: map[string]string{"node": "rates", "hca": "mlx5_0:1"}, Value: 4.9e10}},
		"gpuworks_node_rdma_tx_bytes_per_sec": {{Labels: map[string]string{"node": "rates", "hca": "mlx5_0:1"}, Value: 0}},
	}
	var sinces []int64
	n.router = Router(Deps{Store: n.store, AuthSigner: n.signer, MetricsLatest: func(metric string, matchers map[string]string, sinceMS int64, _ ...int64) ([]MetricLatest, error) {
		sinces = append(sinces, sinceMS)
		var out []MetricLatest
		for _, p := range points[metric] {
			if p.Labels["node"] == matchers["node"] {
				out = append(out, p)
			}
		}
		return out, nil
	}})
	view := n.expect(n.member, http.MethodGet, "/api/v1/nodes/rates/networks", "", http.StatusOK)
	ifaces := view["interfaces"].([]any)
	bond, down := ifaces[0].(map[string]any), ifaces[1].(map[string]any)
	if bond["rx_bytes_per_sec"] != 1.5e9 || bond["tx_bytes_per_sec"] != 2e8 {
		t.Fatalf("bond0 = %v", bond)
	}
	if _, ok := down["rx_bytes_per_sec"]; ok {
		t.Fatalf("a NIC with no recent sample has no rate (not another node's): %v", down)
	}
	ports := view["rdma_ports"].([]any)
	active, idle := ports[0].(map[string]any), ports[1].(map[string]any)
	if active["rx_bytes_per_sec"] != 4.9e10 || active["tx_bytes_per_sec"] != float64(0) {
		t.Fatalf("mlx5_0:1 = %v", active)
	}
	if _, ok := idle["tx_bytes_per_sec"]; ok {
		t.Fatalf("a down port has no rate: %v", idle)
	}
	if len(sinces) != 4 || time.Since(time.UnixMilli(sinces[0])) < 110*time.Second || time.Since(time.UnixMilli(sinces[0])) > 130*time.Second {
		t.Fatalf("rates must be from the last two minutes: %v", sinces)
	}
}

func TestNodeNetworksWithoutAMetricsStoreHaveNoRates(t *testing.T) {
	n := newNetworkAPI(t)
	if _, err := n.store.RegisterNode("plain", store.HashToken("plain"), "plain"); err != nil {
		t.Fatal(err)
	}
	inv := proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth0"}}}
	if err := n.store.UpdateNodeStatus("plain", proto.NodeStatus{Network: &inv}); err != nil {
		t.Fatal(err)
	}
	iface := n.expect(n.member, http.MethodGet, "/api/v1/nodes/plain/networks", "", http.StatusOK)["interfaces"].([]any)[0].(map[string]any)
	if _, ok := iface["rx_bytes_per_sec"]; ok {
		t.Fatalf("no metrics store, no rate: %v", iface)
	}
	// A failing store only leaves the rates out.
	n.router = Router(Deps{Store: n.store, AuthSigner: n.signer, MetricsLatest: func(string, map[string]string, int64, ...int64) ([]MetricLatest, error) {
		return nil, errors.New("database is locked")
	}})
	n.expect(n.member, http.MethodGet, "/api/v1/nodes/plain/networks", "", http.StatusOK)
}

// The node view lists the ports its accelerator cards own, with the exact
// wire keys the UI reads; a node without such cards gets an empty list.
func TestNodeNetworksListAcceleratorPorts(t *testing.T) {
	n := newNetworkAPI(t)
	for _, uuid := range []string{"npu", "plain"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	var reported proto.NodeStatus
	wire := `{"network":{"interfaces":[],"rdma_ports":[],"accelerator_ports":[` +
		`{"name":"npu3","index":3,"ip":"203.0.113.59","netmask":"255.255.0.0","state":"UP","speed_gbps":200},` +
		`{"name":"npu6","index":6,"state":"DOWN"}]}}`
	if err := json.Unmarshal([]byte(wire), &reported); err != nil {
		t.Fatal(err)
	}
	if err := n.store.UpdateNodeStatus("npu", reported); err != nil {
		t.Fatal(err)
	}
	if err := n.store.UpdateNodeStatus("plain", proto.NodeStatus{Network: &proto.NetInventory{}}); err != nil {
		t.Fatal(err)
	}
	raw := func(uuid string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+uuid+"/networks", nil)
		req.Header.Set("Authorization", "Bearer "+n.member)
		rec := httptest.NewRecorder()
		n.router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	want := `"accelerator_ports":[` +
		`{"name":"npu3","index":3,"ip":"203.0.113.59","netmask":"255.255.0.0","state":"UP","speed_gbps":200},` +
		`{"name":"npu6","index":6,"ip":"","netmask":"","state":"DOWN","speed_gbps":0,"gap":"inactive"}]`
	if code, body := raw("npu"); code != http.StatusOK || !strings.Contains(body, want) {
		t.Fatalf("status %d, body %s\nwant it to contain %s", code, body, want)
	}
	if code, body := raw("plain"); code != http.StatusOK || !strings.Contains(body, `"accelerator_ports":[]`) {
		t.Fatalf("status %d, body %s", code, body)
	}
}

func TestBatchManagementRoles(t *testing.T) {
	n := newNetworkAPI(t)
	for _, uuid := range []string{"b-1", "b-2", "b-3"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.store.SetNodeManagementRolesOff("b-2", []string{netplan.RoleServing}); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v1/nodes/management-roles"
	out := n.expect(n.admin, http.MethodPatch, path, `{"node_uuids":["b-1","b-2"],"allow":["serving"],"deny":["compute","storage_sync"]}`, http.StatusOK)
	if !reflect.DeepEqual(out, map[string]any{"updated": float64(2)}) {
		t.Fatalf("result = %v", out)
	}
	for uuid, want := range map[string][]any{"b-1": {"storage_sync", "compute"}, "b-2": {"storage_sync", "compute"}, "b-3": {}} {
		view := n.expect(n.member, http.MethodGet, "/api/v1/nodes/"+uuid+"/networks", "", http.StatusOK)
		if got := view["management"].(map[string]any)["roles_off"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s roles_off = %v, want %v", uuid, got, want)
		}
	}
	// Nothing to change is not an error.
	if out := n.expect(n.admin, http.MethodPatch, path, `{"node_uuids":["b-1"],"deny":["compute"]}`, http.StatusOK); out["updated"] != float64(0) {
		t.Fatalf("no-op result = %v", out)
	}

	n.expectCode(http.MethodPatch, path, `{"node_uuids":[],"allow":["serving"]}`, http.StatusBadRequest, ErrorManagementRolesNodesRequired)
	n.expectCode(http.MethodPatch, path, `{"node_uuids":["b-1"],"allow":["management"]}`, http.StatusBadRequest, ErrorNetworkRoleInvalid)
	n.expectCode(http.MethodPatch, path, `{"node_uuids":["b-1"],"allow":["serving"],"deny":["serving"]}`, http.StatusBadRequest, ErrorManagementRolesConflict)
	missing := n.expect(n.admin, http.MethodPatch, path, `{"node_uuids":["b-3","gone"],"deny":["serving"]}`, http.StatusNotFound)
	if missing["code"] != ErrorManagementRolesNodeNotFound || !reflect.DeepEqual(missing["params"], map[string]any{"node": "gone"}) {
		t.Fatalf("missing node = %v", missing)
	}
	if view := n.expect(n.member, http.MethodGet, "/api/v1/nodes/b-3/networks", "", http.StatusOK); !reflect.DeepEqual(view["management"].(map[string]any)["roles_off"], []any{}) {
		t.Fatalf("a failed batch changed b-3: %v", view["management"])
	}
	n.expect(n.admin, http.MethodPatch, path, `{`, http.StatusBadRequest)
	if out := n.expect(n.member, http.MethodPatch, path, `{"node_uuids":["b-1"],"allow":["serving"]}`, http.StatusForbidden); out["code"] != ErrorAdministratorRequired {
		t.Fatalf("member code = %v", out["code"])
	}
	// The static path does not shadow a node called like it for GET.
	n.expect(n.member, http.MethodGet, "/api/v1/nodes/b-1", "", http.StatusOK)
}
