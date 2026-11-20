package api

import (
	"net/http"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSearchNetworkTargets(t *testing.T) {
	n := newNetworkAPI(t)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"fabric","roles":["compute"],"type":"host_rdma"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"label_key":"pool","label_value":"Training"}`, http.StatusCreated)
	for _, tc := range []struct {
		kind, q string
		want    []any
	}{
		{"attachments", "POOL%3Dtrain", []any{"1"}},
		{"nodes", "train", []any{}},
		{"attachments", "absent", []any{}},
	} {
		got := n.expect(n.member, http.MethodGet, "/api/v1/networks/1/search?kind="+tc.kind+"&q="+tc.q, "", http.StatusOK)["ids"]
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %s: got %v want %v", tc.kind, tc.q, got, tc.want)
		}
	}
	if _, err := n.store.RegisterNode("gpu-direct", store.HashToken("local"), "gpu-direct"); err != nil {
		t.Fatal(err)
	}
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"node_uuid":"gpu-direct"}`, http.StatusCreated)
	for _, tc := range []struct {
		params string
		want   []any
	}{
		{"role=serving&role=compute&source=label&source=node", []any{"gpu-direct"}},
		{"role=serving&source=node", []any{}},
		{"role=compute&source=label", []any{}},
	} {
		got := n.expect(n.member, http.MethodGet, "/api/v1/networks/1/search?kind=nodes&q=gpu&"+tc.params, "", http.StatusOK)["ids"]
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.params, got, tc.want)
		}
	}
	n.expect(n.member, http.MethodGet, "/api/v1/networks/999/search?kind=nodes", "", http.StatusNotFound)
	n.expect(n.member, http.MethodGet, "/api/v1/networks/1/search?kind=unknown", "", http.StatusBadRequest)
}

func TestNetworkTargetSearchFields(t *testing.T) {
	detail := dto.NetworkDetail{Network: dto.Network{Attachments: []dto.NetworkAttachment{{ID: 2, Target: "node", NodeName: "worker", HCAs: []string{"mlx5_0:1"}}}}, Nodes: []dto.NetworkNode{
		{NodeUUID: "abc", NodeName: "worker", IP: "192.0.2.4", HCAs: []dto.NetworkRail{{Name: "mlx5_0:1", IP: "198.51.100.2", NetDev: "ens3"}}, Labels: map[string]string{"zone": "East"}},
	}}
	for _, q := range []string{"WORK", "192.0.2.", "mlx5", "198.51.100.", "ENS3", "zone=east"} {
		if ids := matchNetworkTargets(detail, "nodes", q, nil, nil).IDs; !reflect.DeepEqual(ids, []string{"abc"}) {
			t.Errorf("%q: %v", q, ids)
		}
	}
	if ids := matchNetworkTargets(detail, "attachments", "=", nil, nil).IDs; len(ids) != 0 {
		t.Fatalf("node matched empty label: %v", ids)
	}
}

func TestNetworkNodeSearchIncludesOnlyItsOwnOverrides(t *testing.T) {
	detail := dto.NetworkDetail{
		Network: dto.Network{Attachments: []dto.NetworkAttachment{
			{ID: 2, Target: "node", Interface: "bond7", IP: "192.0.2.101", HCAs: []string{"absent_rail:9"}},
			{ID: 3, Target: "node", Interface: "unresolved-only"},
		}},
		Nodes: []dto.NetworkNode{
			{NodeUUID: "node-a", AttachmentID: 2, Interface: "eth0"},
			{NodeUUID: "node-b", AttachmentID: 4, Interface: "eth1"},
		},
	}
	for _, q := range []string{"ETH0", "bond7", "192.0.2.101", "ABSENT_RAIL"} {
		if ids := matchNetworkTargets(detail, "nodes", q, nil, nil).IDs; !reflect.DeepEqual(ids, []string{"node-a"}) {
			t.Errorf("search %q: %v", q, ids)
		}
	}
	if ids := matchNetworkTargets(detail, "nodes", "unresolved-only", nil, nil).IDs; len(ids) != 0 {
		t.Errorf("unresolved override matched resolved nodes: %v", ids)
	}
}

func TestNetworkNodeSearchCombinesRoleSourceAndQuery(t *testing.T) {
	detail := dto.NetworkDetail{Nodes: []dto.NetworkNode{
		{NodeUUID: "a", NodeName: "gpu-01", Roles: []string{"serving", "compute"}, Source: "label"},
		{NodeUUID: "b", NodeName: "gpu-02", Roles: []string{"storage_sync"}, Source: "label"},
		{NodeUUID: "c", NodeName: "gpu-03", Roles: []string{"compute"}, Source: "node"},
		{NodeUUID: "d", NodeName: "other", Roles: []string{"compute"}, Source: "label"},
	}}
	for _, tc := range []struct {
		q                    string
		roles, sources, want []string
	}{
		{"gpu", []string{"compute", "storage_sync"}, []string{"label"}, []string{"a", "b"}},
		{"gpu", []string{"compute"}, []string{"label", "node"}, []string{"a", "c"}},
		{"", []string{"serving"}, nil, []string{"a"}},
		{"", nil, []string{"node"}, []string{"c"}},
		{"other", nil, []string{"node"}, []string{}},
	} {
		if got := matchNetworkTargets(detail, "nodes", tc.q, tc.roles, tc.sources).IDs; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
}
