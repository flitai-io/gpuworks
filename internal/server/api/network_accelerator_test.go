package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// acceleratorNodes registers two nodes that report the network ports of their
// accelerator cards, as the two 910B4 hosts do: npu-1 owns cards 3 and 4,
// npu-2 owns 3, 4 and a card 6 whose port is down.
func acceleratorNodes(t *testing.T, n *networkAPI) {
	t.Helper()
	for uuid, wire := range map[string]string{
		"npu-1": `{"network":{"interfaces":[{"name":"br0","state":"up","addrs":["198.51.100.31/16"]}],"rdma_ports":[],
			"accelerator_ports":[{"name":"npu3","index":3,"ip":"203.0.113.59","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
			{"name":"npu4","index":4,"ip":"203.0.113.60","netmask":"255.255.0.0","state":"UP","speed_gbps":200}],
			"management":{"interface":"br0","ip":"198.51.100.31"}}}`,
		"npu-2": `{"network":{"interfaces":[{"name":"br0","state":"up","addrs":["198.51.100.32/16"]}],"rdma_ports":[],
			"accelerator_ports":[{"name":"npu3","index":3,"ip":"203.0.113.82","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
			{"name":"npu4","index":4,"ip":"203.0.113.83","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
			{"name":"npu6","index":6,"ip":"203.0.113.85","netmask":"255.255.0.0","state":"DOWN"}],
			"management":{"interface":"br0","ip":"198.51.100.32"}}}`,
	} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		var status proto.NodeStatus
		if err := json.Unmarshal([]byte(wire), &status); err != nil {
			t.Fatal(err)
		}
		if err := n.store.UpdateNodeStatus(uuid, status); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAcceleratorNetworkAPI(t *testing.T) {
	n := newNetworkAPI(t)
	acceleratorNodes(t, n)

	// The role dictionary tells the form which roles each type may carry.
	list := n.expect(n.admin, http.MethodGet, "/api/v1/networks", "", http.StatusOK)
	wantTypeRoles := map[string]any{
		"host_ethernet":    []any{"storage_sync", "compute", "serving"},
		"host_rdma":        []any{"storage_sync", "compute", "serving"},
		"accelerator_rdma": []any{"compute"},
		"accelerator_hccs": []any{"compute"},
	}
	if !reflect.DeepEqual(list["type_roles"], wantTypeRoles) || !reflect.DeepEqual(list["types"], []any{"host_ethernet", "host_rdma", "accelerator_rdma", "accelerator_hccs"}) {
		t.Fatalf("list dictionary = %v / %v", list["types"], list["type_roles"])
	}
	if builtin := list["items"].([]any)[0].(map[string]any); builtin["type"] != "host_ethernet" {
		t.Fatalf("management network type = %v", builtin["type"])
	}

	refused := n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"npu","roles":["compute","storage_sync"],"type":"accelerator_rdma"}`, http.StatusBadRequest)
	if refused["code"] != ErrorNetworkTypeRoleInvalid || !reflect.DeepEqual(refused["params"], map[string]any{"type": "accelerator_rdma", "role": "storage_sync"}) {
		t.Fatalf("refusal = %v", refused)
	}

	created := n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"npu","roles":["compute"],"type":"accelerator_rdma"}`, http.StatusCreated)
	if created["type"] != "accelerator_rdma" || !reflect.DeepEqual(created["type_roles"], wantTypeRoles) || created["test_supported"] != true {
		t.Fatalf("created = %v", created)
	}
	out := n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments/batch", `{"node_uuids":["npu-1","npu-2"]}`, http.StatusOK)
	detail := out["network"].(map[string]any)

	// The node's rails are its cards, and its address is a host address.
	one := nodeOf(t, detail, "npu-1")
	wantRails := []any{
		map[string]any{"name": "npu3", "ip": "203.0.113.59", "netdev": "", "link_layer": "roce", "state": "UP", "speed_gbps": float64(200)},
		map[string]any{"name": "npu4", "ip": "203.0.113.60", "netdev": "", "link_layer": "roce", "state": "UP", "speed_gbps": float64(200)},
	}
	if !reflect.DeepEqual(one["hcas"], wantRails) {
		t.Fatalf("npu-1 rails = %v\nwant %v", one["hcas"], wantRails)
	}
	if one["interface"] != "br0" || one["ip"] != "198.51.100.31" || one["link_layer"] != "roce" || one["hca_source"] != "auto" || one["bootstrap_source"] != "auto" {
		t.Fatalf("npu-1 = %v", one)
	}
	if !reflect.DeepEqual(one["warnings"], []any{}) {
		t.Fatalf("npu-1 warnings = %v", one["warnings"])
	}
	two := nodeOf(t, detail, "npu-2")
	if !reflect.DeepEqual(railNamesOf(two), []string{"npu3", "npu4"}) {
		t.Fatalf("npu-2 rails = %v, want the down card left out", railNamesOf(two))
	}

	// Pinning the down card is allowed and flagged; host RDMA names are not cards.
	hcas := "/api/v1/networks/1/hcas"
	n.expectCode(http.MethodPost, hcas, `{"op":"add","node_uuid":"npu-2","hca":"mlx5_0"}`, http.StatusBadRequest, ErrorNetworkHCAInvalid)
	detail = n.expect(n.admin, http.MethodPost, hcas, `{"op":"add","node_uuid":"npu-2","hca":"npu6"}`, http.StatusOK)
	two = nodeOf(t, detail, "npu-2")
	wantWarnings := []any{
		map[string]any{"code": "hca_unusable", "params": map[string]any{"value": "npu6"}},
		map[string]any{"code": "hca_mismatch", "params": map[string]any{"count": "3", "counts": "2, 3"}},
	}
	if !reflect.DeepEqual(railNamesOf(two), []string{"npu3", "npu4", "npu6"}) || two["hca_source"] != "manual" || !reflect.DeepEqual(two["warnings"], wantWarnings) {
		t.Fatalf("npu-2 after add = %v", two)
	}
	if gap := two["hcas"].([]any)[2].(map[string]any)["gap"]; gap != "inactive" {
		t.Fatalf("npu6 gap = %v", gap)
	}

	// The node page names the network by its type and lists the cards as rails.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/npu-1/networks", nil)
	req.Header.Set("Authorization", "Bearer "+n.member)
	rec := httptest.NewRecorder()
	n.router.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`"role":"compute","network_id":1,"network_name":"npu","network_type":"accelerator_rdma"`,
		`"interface":"br0","ip":"198.51.100.31","hcas":[{"name":"npu3","ip":"203.0.113.59","netdev":"","link_layer":"roce","state":"UP","speed_gbps":200},`,
	} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("status %d, body %s\nwant it to contain %s", rec.Code, body, want)
		}
	}

	// A rails test needs host RDMA ports; the TCP test still covers the
	// bootstrap addresses.
	n.expectCode(http.MethodPost, "/api/v1/networks/1/tests", `{"kind":"rdma_tool"}`, http.StatusBadRequest, ErrorNetworkTestKindInvalid)
}

// fillAcceleratorNetwork puts count nodes of cards cards each on a new
// accelerator RDMA network and returns its tests path.
func fillAcceleratorNetwork(t *testing.T, n *networkAPI, count, cards int) string {
	t.Helper()
	created := n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"fabric","roles":["compute"],"type":"accelerator_rdma"}`, http.StatusCreated)
	id := uint(created["id"].(float64))
	for i := 0; i < count; i++ {
		uuid := fmt.Sprintf("node-%03d", i)
		node, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		inv := proto.NetInventory{
			Interfaces: []proto.NetInterface{{Name: "br0", Addrs: []string{fmt.Sprintf("10.9.%d.%d/16", i/200, i%200+1)}}},
			Management: &proto.ManagementAddress{Interface: "br0", IP: fmt.Sprintf("10.9.%d.%d", i/200, i%200+1)},
		}
		for c := 0; c < cards; c++ {
			inv.AcceleratorPorts = append(inv.AcceleratorPorts, proto.AcceleratorPort{Name: fmt.Sprintf("npu%d", c), Index: c, IP: fmt.Sprintf("172.20.%d.%d", i, c), Netmask: "255.255.0.0", State: "UP", SpeedGbps: 200})
		}
		if err := n.store.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &inv}); err != nil {
			t.Fatal(err)
		}
		if _, err := n.store.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: id, NodeID: node.ID}); err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("/api/v1/networks/%d/tests", id)
}

func TestCardTestRequests(t *testing.T) {
	n := newNetworkAPI(t)
	tests := fillAcceleratorNetwork(t, n, 60, 8)
	estimate := tests + "/estimate"
	// The estimate is the same request, not started: it says how many
	// rounds the run takes and the longest it can last.
	got := n.getRawPost(t, estimate, `{"scope":"all","kind":"card_matrix","pattern":"ring"}`)
	if want := `{"kind":"card_matrix","pattern":"ring","node_count":60,"pair_count":60,"card":{"rounds":8,"probes":3840,"seconds":24,"max_seconds":600}}`; strings.TrimSpace(got) != want {
		t.Fatalf("estimate = %s\nwant       %s", got, want)
	}
	got = n.getRawPost(t, estimate, `{"scope":"nodes","node_uuids":["node-000","node-001"],"kind":"card_ping"}`)
	if want := `{"kind":"card_ping","pattern":"mesh","node_count":2,"pair_count":1,"card":{"rounds":1,"probes":8,"seconds":3,"max_seconds":600}}`; strings.TrimSpace(got) != want {
		t.Fatalf("estimate = %s\nwant       %s", got, want)
	}
	// A TCP test has no card part; its pairs are ordered.
	got = n.getRawPost(t, estimate, `{"scope":"nodes","node_uuids":["node-000","node-001","node-002"],"kind":"tcp"}`)
	if want := `{"kind":"tcp","pattern":"mesh","node_count":3,"pair_count":6}`; strings.TrimSpace(got) != want {
		t.Fatalf("estimate = %s\nwant       %s", got, want)
	}
	if run, err := n.store.LatestNetworkTestRun(1); err == nil {
		t.Fatalf("an estimate started a run: %+v", run)
	}
	// It is refused exactly as the start would be.
	n.expect(n.member, http.MethodPost, estimate, `{"scope":"all","kind":"card_ping"}`, http.StatusForbidden)
	n.expect(n.admin, http.MethodGet, estimate, "", http.StatusMethodNotAllowed)
	n.expect(n.admin, http.MethodPost, estimate, `{`, http.StatusBadRequest)
	n.expectCode(http.MethodPost, "/api/v1/networks/9/tests/estimate", `{"scope":"all"}`, http.StatusNotFound, ErrorNetworkNotFound)
	n.expectCode(http.MethodPost, estimate, `{"scope":"everything","kind":"card_ping"}`, http.StatusBadRequest, ErrorNetworkTestScopeInvalid)
	n.expectCode(http.MethodPost, estimate, `{"scope":"all","kind":"card_matrix","pattern":"mesh"}`, http.StatusBadRequest, ErrorNetworkTestTooLong)

	n.expectCode(http.MethodPost, tests, `{"scope":"all","kind":"card"}`, http.StatusBadRequest, ErrorNetworkTestKindInvalid)
	// What bounds a card test is its rounds: 60 nodes of 8 cards in a full
	// mesh matrix is 240 rounds, twelve minutes at worst.
	long := n.expect(n.admin, http.MethodPost, tests, `{"scope":"all","kind":"card_matrix","pattern":"mesh"}`, http.StatusBadRequest)
	if long["code"] != ErrorNetworkTestTooLong || !reflect.DeepEqual(long["params"], map[string]any{"rounds": float64(240), "seconds": float64(720), "maximum": float64(600)}) {
		t.Fatalf("too long = %v", long)
	}
	// The same nodes as a ring are eight rounds, and their same-named cards
	// in a mesh thirty.
	for _, body := range []string{`{"scope":"all","kind":"card_matrix","pattern":"ring"}`, `{"scope":"all","kind":"card_ping","pattern":"mesh"}`} {
		run := n.expect(n.admin, http.MethodPost, tests, body, http.StatusAccepted)
		if run["status"] != "running" || run["bandwidth"] != false || run["node_count"] != float64(60) {
			t.Fatalf("run = %v", run)
		}
		stored, err := n.store.LatestNetworkTestRun(1)
		if err != nil {
			t.Fatal(err)
		}
		if err := n.store.AbandonNetworkTestRun(stored.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Forty nodes fit a full mesh matrix: 20 node rounds of 8 shifts. A
	// running run already tells how many node pairs it will probe.
	forty := make([]string, 40)
	for i := range forty {
		forty[i] = fmt.Sprintf("%q", fmt.Sprintf("node-%03d", i))
	}
	run := n.expect(n.admin, http.MethodPost, tests, `{"scope":"nodes","node_uuids":[`+strings.Join(forty, ",")+`],"kind":"card_matrix","pattern":"mesh"}`, http.StatusAccepted)
	if run["kind"] != "card_matrix" || run["pattern"] != "mesh" || run["pair_count"] != float64(780) {
		t.Fatalf("run = %v", run)
	}
	stored, _ := n.store.LatestNetworkTestRun(1)
	if stored.Kind != "card_matrix" || stored.Bandwidth {
		t.Fatalf("stored run = %+v", stored)
	}

	// Results reach the detail page with each card pair's outcome, in the
	// exact keys the UI reads.
	results := `[{"source_uuid":"node-000","target_uuid":"node-001","target":"10.9.0.2","reachable":true,"rails":[` +
		`{"rail":"npu0","source_ip":"172.20.0.0","target_rail":"npu0","target_ip":"172.20.1.0","gbps":0,"reachable":true,"rtt_ms":0.25},` +
		`{"rail":"npu0","source_ip":"172.20.0.0","target_rail":"npu1","target_ip":"172.20.1.1","gbps":0,"code":"unreachable","error":"no answer within the time limit"}]}]`
	if err := n.store.FinishNetworkTestRun(stored.ID, results, 1, 0); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/networks/1", nil)
	req.Header.Set("Authorization", "Bearer "+n.member)
	rec := httptest.NewRecorder()
	n.router.ServeHTTP(rec, req)
	want := `"rails":[{"rail":"npu0","source_ip":"172.20.0.0","target_rail":"npu0","target_ip":"172.20.1.0","gbps":0,"reachable":true,"rtt_ms":0.25},` +
		`{"rail":"npu0","source_ip":"172.20.0.0","target_rail":"npu1","target_ip":"172.20.1.1","gbps":0,"code":"unreachable","error":"no answer within the time limit"}]`
	body := rec.Body.String()
	// One of two card pairs answered, the same-named one: the diagonal, read
	// from the results by the server, for the pair and for the run.
	if rec.Code != http.StatusOK || !strings.Contains(body, want+`,"card_shape":"diagonal"}`) || !strings.Contains(body, `"status":"passed"`) ||
		!strings.Contains(body, `"rail_count":2,"rail_passed_count":1,"card_shape":"diagonal"`) {
		t.Fatalf("status %d, latest test in %s", rec.Code, body)
	}
	// The list carries no test results at all.
	list := n.getRaw(t, "/api/v1/networks")
	if strings.Contains(list, "latest_test") || strings.Contains(list, "npu0") {
		t.Fatalf("network list carries test results: %s", list)
	}

	// Card tests are for accelerator networks only.
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"lan","roles":["compute"],"type":"host_ethernet","cidr":"10.9.0.0/16"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/2/attachments/batch", `{"node_uuids":["node-000","node-001"]}`, http.StatusOK)
	n.expectCode(http.MethodPost, "/api/v1/networks/2/tests", `{"scope":"all","kind":"card_ping"}`, http.StatusBadRequest, ErrorNetworkTestKindInvalid)
	n.expectCode(http.MethodPost, "/api/v1/networks/2/tests/estimate", `{"scope":"all","kind":"card_matrix"}`, http.StatusBadRequest, ErrorNetworkTestKindInvalid)
}

func (n *networkAPI) getRawPost(t *testing.T, path, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+n.admin)
	rec := httptest.NewRecorder()
	n.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func (n *networkAPI) getRaw(t *testing.T, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+n.member)
	rec := httptest.NewRecorder()
	n.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// A multi-node group of Ascend nodes is refused until the nodes share an
// accelerator RDMA network: on any other network its first collective fails.
func TestDistributedDeployOnAscendNeedsAnAcceleratorNetwork(t *testing.T) {
	n := newNetworkAPI(t)
	for uuid, wire := range map[string]string{
		"npu-1": `{"sys":{"hostname":"npu-1","gpu_vendor":"ascend","runtime_driver":"docker"},"gpus":[{"index":3,"name":"Ascend 910B4","vendor":"ascend"},{"index":4,"name":"Ascend 910B4","vendor":"ascend"}],
			"network":{"interfaces":[{"name":"br0","state":"up","addrs":["198.51.100.31/16"]}],"rdma_ports":[],
			"accelerator_ports":[{"name":"npu3","index":3,"ip":"203.0.113.59","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
			{"name":"npu4","index":4,"ip":"203.0.113.60","netmask":"255.255.0.0","state":"UP","speed_gbps":200}],
			"management":{"interface":"br0","ip":"198.51.100.31"}}}`,
		"npu-2": `{"sys":{"hostname":"npu-2","gpu_vendor":"ascend","runtime_driver":"docker"},"gpus":[{"index":3,"name":"Ascend 910B4","vendor":"ascend"},{"index":4,"name":"Ascend 910B4","vendor":"ascend"}],
			"network":{"interfaces":[{"name":"br0","state":"up","addrs":["198.51.100.32/16"]}],"rdma_ports":[],
			"accelerator_ports":[{"name":"npu3","index":3,"ip":"203.0.113.82","netmask":"255.255.0.0","state":"UP","speed_gbps":200},
			{"name":"npu4","index":4,"ip":"203.0.113.83","netmask":"255.255.0.0","state":"UP","speed_gbps":200}],
			"management":{"interface":"br0","ip":"198.51.100.32"}}}`,
		// Its cards' ports are all down, so it has no rail to offer.
		"npu-3": `{"sys":{"hostname":"npu-3","gpu_vendor":"ascend","runtime_driver":"docker"},"gpus":[{"index":0,"name":"Ascend 910B4","vendor":"ascend"},{"index":1,"name":"Ascend 910B4","vendor":"ascend"}],
			"network":{"interfaces":[{"name":"br0","state":"up","addrs":["198.51.100.33/16"]}],"rdma_ports":[],
			"accelerator_ports":[{"name":"npu0","index":0,"ip":"203.0.113.90","netmask":"255.255.0.0","state":"DOWN"}],
			"management":{"interface":"br0","ip":"198.51.100.33"}}}`,
	} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		var status proto.NodeStatus
		if err := json.Unmarshal([]byte(wire), &status); err != nil {
			t.Fatal(err)
		}
		if err := n.store.UpdateNodeStatus(uuid, status); err != nil {
			t.Fatal(err)
		}
	}
	h := apiHandlers{d: Deps{Store: n.store}}
	prepare := func(uuids ...string) (dto.DeployRequest, string, map[string]any) {
		t.Helper()
		req := dto.DeployRequest{NodeUUIDs: uuids, NNodes: len(uuids), Placement: map[string]any{"tp": float64(len(uuids))}}
		_, err := h.deployments().PrepareDistributedDeployRequest(&req, 1)
		var product *messages.ProductError
		if errors.As(err, &product) {
			return req, product.Code(), product.Params()
		}
		if err != nil {
			t.Fatalf("prepare %v: %v", uuids, err)
		}
		return req, "", nil
	}

	// With nothing declared the group would share the management network.
	_, code, params := prepare("npu-1", "npu-2")
	if code != messages.ErrorComputeNetworkAcceleratorRequired || !reflect.DeepEqual(params, map[string]any{"node": "npu-1", "network": "management"}) {
		t.Fatalf("on the management network: %s %v", code, params)
	}

	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"npu","roles":["compute"],"type":"accelerator_rdma"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments/batch", `{"node_uuids":["npu-1","npu-2","npu-3"]}`, http.StatusOK)
	req, code, params := prepare("npu-1", "npu-2")
	if code != "" {
		t.Fatalf("on the accelerator network: %s %v", code, params)
	}
	// The leader is dialed on its host address, never on a card's.
	if req.MasterAddr != "198.51.100.31" {
		t.Fatalf("master address = %q", req.MasterAddr)
	}
	_, code, params = prepare("npu-1", "npu-3")
	if code != messages.ErrorComputeNetworkNoCards || !reflect.DeepEqual(params, map[string]any{"node": "npu-3", "network": "npu"}) {
		t.Fatalf("a node without a usable card: %s %v", code, params)
	}
}
