package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type networkAPI struct {
	t      *testing.T
	router http.Handler
	admin  string
	member string
	store  *store.Store
	signer *auth.TokenSigner
}

func newNetworkAPI(t *testing.T) *networkAPI {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, member := roleTestToken(t, st, "member")
	_, admin := roleTestToken(t, st, "admin")
	return &networkAPI{t: t, router: Router(Deps{Store: st, AuthSigner: signer}), admin: admin, member: member, store: st, signer: signer}
}

func (n *networkAPI) do(token, method, path, body string) (int, map[string]any) {
	n.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	n.router.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			n.t.Fatalf("%s %s: body %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

func (n *networkAPI) expect(token, method, path, body string, status int) map[string]any {
	n.t.Helper()
	code, out := n.do(token, method, path, body)
	if code != status {
		n.t.Fatalf("%s %s = %d %v, want %d", method, path, code, out, status)
	}
	return out
}

func (n *networkAPI) expectCode(method, path, body string, status int, code string) {
	n.t.Helper()
	out := n.expect(n.admin, method, path, body, status)
	if out["code"] != code {
		n.t.Fatalf("%s %s code = %v, want %s (%v)", method, path, out["code"], code, out)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestNetworksAPIAuthorization(t *testing.T) {
	n := newNetworkAPI(t)
	n.expect("", http.MethodGet, "/api/v1/networks", "", http.StatusUnauthorized)
	n.expect(n.member, http.MethodGet, "/api/v1/networks", "", http.StatusOK)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/networks"},
		{http.MethodPut, "/api/v1/networks/1"},
		{http.MethodDelete, "/api/v1/networks/1"},
		{http.MethodPost, "/api/v1/networks/1/attachments"},
		{http.MethodPut, "/api/v1/networks/1/attachments/1"},
		{http.MethodDelete, "/api/v1/networks/1/attachments/1"},
		{http.MethodPost, "/api/v1/networks/1/tests"},
	} {
		out := n.expect(n.member, tc.method, tc.path, `{}`, http.StatusForbidden)
		if out["code"] != ErrorAdministratorRequired {
			t.Fatalf("%s %s code = %v", tc.method, tc.path, out["code"])
		}
	}
	n.expect(n.admin, http.MethodPatch, "/api/v1/networks/1", `{}`, http.StatusMethodNotAllowed)
}

func TestNetworksAPIValidation(t *testing.T) {
	n := newNetworkAPI(t)
	for _, tc := range []struct {
		body string
		code string
	}{
		{`{"roles":["compute"],"type":"host_rdma"}`, ErrorNetworkNameRequired},
		{`{"name":"` + strings.Repeat("x", 65) + `","roles":["compute"],"type":"host_rdma"}`, ErrorNetworkNameRequired},
		{`{"name":"a","roles":[],"type":"host_rdma"}`, ErrorNetworkRoleInvalid},
		{`{"name":"a","roles":["management"],"type":"host_rdma"}`, ErrorNetworkRoleInvalid},
		{`{"name":"a","roles":["compute"]}`, ErrorNetworkTypeInvalid},
		{`{"name":"a","roles":["compute"],"type":"infiniband"}`, ErrorNetworkTypeInvalid},
		{`{"name":"a","roles":["compute"],"type":"host_rdma","cidr":"10.0.0.0/33"}`, ErrorNetworkSelectorInvalid},
		{`{"name":"a","roles":["compute"],"type":"host_rdma","interface":"eth[0"}`, ErrorNetworkSelectorInvalid},
		{`{"name":"a","roles":["compute"],"type":"host_rdma","interface":"eth 0"}`, ErrorNetworkSelectorInvalid},
		// RDMA networks infer their bootstrap interface; only a CIDR may narrow it.
		{`{"name":"a","roles":["compute"],"type":"host_rdma","interface":"ib*"}`, ErrorNetworkSelectorInvalid},
		{`{"name":"a","roles":["compute"],"type":"accelerator_rdma","interface":"eth*"}`, ErrorNetworkSelectorInvalid},
		// The old type names are gone.
		{`{"name":"a","roles":["compute"],"type":"rdma"}`, ErrorNetworkTypeInvalid},
		{`{"name":"a","roles":["compute"],"type":"ethernet"}`, ErrorNetworkTypeInvalid},
		// So is the old name of the compute role.
		{`{"name":"a","roles":["parameter"],"type":"host_rdma"}`, ErrorNetworkRoleInvalid},
		// Accelerator cards carry compute traffic only.
		{`{"name":"a","roles":["compute","serving"],"type":"accelerator_rdma"}`, ErrorNetworkTypeRoleInvalid},
		{`{"name":"a","roles":["storage_sync"],"type":"accelerator_rdma"}`, ErrorNetworkTypeRoleInvalid},
	} {
		n.expectCode(http.MethodPost, "/api/v1/networks", tc.body, http.StatusBadRequest, tc.code)
	}
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{`, http.StatusBadRequest)
	n.expectCode(http.MethodGet, "/api/v1/networks/42", "", http.StatusNotFound, ErrorNetworkNotFound)
	n.expectCode(http.MethodPut, "/api/v1/networks/42", `{"name":"a","roles":["compute"],"type":"host_rdma"}`, http.StatusNotFound, ErrorNetworkNotFound)
	n.expectCode(http.MethodDelete, "/api/v1/networks/42", "", http.StatusNotFound, ErrorNetworkNotFound)
	n.expectCode(http.MethodPost, "/api/v1/networks/42/attachments", `{"label_key":"a"}`, http.StatusNotFound, ErrorNetworkNotFound)
}

func TestNetworksAPILifecycle(t *testing.T) {
	n := newNetworkAPI(t)
	h1, err := n.store.RegisterNode("uuid-h1", store.HashToken("t1"), "h20-01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.store.RenameNode(h1.UUID, "h20-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.store.SetNodeLabels(h1.UUID, map[string]string{"pool": "h20-a"}); err != nil {
		t.Fatal(err)
	}
	inv := proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "bond0", State: "up", MTU: 9000, SpeedMbps: 200000, Addrs: []string{"10.20.1.11/16"}}},
		RDMAPorts:  []proto.RDMAPort{{Device: "mlx5_0", Port: 1, State: "ACTIVE", LinkLayer: "InfiniBand", RateGbps: 400}},
	}
	if err := n.store.UpdateNodeStatus(h1.UUID, proto.NodeStatus{Network: &inv}); err != nil {
		t.Fatal(err)
	}

	created := n.expect(n.admin, http.MethodPost, "/api/v1/networks",
		`{"name":" ib-a ","description":"fabric","roles":["compute","compute"],"type":"host_rdma","cidr":"10.20.9.9/16"}`, http.StatusCreated)
	wantKeys := []string{"attachments", "builtin", "cidr", "created_at", "description", "id", "interface", "latest_test", "name", "nodes", "resolved_node_count", "role_options", "roles", "test_supported", "type", "type_options", "type_roles", "updated_at", "warning_node_count"}
	if got := keys(created); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("detail keys = %v\nwant %v", got, wantKeys)
	}
	if created["name"] != "ib-a" || created["cidr"] != "10.20.0.0/16" || !reflect.DeepEqual(created["roles"], []any{"compute"}) ||
		!reflect.DeepEqual(created["attachments"], []any{}) || !reflect.DeepEqual(created["nodes"], []any{}) ||
		!reflect.DeepEqual(created["role_options"], []any{"storage_sync", "compute", "serving"}) ||
		!reflect.DeepEqual(created["type_options"], []any{"host_ethernet", "host_rdma", "accelerator_rdma", "accelerator_hccs"}) {
		t.Fatalf("created = %v", created)
	}
	id := "/api/v1/networks/1"
	n.expectCode(http.MethodPost, "/api/v1/networks", `{"name":"IB-A","roles":["serving"],"type":"host_ethernet"}`, http.StatusConflict, ErrorNetworkNameTaken)
	legacy := n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"legacy","roles":["serving","compute"],"type":"host_ethernet","interface":"bond*"}`, http.StatusCreated)
	if !reflect.DeepEqual(legacy["roles"], []any{"compute", "serving"}) {
		t.Fatalf("roles not canonicalized: %v", legacy["roles"])
	}

	// Attachments: exactly one target, overrides only on nodes, HCAs only on RDMA.
	n.expectCode(http.MethodPost, id+"/attachments", `{}`, http.StatusBadRequest, ErrorNetworkAttachmentTargetInvalid)
	n.expectCode(http.MethodPost, id+"/attachments", `{"node_uuid":"uuid-h1","label_key":"pool"}`, http.StatusBadRequest, ErrorNetworkAttachmentTargetInvalid)
	n.expectCode(http.MethodPost, id+"/attachments", `{"node_uuid":"missing"}`, http.StatusBadRequest, ErrorNetworkAttachmentTargetInvalid)
	n.expectCode(http.MethodPost, id+"/attachments", `{"label_key":"bad key"}`, http.StatusBadRequest, ErrorNetworkAttachmentTargetInvalid)
	n.expectCode(http.MethodPost, id+"/attachments", `{"label_key":"pool","label_value":"h20-a","interface":"bond0"}`, http.StatusBadRequest, ErrorNetworkOverrideInvalid)
	n.expectCode(http.MethodPost, id+"/attachments", `{"node_uuid":"uuid-h1","ip":"10.20.1"}`, http.StatusBadRequest, ErrorNetworkOverrideInvalid)
	n.expectCode(http.MethodPost, id+"/attachments", `{"node_uuid":"uuid-h1","interface":"bond0;rm"}`, http.StatusBadRequest, ErrorNetworkOverrideInvalid)
	n.expectCode(http.MethodPost, "/api/v1/networks/2/attachments", `{"node_uuid":"uuid-h1","hcas":["mlx5_0"]}`, http.StatusBadRequest, ErrorNetworkOverrideInvalid)

	detail := n.expect(n.admin, http.MethodPost, id+"/attachments", `{"label_key":"pool","label_value":"h20-a"}`, http.StatusCreated)
	n.expectCode(http.MethodPost, id+"/attachments", `{"label_key":"pool","label_value":"h20-a"}`, http.StatusConflict, ErrorNetworkAttachmentExists)
	nodes := detail["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %v", nodes)
	}
	node := nodes[0].(map[string]any)
	ibRail := map[string]any{"name": "mlx5_0:1", "ip": "", "netdev": "", "link_layer": "infiniband", "state": "ACTIVE", "speed_gbps": float64(400)}
	wantNode := map[string]any{
		"node_uuid": "uuid-h1", "node_name": "h20-01", "labels": map[string]any{"pool": "h20-a"}, "roles": []any{"compute"}, "attachment_id": float64(1),
		"source": "label", "label_key": "pool", "label_value": "h20-a", "interface": "bond0", "ip": "10.20.1.11",
		"hcas": []any{ibRail}, "link_layer": "infiniband", "hca_source": "auto", "bootstrap_source": "auto", "tools": []any{}, "warnings": []any{},
	}
	if !reflect.DeepEqual(node, wantNode) {
		t.Fatalf("resolved node = %v\nwant %v", node, wantNode)
	}

	// legacy also reaches h1 by label for the compute role and loses the tie.
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/2/attachments", `{"label_key":"pool","label_value":"h20-a"}`, http.StatusCreated)
	list := n.expect(n.member, http.MethodGet, "/api/v1/networks", "", http.StatusOK)
	items := list["items"].([]any)
	if mgmt := items[0].(map[string]any); mgmt["builtin"] != true || mgmt["name"] != "management" || mgmt["id"] != float64(0) {
		t.Fatalf("management network not listed first: %v", mgmt)
	}
	items = items[1:]
	first := items[0].(map[string]any)
	if first["resolved_node_count"] != float64(1) || first["warning_node_count"] != float64(1) {
		t.Fatalf("list item = %v", first)
	}
	if second := items[1].(map[string]any); second["resolved_node_count"] != float64(1) || second["warning_node_count"] != float64(0) {
		t.Fatalf("legacy still serves h1 as the serving network: %v", second)
	}

	// A node attachment with overrides outranks the label, clearing the conflict.
	detail = n.expect(n.admin, http.MethodPost, id+"/attachments", `{"node_uuid":"uuid-h1","interface":"bond0","hcas":["mlx5_0"," ","mlx5_0","mlx5_9:1"]}`, http.StatusCreated)
	node = detail["nodes"].([]any)[0].(map[string]any)
	wantWarnings := []any{map[string]any{"code": "override_missing", "params": map[string]any{"kind": "hca", "value": "mlx5_9:1"}}}
	unknownRail := map[string]any{"name": "mlx5_9:1", "ip": "", "netdev": "", "link_layer": "", "state": "", "speed_gbps": float64(0), "gap": "unknown"}
	if node["source"] != "node" || !reflect.DeepEqual(node["hcas"], []any{ibRail, unknownRail}) || !reflect.DeepEqual(node["warnings"], wantWarnings) ||
		node["hca_source"] != "manual" || node["bootstrap_source"] != "manual" {
		t.Fatalf("node attachment = %v", node)
	}
	attachments := detail["attachments"].([]any)
	nodeAttachment := attachments[1].(map[string]any)
	if nodeAttachment["target"] != "node" || nodeAttachment["node_name"] != "h20-01" || !reflect.DeepEqual(nodeAttachment["hcas"], []any{"mlx5_0", "mlx5_9:1"}) {
		t.Fatalf("attachment = %v", nodeAttachment)
	}

	nodeView := n.expect(n.member, http.MethodGet, "/api/v1/nodes/uuid-h1/networks", "", http.StatusOK)
	if nodeView["inventory_reported"] != true || len(nodeView["networks"].([]any)) != 2 {
		t.Fatalf("node networks = %v", nodeView)
	}
	iface := nodeView["interfaces"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(keys(iface), []string{"addrs", "mac", "mtu", "name", "speed_mbps", "state"}) || iface["speed_mbps"] != float64(200000) {
		t.Fatalf("interface = %v", iface)
	}
	n.expect(n.member, http.MethodGet, "/api/v1/nodes/missing/networks", "", http.StatusNotFound)

	// Overrides can be edited on node attachments only.
	n.expectCode(http.MethodPut, id+"/attachments/1", `{"interface":"bond0"}`, http.StatusBadRequest, ErrorNetworkOverrideInvalid)
	n.expectCode(http.MethodPut, id+"/attachments/99", `{}`, http.StatusNotFound, ErrorNetworkAttachmentNotFound)
	n.expectCode(http.MethodPut, "/api/v1/networks/2/attachments/3", `{}`, http.StatusNotFound, ErrorNetworkAttachmentNotFound)
	detail = n.expect(n.admin, http.MethodPut, id+"/attachments/3", `{"interface":"","ip":"","hcas":[]}`, http.StatusOK)
	node = detail["nodes"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(node["hcas"], []any{ibRail}) || !reflect.DeepEqual(node["warnings"], []any{}) || node["hca_source"] != "auto" {
		t.Fatalf("cleared overrides = %v", node)
	}

	n.expect(n.admin, http.MethodPut, id, `{"name":"ib-a","roles":["compute","storage_sync"],"type":"host_rdma"}`, http.StatusOK)
	n.expectCode(http.MethodPut, id, `{"name":"legacy","roles":["compute"],"type":"host_rdma"}`, http.StatusConflict, ErrorNetworkNameTaken)
	n.expectCode(http.MethodDelete, id+"/attachments/99", "", http.StatusNotFound, ErrorNetworkAttachmentNotFound)
	detail = n.expect(n.admin, http.MethodDelete, id+"/attachments/3", "", http.StatusOK)
	if len(detail["attachments"].([]any)) != 1 {
		t.Fatalf("attachments after detach = %v", detail["attachments"])
	}
	n.expect(n.admin, http.MethodDelete, id, "", http.StatusOK)
	n.expectCode(http.MethodGet, id, "", http.StatusNotFound, ErrorNetworkNotFound)
}

func TestNetworkChangesResyncOnlineNodes(t *testing.T) {
	n := newNetworkAPI(t)
	for _, uuid := range []string{"online", "offline"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	var synced []string
	signer, _ := roleTestToken(t, n.store, "admin")
	n.router = Router(Deps{
		Store: n.store, AuthSigner: signer,
		NodeSync: nodeSyncCallbacks{request: func(uuid string) { synced = append(synced, uuid) }},
		Online:   func(uuid string) bool { return uuid == "online" },
	})
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"s","roles":["storage_sync"],"type":"host_ethernet"}`, http.StatusCreated)
	synced = nil
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"label_key":"pool","label_value":"a"}`, http.StatusCreated)
	if !reflect.DeepEqual(synced, []string{"online"}) {
		t.Fatalf("attach synced %v", synced)
	}
	synced = nil
	n.expect(n.admin, http.MethodPut, "/api/v1/nodes/online/labels", `{"labels":{"pool":"a"}}`, http.StatusOK)
	if !reflect.DeepEqual(synced, []string{"online"}) {
		t.Fatalf("label change synced %v", synced)
	}
	synced = nil
	n.expect(n.admin, http.MethodDelete, "/api/v1/networks/1", "", http.StatusOK)
	if !reflect.DeepEqual(synced, []string{"online"}) {
		t.Fatalf("delete synced %v", synced)
	}
	synced = nil
	n.expect(n.admin, http.MethodDelete, "/api/v1/networks/1", "", http.StatusNotFound)
	if len(synced) != 0 {
		t.Fatalf("failed delete synced %v", synced)
	}
}

func TestPrepareDistributedDeployRequestUsesComputeNetworks(t *testing.T) {
	n := newNetworkAPI(t)
	for i, uuid := range []string{"h1", "h2", "h3"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		inv := proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "bond0", Addrs: []string{"10.20.1." + string(rune('1'+i)) + "/16"}}}}
		// No direct address: the compute network alone makes the rank reachable.
		if err := n.store.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:     proto.SysInfo{Hostname: uuid, RuntimeDriver: []string{"docker", "docker", "udocker"}[i]},
			GPUs:    []proto.GPUInfo{{Index: 0, Name: "GPU"}},
			Network: &inv,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ib, _ := n.store.CreateNetwork(&store.Network{Name: "ib-a", Type: netplan.TypeHostRDMA, CIDR: "10.20.0.0/16", Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	eth, _ := n.store.CreateNetwork(&store.Network{Name: "eth-b", Type: netplan.TypeHostEthernet, CIDR: "10.20.0.0/16", Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	h := apiHandlers{d: Deps{Store: n.store}}
	prepare := func(uuids ...string) (dto.DeployRequest, error) {
		req := dto.DeployRequest{NodeUUIDs: uuids, NNodes: len(uuids), Placement: map[string]any{"tp": float64(len(uuids))}}
		_, err := h.deployments().PrepareDistributedDeployRequest(&req, 1)
		return req, err
	}
	code := func(err error) any {
		var pe interface{ Code() string }
		if errors.As(err, &pe) {
			return pe.Code()
		}
		return err
	}
	attach := func(network *store.Network, uuid string) {
		t.Helper()
		node, _ := n.store.NodeByUUID(uuid)
		if _, err := n.store.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: network.ID, NodeID: node.ID}); err != nil {
			t.Fatal(err)
		}
	}

	attach(ib, "h1")
	if _, err := prepare("h1", "h2"); code(err) != ErrorComputeNetworkMixed {
		t.Fatalf("mixed err = %v", err)
	}
	attach(eth, "h2")
	if _, err := prepare("h1", "h2"); code(err) != ErrorComputeNetworkMismatch {
		t.Fatalf("mismatch err = %v", err)
	}
	attach(ib, "h3")
	if _, err := prepare("h1", "h3"); code(err) != messages.ErrorComputeNetworkRuntime {
		t.Fatalf("udocker err = %v", err)
	}
	if err := n.store.DeleteNetwork(eth.ID); err != nil {
		t.Fatal(err)
	}
	attach(ib, "h2")
	req, err := prepare("h2", "h1")
	if err != nil {
		t.Fatal(err)
	}
	if req.MasterAddr != "10.20.1.2" {
		t.Fatalf("master addr = %q, want the leader's compute IP", req.MasterAddr)
	}
}

func TestNetworkTestRequests(t *testing.T) {
	n := newNetworkAPI(t)
	for i, uuid := range []string{"a", "b", "c", "d"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		labels := map[string]string{"rack": "r1"}
		if i >= 2 {
			labels["rack"] = "r2"
		}
		if _, err := n.store.SetNodeLabels(uuid, labels); err != nil {
			t.Fatal(err)
		}
	}
	lan := n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"lan","roles":["storage_sync"],"type":"host_ethernet","cidr":"10.30.0.0/16"}`, http.StatusCreated)
	if lan["test_supported"] != true || lan["latest_test"] != nil {
		t.Fatalf("new network = %v", lan)
	}
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"label_key":"rack","label_value":"r1"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"label_key":"rack","label_value":"r2"}`, http.StatusCreated)
	serving := n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"svc","roles":["serving"],"type":"host_ethernet"}`, http.StatusCreated)
	if serving["test_supported"] != false {
		t.Fatalf("serving network = %v", serving)
	}

	tests := "/api/v1/networks/1/tests"
	n.expectCode(http.MethodPost, "/api/v1/networks/2/tests", `{"scope":"all"}`, http.StatusBadRequest, ErrorNetworkTestUnsupported)
	n.expectCode(http.MethodPost, "/api/v1/networks/9/tests", `{"scope":"all"}`, http.StatusNotFound, ErrorNetworkNotFound)
	n.expectCode(http.MethodPost, tests, `{"scope":"everything"}`, http.StatusBadRequest, ErrorNetworkTestScopeInvalid)
	n.expectCode(http.MethodPost, tests, `{"scope":"label"}`, http.StatusBadRequest, ErrorNetworkTestScopeInvalid)
	n.expectCode(http.MethodPost, tests, `{"scope":"nodes","node_uuids":["a","zzz"]}`, http.StatusBadRequest, ErrorNetworkTestNodeInvalid)
	n.expectCode(http.MethodPost, tests, `{"scope":"nodes","node_uuids":["a","a"]}`, http.StatusBadRequest, ErrorNetworkTestNodeCount)
	n.expectCode(http.MethodPost, tests, `{"scope":"label","label_key":"rack","label_value":"r9"}`, http.StatusBadRequest, ErrorNetworkTestNodeCount)
	n.expectCode(http.MethodPost, tests, `{"scope":"all","seconds":11}`, http.StatusBadRequest, ErrorPreflightDurationInvalid)
	n.expectCode(http.MethodPost, tests, `{"scope":"all","pattern":"star"}`, http.StatusBadRequest, ErrorNetworkTestPatternInvalid)

	run := n.expect(n.admin, http.MethodPost, tests, `{"scope":"label","label_key":"rack","label_value":"r2","bandwidth":false}`, http.StatusAccepted)
	wantKeys := []string{"bandwidth", "id", "kind", "label_key", "label_value", "node_count", "node_uuids", "pair_count", "pairs", "partial_count", "passed_count", "pattern", "rail_count", "rail_passed_count", "scope", "started_at", "status"}
	if !reflect.DeepEqual(keys(run), wantKeys) {
		t.Fatalf("run keys = %v", keys(run))
	}
	// A running mesh of two nodes will probe both ordered pairs.
	if run["status"] != "running" || run["kind"] != "tcp" || run["pattern"] != "mesh" || run["pair_count"] != float64(2) || run["node_count"] != float64(2) || run["label_key"] != "rack" || run["label_value"] != "r2" || run["bandwidth"] != false ||
		!reflect.DeepEqual(run["node_uuids"], []any{"c", "d"}) {
		t.Fatalf("run = %v", run)
	}
	n.expectCode(http.MethodPost, tests, `{"scope":"all"}`, http.StatusConflict, ErrorNetworkTestRunning)
	stored, _ := n.store.LatestNetworkTestRun(1)
	if stored.Nodes != `["c","d"]` || stored.Seconds != 3 || stored.Pattern != "mesh" || stored.LabelKey != "rack" || stored.LabelValue != "r2" {
		t.Fatalf("stored run = %+v", stored)
	}

	// Results reach the detail page with node names.
	if err := n.store.FinishNetworkTestRun(stored.ID, `[{"source_uuid":"c","target_uuid":"d","target":"10.30.1.4:40000","reachable":true,"rtt_ms_p50":0.2},{"source_uuid":"d","target_uuid":"c","error":"dial tcp: i/o timeout"}]`, 2, 1); err != nil {
		t.Fatal(err)
	}
	detail := n.expect(n.member, http.MethodGet, "/api/v1/networks/1", "", http.StatusOK)
	latest := detail["latest_test"].(map[string]any)
	pairs := latest["pairs"].([]any)
	first := pairs[0].(map[string]any)
	if latest["status"] != "done" || latest["passed_count"] != float64(1) || len(pairs) != 2 || first["source_name"] != "c" || first["passed"] != true {
		t.Fatalf("latest = %v", latest)
	}
	if second := pairs[1].(map[string]any); second["passed"] != false || second["error"] != "dial tcp: i/o timeout" {
		t.Fatalf("failed pair = %v", second)
	}
	if first["status"] != "passed" || pairs[1].(map[string]any)["status"] != "failed" {
		t.Fatalf("statuses = %v", pairs)
	}
	run = n.expect(n.admin, http.MethodPost, tests, `{"scope":"all","pattern":"ring","seconds":2}`, http.StatusAccepted)
	if run["pattern"] != "ring" || run["pair_count"] != float64(4) || run["node_count"] != float64(4) || run["bandwidth"] != true {
		t.Fatalf("ring run = %v", run)
	}
}

// An rdma_tool run's pairs report each rail and a partial status; the run
// counts partial pairs and rails.
func TestNetworkTestRunReportsRails(t *testing.T) {
	n := newNetworkAPI(t)
	for _, uuid := range []string{"a", "b"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"f","roles":["compute"],"type":"host_rdma"}`, http.StatusCreated)
	run, err := n.store.StartNetworkTestRun(&store.NetworkTestRun{NetworkID: 1, Kind: "rdma_tool", Pattern: "ring", Scope: "all", Bandwidth: true, Nodes: `["a","b"]`, NodeCount: 2})
	if err != nil {
		t.Fatal(err)
	}
	pairs := `[{"source_uuid":"a","target_uuid":"b","target":"100.64.29.210","reachable":true,"gbps":24.4,"rails":[
		{"rail":"r0:1","source_ip":"100.64.29.209","target_rail":"r0:1","target_ip":"100.64.29.210","gbps":24.4},
		{"rail":"r9:1","source_ip":"","target_rail":"r9:1","gbps":0,"code":"no_roce_v2_gid","side":"target","error":"RoCE port has no IPv4 RoCE v2 GID"}]},
		{"source_uuid":"b","target_uuid":"a","code":"offline","side":"target","error":"target is offline"}]`
	if err := n.store.FinishNetworkTestRun(run.ID, pairs, 2, 0); err != nil {
		t.Fatal(err)
	}
	latest := n.expect(n.member, http.MethodGet, "/api/v1/networks/1", "", http.StatusOK)["latest_test"].(map[string]any)
	if latest["pattern"] != "ring" || latest["partial_count"] != float64(1) || latest["rail_count"] != float64(2) || latest["rail_passed_count"] != float64(1) {
		t.Fatalf("latest = %v", latest)
	}
	got := latest["pairs"].([]any)
	ab, ba := got[0].(map[string]any), got[1].(map[string]any)
	wantRail := map[string]any{"rail": "r9:1", "target_rail": "r9:1", "gbps": float64(0), "code": "no_roce_v2_gid", "side": "target", "error": "RoCE port has no IPv4 RoCE v2 GID"}
	if ab["status"] != "partial" || ab["passed"] != false || !reflect.DeepEqual(ab["rails"].([]any)[1], wantRail) {
		t.Fatalf("a->b = %v", ab)
	}
	if ba["status"] != "failed" || ba["code"] != "offline" || ba["side"] != "target" || ba["rails"] != nil {
		t.Fatalf("b->a = %v", ba)
	}
}

func TestNodeNetworksReportTheServingForward(t *testing.T) {
	n := newNetworkAPI(t)
	node, err := n.store.RegisterNode("svc-node", store.HashToken("svc"), "svc-node")
	if err != nil {
		t.Fatal(err)
	}
	inv := proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth1", Addrs: []string{"10.40.0.5/24"}}}}
	if err := n.store.UpdateNodeStatus(node.UUID, proto.NodeStatus{Network: &inv}); err != nil {
		t.Fatal(err)
	}
	view := n.expect(n.member, http.MethodGet, "/api/v1/nodes/svc-node/networks", "", http.StatusOK)
	if view["serving_forward"] != nil {
		t.Fatalf("serving_forward without a serving network = %v", view["serving_forward"])
	}
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"svc","roles":["serving"],"type":"host_ethernet","cidr":"10.40.0.0/24"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"node_uuid":"svc-node"}`, http.StatusCreated)
	view = n.expect(n.member, http.MethodGet, "/api/v1/nodes/svc-node/networks", "", http.StatusOK)
	if got := view["serving_forward"]; !reflect.DeepEqual(got, map[string]any{"address": "10.40.0.5:19443"}) {
		t.Fatalf("untried forward = %v", got)
	}
	if err := n.store.SetNodeServingForward("svc-node", store.ServingForwardStatus{Mode: store.ServingForwardUnreachable, Reason: "dial tcp: i/o timeout", At: time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	view = n.expect(n.member, http.MethodGet, "/api/v1/nodes/svc-node/networks", "", http.StatusOK)
	want := map[string]any{"address": "10.40.0.5:19443", "mode": "unreachable", "reason": "dial tcp: i/o timeout", "at": "2026-09-26T01:00:00Z"}
	if got := view["serving_forward"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("forward = %v", got)
	}
}

func TestNodeListCarriesTheComputeNetworkAndInstancesKeepRankZero(t *testing.T) {
	n := newNetworkAPI(t)
	node, err := n.store.RegisterNode("ib-node", store.HashToken("ib"), "ib-node")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := n.store.RegisterNode("plain", store.HashToken("plain"), "plain")
	inv := proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{"10.96.0.56/32"}}}}
	if err := n.store.UpdateNodeStatus(node.UUID, proto.NodeStatus{Network: &inv}); err != nil {
		t.Fatal(err)
	}
	ib, _ := n.store.CreateNetwork(&store.Network{Name: "fabric", Type: netplan.TypeHostRDMA, CIDR: "10.96.0.0/13", Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if _, err := n.store.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: ib.ID, NodeID: node.ID}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+n.member)
	rec := httptest.NewRecorder()
	n.router.ServeHTTP(rec, req)
	var nodes []map[string]any
	if err := unmarshalList(rec.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	byUUID := map[string]map[string]any{}
	for _, v := range nodes {
		byUUID[v["uuid"].(string)] = v
	}
	if got := byUUID[node.UUID]; got["compute_network"] != "fabric" || got["compute_address"] != "10.96.0.56" {
		t.Fatalf("ib node = %v", got)
	}
	if _, ok := byUUID[other.UUID]["compute_network"]; ok {
		t.Fatalf("node without a compute network = %v", byUUID[other.UUID])
	}
	detail := n.expect(n.member, http.MethodGet, "/api/v1/nodes/ib-node", "", http.StatusOK)
	if detail["compute_address"] != "10.96.0.56" {
		t.Fatalf("detail = %v", detail["compute_address"])
	}

	model, _ := n.store.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Name: "m"})
	dep, _ := n.store.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, State: "active", NNodes: 2})
	if _, err := n.store.CreateInstance(&store.Instance{UUID: "leader", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, NodeRank: 0}); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/instances", nil)
	req.Header.Set("Authorization", "Bearer "+n.member)
	rec = httptest.NewRecorder()
	n.router.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"node_rank":0`) {
		t.Fatalf("leader instance lost its rank: %s", rec.Body.String())
	}
}

func TestRDMAToolTestRequests(t *testing.T) {
	n := newNetworkAPI(t)
	for _, uuid := range []string{"a", "b"} {
		node, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		inv := proto.NetInventory{
			Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{"10.96.0." + map[string]string{"a": "1", "b": "2"}[uuid] + "/32"}}},
			RDMAPorts:  []proto.RDMAPort{{Device: "mlx5_0", Port: 1, State: "ACTIVE", LinkLayer: "InfiniBand", RateGbps: 400}},
		}
		if uuid == "a" {
			inv.Tools = []string{"ib_write_bw"}
		}
		if err := n.store.UpdateNodeStatus(node.UUID, proto.NodeStatus{Network: &inv}); err != nil {
			t.Fatal(err)
		}
		if _, err := n.store.SetNodeLabels(uuid, map[string]string{"pool": "ib"}); err != nil {
			t.Fatal(err)
		}
	}
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"eth","roles":["storage_sync"],"type":"host_ethernet","cidr":"10.96.0.0/13"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"ib","roles":["compute"],"type":"host_rdma","cidr":"10.96.0.0/13"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"label_key":"pool","label_value":"ib"}`, http.StatusCreated)
	detail := n.expect(n.admin, http.MethodPost, "/api/v1/networks/2/attachments", `{"label_key":"pool","label_value":"ib"}`, http.StatusCreated)
	tools := map[string]any{}
	for _, node := range detail["nodes"].([]any) {
		m := node.(map[string]any)
		tools[m["node_uuid"].(string)] = m["tools"]
	}
	if !reflect.DeepEqual(tools, map[string]any{"a": []any{"ib_write_bw"}, "b": []any{}}) {
		t.Fatalf("tools = %v", tools)
	}

	n.expectCode(http.MethodPost, "/api/v1/networks/2/tests", `{"kind":"iperf","scope":"all"}`, http.StatusBadRequest, ErrorNetworkTestKindInvalid)
	n.expectCode(http.MethodPost, "/api/v1/networks/1/tests", `{"kind":"rdma_tool","scope":"all"}`, http.StatusBadRequest, ErrorNetworkTestKindInvalid)
	run := n.expect(n.admin, http.MethodPost, "/api/v1/networks/2/tests", `{"kind":"rdma_tool","scope":"all","bandwidth":false}`, http.StatusAccepted)
	if run["kind"] != "rdma_tool" || run["bandwidth"] != true {
		t.Fatalf("rdma run = %v (bandwidth is what an rdma test measures)", run)
	}
	stored, _ := n.store.LatestNetworkTestRun(2)
	if err := n.store.FinishNetworkTestRun(stored.ID, `[{"source_uuid":"a","target_uuid":"b","target":"10.96.0.2","reachable":true,"gbps":700,"rails":[{"rail":"mlx5_0:1","gbps":350},{"rail":"mlx5_1:1","gbps":350}]}]`, 1, 1); err != nil {
		t.Fatal(err)
	}
	latest := n.expect(n.member, http.MethodGet, "/api/v1/networks/2", "", http.StatusOK)["latest_test"].(map[string]any)
	pair := latest["pairs"].([]any)[0].(map[string]any)
	if latest["kind"] != "rdma_tool" || pair["passed"] != true || len(pair["rails"].([]any)) != 2 {
		t.Fatalf("latest = %v", latest)
	}
}

// NCCL tests in an image were removed: the kind is refused like any other
// unknown one, whatever image the request names.
func TestNCCLImageTestsAreRefused(t *testing.T) {
	n := newNetworkAPI(t)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"ib","roles":["compute"],"type":"host_rdma"}`, http.StatusCreated)
	n.expectCode(http.MethodPost, "/api/v1/networks/1/tests", `{"kind":"nccl_image","image":"vllm/vllm-openai:nightly","scope":"all"}`, http.StatusBadRequest, ErrorNetworkTestKindInvalid)
}

// testManagement is the network inventory a real agent reports: the address
// it reaches the server from, which the management network carries.
func testManagement(ip string) *proto.NetInventory {
	return &proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{ip + "/24"}}},
		Management: &proto.ManagementAddress{Interface: "eth0", IP: ip},
	}
}

// The node list and detail carry the generated labels as their own field,
// apart from the written ones, with the literal keys and values a selector
// is matched on and the text to show for each.
func TestNodeListCarriesTheGeneratedSystemLabels(t *testing.T) {
	n := newNetworkAPI(t)
	node, err := n.store.RegisterNode("labelled", store.HashToken("labelled"), "labelled")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys: proto.SysInfo{Arch: "amd64", GPUVendor: "nvidia"},
		Network: &proto.NetInventory{
			Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{"10.96.0.60/32"}}},
			Management: &proto.ManagementAddress{Interface: "eth0", IP: "10.96.0.60"},
		},
		GPUs: []proto.GPUInfo{
			{Index: 0, UUID: "g-0", Name: "NVIDIA GeForce RTX 4090", ComputeCapability: "8.9", MemoryTotalBytes: 24 << 30, Healthy: true},
			{Index: 1, UUID: "g-1", Name: "Quadro RTX 8000", ComputeCapability: "7.5", MemoryTotalBytes: 48 << 30, Healthy: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.store.SetNodeLabels(node.UUID, map[string]string{"pool": "inference"}); err != nil {
		t.Fatal(err)
	}
	fabric, _ := n.store.CreateNetwork(&store.Network{Name: "fabric", Type: netplan.TypeHostRDMA, CIDR: "10.96.0.0/13", Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if _, err := n.store.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: fabric.ID, NodeID: node.ID}); err != nil {
		t.Fatal(err)
	}

	detail := n.expect(n.member, http.MethodGet, "/api/v1/nodes/labelled", "", http.StatusOK)
	raw, err := json.Marshal(detail["system_labels"])
	if err != nil {
		t.Fatal(err)
	}
	var labels []struct {
		Key         string `json:"key"`
		Value       string `json:"value"`
		Display     string `json:"display"`
		DisplayCode string `json:"display_code"`
	}
	if err := json.Unmarshal(raw, &labels); err != nil {
		t.Fatal(err)
	}
	var rendered []string
	for _, label := range labels {
		rendered = append(rendered, label.Key+"="+label.Value+"|"+label.Display+"|"+label.DisplayCode)
	}
	want := []string{
		"gpuworks.ai/compute-network=" + strconv.FormatUint(uint64(fabric.ID), 10) + "|fabric|",
		"gpuworks.ai/cpu-arch=amd64|amd64|",
		"gpuworks.ai/gpu-arch=ada|Ada|",
		"gpuworks.ai/gpu-arch=turing|Turing|",
		"gpuworks.ai/gpu-model=GeForce RTX 4090|GeForce RTX 4090|",
		"gpuworks.ai/gpu-model=Quadro RTX 8000|Quadro RTX 8000|",
		"gpuworks.ai/node=" + node.UUID + "|labelled|",
		"gpuworks.ai/serving-network=management|Management network|networks:management.name",
		// The default local storage applies to every node.
		"gpuworks.ai/storage=" + strconv.FormatUint(uint64(n.store.DefaultLocalStorageID()), 10) + "|default|",
		"gpuworks.ai/storage-sync-network=management|Management network|networks:management.name",
		"gpuworks.ai/vendor=nvidia|NVIDIA|common:vendor.nvidia",
	}
	if !reflect.DeepEqual(rendered, want) {
		t.Fatalf("system labels =\n%s\nwant\n%s", strings.Join(rendered, "\n"), strings.Join(want, "\n"))
	}
	// The written labels keep their own field and their own shape.
	if got := detail["labels"]; !reflect.DeepEqual(got, map[string]any{"pool": "inference"}) {
		t.Fatalf("labels = %v", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+n.member)
	rec := httptest.NewRecorder()
	n.router.ServeHTTP(rec, req)
	var listed []map[string]any
	if err := unmarshalList(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	for _, view := range listed {
		if view["uuid"] != node.UUID {
			continue
		}
		if !reflect.DeepEqual(view["system_labels"], detail["system_labels"]) {
			t.Fatalf("the list and the detail disagree:\n%v\n%v", view["system_labels"], detail["system_labels"])
		}
		return
	}
	t.Fatal("node missing from the list")
}
