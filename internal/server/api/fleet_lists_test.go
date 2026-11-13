package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fleetFixture is four nodes in four states:
//
//	node-a  online    nvidia  24 GiB + 48 GiB (unhealthy)  pool=inference rack=r1  64 GiB RAM
//	node-b  offline   nvidia  80 GiB                       pool=training           32 GiB RAM
//	node-c  online     ascend  64 GiB                       rack=r2
//	node-d  connecting no accelerator, no labels
func fleetFixture(t *testing.T) *networkAPI {
	t.Helper()
	n := newNetworkAPI(t)
	add := func(name, state, vendor string, memKB int64, labels map[string]string, gpus ...proto.GPUInfo) {
		t.Helper()
		if _, err := n.store.RegisterNode(name, store.HashToken(name), name); err != nil {
			t.Fatal(err)
		}
		if err := n.store.UpdateNodeStatus(name, proto.NodeStatus{Sys: proto.SysInfo{Hostname: name, Arch: "amd64", GPUVendor: vendor, MemTotalKB: memKB}, GPUs: gpus}); err != nil {
			t.Fatal(err)
		}
		if err := n.store.SetNodeState(name, state); err != nil {
			t.Fatal(err)
		}
		if labels != nil {
			if _, err := n.store.SetNodeLabels(name, labels); err != nil {
				t.Fatal(err)
			}
		}
	}
	gpu := func(index int, gib int64, healthy bool, vendor string) proto.GPUInfo {
		return proto.GPUInfo{Index: index, UUID: vendor + "-" + string(rune('0'+index)) + "-" + string(rune('a'+gib%26)), Name: "card", Vendor: vendor, ComputeCapability: "8.9", MemoryTotalBytes: gib << 30, Healthy: healthy}
	}
	add("node-a", store.NodeStateOnline, "nvidia", 64<<20, map[string]string{"pool": "inference", "rack": "r1"}, gpu(0, 24, true, "nvidia"), gpu(1, 48, false, "nvidia"))
	add("node-b", store.NodeStateOffline, "nvidia", 32<<20, map[string]string{"pool": "training"}, gpu(0, 80, true, "nvidia"))
	add("node-c", store.NodeStateOnline, "ascend", 0, map[string]string{"rack": "r2"}, gpu(0, 64, true, "ascend"))
	add("node-d", store.NodeStateConnecting, "", 0, nil)
	return n
}

func (n *networkAPI) memberGet(path string) []byte {
	n.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+n.member)
	rec := httptest.NewRecorder()
	n.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		n.t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func (n *networkAPI) nodePage(path string) dto.NodePage {
	n.t.Helper()
	var page dto.NodePage
	if err := json.Unmarshal(n.memberGet(path), &page); err != nil {
		n.t.Fatal(err)
	}
	return page
}

func nodePageNames(page dto.NodePage) []string {
	names := []string{}
	for _, item := range page.Items {
		names = append(names, item.Hostname)
	}
	sort.Strings(names)
	return names
}

func TestNodePageSlicesFiltersAndOffersEveryLabel(t *testing.T) {
	n := fleetFixture(t)

	first := n.nodePage("/api/v1/nodes?page=1&page_size=3")
	if first.Total != 4 || first.Page != 1 || first.PageSize != 3 || len(first.Items) != 3 || first.Items[0].Hostname != "node-d" {
		t.Fatalf("first page = total %d page %d size %d rows %v, want 4/1/3 newest first", first.Total, first.Page, first.PageSize, nodePageNames(first))
	}
	wantLabels := []dto.NodeLabelValues{{Key: "pool", Values: []string{"inference", "training"}}, {Key: "rack", Values: []string{"r1", "r2"}}}
	if !reflect.DeepEqual(first.Labels, wantLabels) {
		t.Fatalf("label facets = %+v, want %+v", first.Labels, wantLabels)
	}
	second := n.nodePage("/api/v1/nodes?page=2&page_size=3")
	if second.Total != 4 || len(second.Items) != 1 || second.Items[0].Hostname != "node-a" {
		t.Fatalf("second page = %v", nodePageNames(second))
	}
	if second.Items[0].GPUCount != 2 || len(second.Items[0].GPUs) != 2 || second.Items[0].Labels["pool"] != "inference" || len(second.Items[0].SystemLabels) == 0 {
		t.Fatalf("a page row lost list columns: %+v", second.Items[0])
	}

	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(n.memberGet("/api/v1/nodes?page=1"), &raw); err != nil {
		t.Fatal(err)
	}
	for _, item := range raw.Items {
		for _, heavy := range []string{"topology", "path_usage", "token"} {
			if _, ok := item[heavy]; ok {
				t.Errorf("a node page row carries %s", heavy)
			}
		}
	}

	for _, tc := range []struct {
		query string
		want  []string
	}{
		// Values of one key are alternatives; different keys must all match.
		{"label=pool%3Dinference&label=pool%3Dtraining", []string{"node-a", "node-b"}},
		{"label=pool%3Dinference&label=rack%3Dr1", []string{"node-a"}},
		{"label=pool%3Dtraining&label=rack%3Dr2", []string{}},
		{"state=online", []string{"node-a", "node-c"}},
		{"state=online,connecting", []string{"node-a", "node-c", "node-d"}},
		{"q=NODE-B", []string{"node-b"}},
		{"q=node&state=connecting", []string{"node-d"}},
		// System keys match the generated labels, as placement does.
		{"label=gpuworks.ai%2Fvendor%3Dnvidia", []string{"node-a", "node-b"}},
		{"label=gpuworks.ai%2Fvendor%3Dascend&label=rack%3Dr2", []string{"node-c"}},
	} {
		page := n.nodePage("/api/v1/nodes?page=1&" + tc.query)
		if got := nodePageNames(page); page.Total != len(tc.want) || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s matched %v (total %d), want %v", tc.query, got, page.Total, tc.want)
		}
		// Facets and All describe the fleet, not the filtered page.
		if page.All != 4 || !reflect.DeepEqual(page.Labels, wantLabels) {
			t.Errorf("%s: label facets = %+v", tc.query, page.Labels)
		}
	}

	var full []dto.Node
	if err := unmarshalList(n.memberGet("/api/v1/nodes"), &full); err != nil || len(full) != 4 {
		t.Fatalf("GET /nodes without paging = %d nodes err=%v, want the full array", len(full), err)
	}
}

// The label filter's edge cases, carried over from the client-side filter it
// replaced: a pair splits on its first '=', an empty value is a value, and a
// missing key never matches one. The filter is placement's own matcher.
func TestLabelFilterEdgeCases(t *testing.T) {
	parse := func(pairs ...string) nodelabel.Requirements {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
		q := req.URL.Query()
		for _, pair := range pairs {
			q.Add("label", pair)
		}
		req.URL.RawQuery = q.Encode()
		reqs, err := parseLabelFilter(req)
		if err != nil {
			t.Fatal(err)
		}
		return reqs
	}
	for _, tc := range []struct {
		labels string
		pairs  []string
		want   bool
	}{
		{`{"pool":"a"}`, nil, true},
		{`{}`, nil, true},
		{`{"empty":""}`, []string{"empty="}, true},
		{`{}`, []string{"empty="}, false},
		{`{"url":"a=b"}`, []string{"url=a=b"}, true},
		{`{"url":"a"}`, []string{"url=a=b"}, false},
		{`{"pool":"b","zone":"y"}`, []string{"pool=a", "pool=b"}, true},
		{`{"pool":"a","zone":"x"}`, []string{"pool=a", "zone=y"}, false},
	} {
		node := &store.Node{UUID: "n", Labels: tc.labels}
		if got := planning.MatchRequirements(node, planning.NodeFacts{}, parse(tc.pairs...)) == nil; got != tc.want {
			t.Errorf("labels %s with filter %v = %v, want %v", tc.labels, tc.pairs, got, tc.want)
		}
	}
}

func TestNodePageWireJSON(t *testing.T) {
	got, err := json.Marshal(dto.NodePage{ListPage: dto.ListPage[dto.Node]{Items: []dto.Node{}, Page: 1, PageSize: 50, Total: 0}, All: 3, Labels: []dto.NodeLabelValues{{Key: "pool", Values: []string{"a"}}}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"items":[],"page":1,"page_size":50,"total":0,"all":3,"labels":[{"key":"pool","values":["a"]}]}`
	if string(got) != want {
		t.Fatalf("wire JSON\n got: %s\nwant: %s", got, want)
	}
}

func TestDashboardSummaryCountsTheFleet(t *testing.T) {
	n := fleetFixture(t)
	model, err := n.store.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "org/m"})
	if err != nil {
		t.Fatal(err)
	}
	nodeA, err := n.store.NodeByUUID("node-a")
	if err != nil {
		t.Fatal(err)
	}
	deploy := func(name string, sleepable bool, instanceState string) {
		dep, err := n.store.CreateDeployment(&store.Deployment{Name: name, ModelID: model.ID, NodeID: nodeA.ID, Replicas: 1, Sleepable: sleepable})
		if err != nil {
			t.Fatal(err)
		}
		if instanceState != "" {
			if _, err := n.store.CreateInstance(&store.Instance{UUID: "inst-" + name, DeploymentID: dep.ID, NodeID: nodeA.ID, State: instanceState}); err != nil {
				t.Fatal(err)
			}
		}
	}
	deploy("serving", true, proto.InstRunning)
	deploy("booting", false, proto.InstStarting)
	deploy("pulling", false, proto.InstPulling)
	deploy("queued", true, "")

	var got dto.DashboardSummary
	if err := json.Unmarshal(n.memberGet("/api/v1/dashboard/summary"), &got); err != nil {
		t.Fatal(err)
	}
	wantNodes := dto.DashboardNodes{Total: 4, Online: 2, Offline: 1, Connecting: 1, GPUs: 4, UnusableGPUs: 1, VRAMGiB: 216, MemoryGiB: 96}
	if !reflect.DeepEqual(got.Nodes, wantNodes) {
		t.Errorf("nodes = %+v, want %+v", got.Nodes, wantNodes)
	}
	wantDeps := dto.DashboardDeployments{Total: 4, Running: 1, Warming: 2, Pending: 1, Sleepable: 2}
	if got.Deployments != wantDeps {
		t.Errorf("deployments = %+v, want %+v", got.Deployments, wantDeps)
	}
	wantVendor := []dto.CapacityGroup{
		{Key: "nvidia", Nodes: 2, GPUs: 3, VRAMGiB: 152, UnusableGPUs: 1},
		{Key: "ascend", Nodes: 1, GPUs: 1, VRAMGiB: 64},
		{Key: "unknown", Nodes: 1},
	}
	if !reflect.DeepEqual(got.ByVendor, wantVendor) {
		t.Errorf("by vendor = %+v, want %+v", got.ByVendor, wantVendor)
	}
	if !reflect.DeepEqual(got.LabelKeys, []string{"pool", "rack"}) || got.LabelKey != "pool" {
		t.Errorf("label keys = %v picked %q, want [pool rack] picking pool", got.LabelKeys, got.LabelKey)
	}
	wantPool := []dto.CapacityGroup{
		{Key: "pool=inference", Nodes: 1, GPUs: 2, VRAMGiB: 72, UnusableGPUs: 1},
		{Key: "", Unlabeled: true, Nodes: 2, GPUs: 1, VRAMGiB: 64},
		{Key: "pool=training", Nodes: 1, GPUs: 1, VRAMGiB: 80},
	}
	if !reflect.DeepEqual(got.ByLabel, wantPool) {
		t.Errorf("by pool = %+v, want %+v", got.ByLabel, wantPool)
	}

	var rack dto.DashboardSummary
	if err := json.Unmarshal(n.memberGet("/api/v1/dashboard/summary?label_key=rack"), &rack); err != nil {
		t.Fatal(err)
	}
	if rack.LabelKey != "rack" || len(rack.ByLabel) != 3 || rack.ByLabel[0].Key != "rack=r1" {
		t.Errorf("label_key=rack grouped %q into %+v", rack.LabelKey, rack.ByLabel)
	}
	var gone dto.DashboardSummary
	if err := json.Unmarshal(n.memberGet("/api/v1/dashboard/summary?label_key=removed"), &gone); err != nil {
		t.Fatal(err)
	}
	if gone.LabelKey != "pool" {
		t.Errorf("a key no node carries picked %q, want the most used key", gone.LabelKey)
	}
	if math.IsNaN(got.Nodes.VRAMGiB) {
		t.Fatal("vram is NaN")
	}
}

func TestDashboardSummaryGroupsMixedNodeCardsByTheirOwnVendor(t *testing.T) {
	node := store.Node{
		GPUCount:  2,
		GPUVendor: "nvidia",
		GPUs: []store.GPU{
			{Vendor: "nvidia", MemoryTotal: 24 << 30},
			{Vendor: "ascend", MemoryTotal: 64 << 30},
		},
	}
	got := summarizeNodes([]store.Node{node}, "")
	byVendor := make(map[string]dto.CapacityGroup, len(got.ByVendor))
	for _, row := range got.ByVendor {
		byVendor[row.Key] = row
	}
	if len(byVendor) != 2 || byVendor["nvidia"].GPUs != 1 || byVendor["nvidia"].VRAMGiB != 24 || byVendor["ascend"].GPUs != 1 || byVendor["ascend"].VRAMGiB != 64 {
		t.Fatalf("mixed node vendor capacity = %+v, want one card and its VRAM under each vendor", got.ByVendor)
	}
}

func TestDashboardSummaryOfAnEmptyFleet(t *testing.T) {
	n := newNetworkAPI(t)
	raw := n.memberGet("/api/v1/dashboard/summary")
	const want = `{"current_metrics":{"utilization_pct":null,"temperature_c":null,"power_watts":null,"memory_used_bytes":null,"vram_used_bytes":null,"vram_total_bytes":null},"nodes":{"total":0,"online":0,"offline":0,"connecting":0,"gpus":0,"unusable_gpus":0,"vram_gib":0,"memory_gib":0},"deployments":{"total":0,"running":0,"warming":0,"sleeping":0,"pending":0,"other":0,"sleepable":0},"cards":{"nodes":0,"online_nodes":0,"cards":0,"awake":0,"asleep":0,"free":0,"held_outside":0,"unhealthy":0,"offline":0,"contention":{"waiting_wakes":0,"held_requests":0,"yields_last_hour":0},"allocated":0},"tasks":{"batches":null,"benchmarks":0},"by_vendor":[],"label_keys":[],"label_key":"","by_label":[]}`
	if string(raw) != want+"\n" && string(raw) != want {
		t.Fatalf("empty summary\n got: %s\nwant: %s", raw, want)
	}
}

func (n *networkAPI) facets(query string) dto.NodeFacets {
	n.t.Helper()
	var out dto.NodeFacets
	if err := json.Unmarshal(n.memberGet("/api/v1/nodes/facets?"+query), &out); err != nil {
		n.t.Fatal(err)
	}
	return out
}

func TestNodeFacetsCountWhatTheMatchedNodesCarry(t *testing.T) {
	n := fleetFixture(t)

	all := n.facets("")
	if all.Total != 4 {
		t.Fatalf("total = %d, want 4", all.Total)
	}
	wantLabels := []dto.NodeLabelCounts{
		{Key: "pool", Values: []dto.ValueCount{{Value: "inference", Nodes: 1}, {Value: "training", Nodes: 1}}},
		{Key: "rack", Values: []dto.ValueCount{{Value: "r1", Nodes: 1}, {Value: "r2", Nodes: 1}}},
	}
	if !reflect.DeepEqual(all.Labels, wantLabels) {
		t.Errorf("labels = %+v, want %+v", all.Labels, wantLabels)
	}
	system := map[string]map[string]dto.SystemLabelValueCount{}
	for _, key := range all.SystemLabels {
		system[key.Key] = map[string]dto.SystemLabelValueCount{}
		for _, value := range key.Values {
			system[key.Key][value.Value] = value
		}
	}
	if _, ok := system[nodelabel.KeyNode]; ok {
		t.Error("the node identity is offered as a facet; every node has its own")
	}
	if nvidia := system[nodelabel.KeyVendor]["nvidia"]; nvidia.Nodes != 2 || nvidia.Display == "" || nvidia.DisplayCode != "common:vendor.nvidia" {
		t.Errorf("vendor nvidia facet = %+v, want 2 nodes with the server's display", nvidia)
	}
	if ascend := system[nodelabel.KeyVendor]["ascend"]; ascend.Nodes != 1 {
		t.Errorf("vendor ascend facet = %+v, want 1 node", ascend)
	}
	// node-a has two nvidia cards of one model: one node, not two.
	model := accelerator.NormalizeGPUName("card")
	// The nvidia chip has three cards (24, 48 and 80 GiB): the largest is its memory.
	wantGPUs := []dto.NodeGPUCount{{Vendor: "ascend", Name: "card", CC: "8.9", Model: model, Nodes: 1, Cards: 1, MemoryBytes: 64 << 30}, {Vendor: "nvidia", Name: "card", CC: "8.9", Model: model, Nodes: 2, Cards: 3, MemoryBytes: 80 << 30}}
	if !reflect.DeepEqual(all.GPUs, wantGPUs) {
		t.Errorf("gpus = %+v, want %+v", all.GPUs, wantGPUs)
	}

	// The filters are the paged list's: the facets describe the matches only.
	offline := n.facets("state=offline")
	if offline.Total != 1 || len(offline.Labels) != 1 || offline.Labels[0].Values[0] != (dto.ValueCount{Value: "training", Nodes: 1}) || len(offline.GPUs) != 1 {
		t.Errorf("state=offline facets = %+v, want node-b's alone", offline)
	}
	// A facet's model lists the nodes with that card.
	withCard := n.nodePage("/api/v1/nodes?page=1&label=gpuworks.ai%2Fgpu-model%3D" + model + "&label=gpuworks.ai%2Fvendor%3Dnvidia")
	if got := nodePageNames(withCard); !reflect.DeepEqual(got, []string{"node-a", "node-b"}) {
		t.Errorf("nodes with the nvidia %s = %v", model, got)
	}
	nvidia := n.facets("label=gpuworks.ai%2Fvendor%3Dnvidia")
	if nvidia.Total != 2 || len(nvidia.GPUs) != 1 || nvidia.GPUs[0].Vendor != "nvidia" {
		t.Errorf("vendor-filtered facets = %+v, want the two nvidia nodes", nvidia)
	}
	none := n.facets("label=pool%3Dnowhere")
	if none.Total != 0 || none.Labels == nil || none.SystemLabels == nil || none.GPUs == nil {
		t.Errorf("an empty match = %+v, want zero with empty (not null) lists", none)
	}
}

func TestNodeFacetsWireJSON(t *testing.T) {
	got, err := json.Marshal(dto.NodeFacets{
		Total:        2,
		Labels:       []dto.NodeLabelCounts{{Key: "pool", Values: []dto.ValueCount{{Value: "a", Nodes: 2}}}},
		SystemLabels: []dto.NodeSystemLabelCounts{{Key: "gpuworks.ai/vendor", Values: []dto.SystemLabelValueCount{{Value: "nvidia", Display: "NVIDIA", DisplayCode: "common:vendor.nvidia", Nodes: 2}}}},
		GPUs:         []dto.NodeGPUCount{{Vendor: "nvidia", Name: "NVIDIA A100", CC: "8.0", Model: "A100", Nodes: 2, Cards: 16, MemoryBytes: 80 << 30}},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"total":2,"labels":[{"key":"pool","values":[{"value":"a","nodes":2}]}],"system_labels":[{"key":"gpuworks.ai/vendor","values":[{"value":"nvidia","display":"NVIDIA","display_code":"common:vendor.nvidia","nodes":2}]}],"gpus":[{"vendor":"nvidia","name":"NVIDIA A100","cc":"8.0","model":"A100","nodes":2,"cards":16,"memory_bytes":85899345920}]}`
	if string(got) != want {
		t.Fatalf("wire JSON\n got: %s\nwant: %s", got, want)
	}
}

// A sleeping deployment serves nothing, so the fleet summary counts it as
// sleeping, not running, and every deployment lands in exactly one bucket.
func TestDashboardSummaryDoesNotCountSleepingDeploymentsAsRunning(t *testing.T) {
	n := fleetFixture(t)
	model, err := n.store.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "org/m"})
	if err != nil {
		t.Fatal(err)
	}
	nodeA, err := n.store.NodeByUUID("node-a")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string) uint {
		dep, err := n.store.CreateDeployment(&store.Deployment{Name: name, ModelID: model.ID, NodeID: nodeA.ID, Replicas: 1, Sleepable: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := n.store.CreateInstance(&store.Instance{UUID: "inst-" + name, DeploymentID: dep.ID, NodeID: nodeA.ID,
			State: proto.InstRunning, APIPort: 8000,
			DesiredStateJSON: `{"uuid":"inst-` + name + `","desired_state":"running","sleepable":true}`}); err != nil {
			t.Fatal(err)
		}
		return dep.ID
	}
	awake := mk("awake")
	asleep := mk("asleep")
	_ = awake
	if err := n.store.OrderDeploymentSleep(asleep, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	if err := n.store.UpdateInstanceStatus(proto.InstanceStatus{UUID: "inst-asleep", State: proto.InstRunning,
		SleepLevel: 1, Phase: proto.PhaseSleep}); err != nil {
		t.Fatal(err)
	}

	var got dto.DashboardSummary
	if err := json.Unmarshal(n.memberGet("/api/v1/dashboard/summary"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Deployments.Running != 1 {
		t.Errorf("running = %d, want 1: the asleep deployment serves nothing and must not count as running",
			got.Deployments.Running)
	}
	if got.Deployments.Warming != 0 {
		t.Errorf("warming = %d, want 0: a fully asleep deployment is not warming", got.Deployments.Warming)
	}
	if got.Deployments.Total != 2 {
		t.Errorf("total = %d, want 2: it is still a deployment", got.Deployments.Total)
	}
	if got.Deployments.Sleeping != 1 {
		t.Errorf("sleeping = %d, want 1: the asleep deployment is counted as sleeping", got.Deployments.Sleeping)
	}
	if d := got.Deployments; d.Other != 0 || d.Total != d.Running+d.Warming+d.Sleeping+d.Pending+d.Other {
		t.Errorf("deployments = %+v, want the buckets to add up to the total with nothing in other", d)
	}
}

// A stopped deployment is in no named bucket: it is counted as other, once,
// and not also as pending.
func TestDashboardSummaryCountsUnnamedStatesAsOther(t *testing.T) {
	n := fleetFixture(t)
	model, err := n.store.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "org/m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.store.CreateDeployment(&store.Deployment{Name: "queued", ModelID: model.ID, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.store.CreateDeployment(&store.Deployment{Name: "stopped", ModelID: model.ID, Replicas: 1, State: store.DeployStopped}); err != nil {
		t.Fatal(err)
	}
	var got dto.DashboardSummary
	if err := json.Unmarshal(n.memberGet("/api/v1/dashboard/summary"), &got); err != nil {
		t.Fatal(err)
	}
	want := dto.DashboardDeployments{Total: 2, Pending: 1, Other: 1}
	if got.Deployments != want {
		t.Errorf("deployments = %+v, want %+v", got.Deployments, want)
	}
	if got.Cards == nil || got.Cards.Allocated != got.Cards.Awake+got.Cards.Asleep {
		t.Errorf("cards = %+v, want allocated = awake + asleep", got.Cards)
	}
}

// The wire of a summary whose overview could not be built (cards null) and
// with deployments in every bucket.
func TestDashboardSummaryWireJSONWithUnknownCardsAndEveryBucket(t *testing.T) {
	got, err := json.Marshal(dto.DashboardSummary{
		Deployments: dto.DashboardDeployments{Total: 10, Running: 3, Warming: 2, Sleeping: 1, Pending: 2, Other: 2, Sleepable: 4},
		Tasks:       dto.DashboardTasks{Benchmarks: 1},
		ByVendor:    []dto.CapacityGroup{}, LabelKeys: []string{}, ByLabel: []dto.CapacityGroup{},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"current_metrics":{"utilization_pct":null,"temperature_c":null,"power_watts":null,"memory_used_bytes":null,"vram_used_bytes":null,"vram_total_bytes":null},"nodes":{"total":0,"online":0,"offline":0,"connecting":0,"gpus":0,"unusable_gpus":0,"vram_gib":0,"memory_gib":0},"deployments":{"total":10,"running":3,"warming":2,"sleeping":1,"pending":2,"other":2,"sleepable":4},"cards":null,"tasks":{"batches":null,"benchmarks":1},"by_vendor":[],"label_keys":[],"label_key":"","by_label":[]}`
	if string(got) != want {
		t.Fatalf("wire JSON\n got: %s\nwant: %s", got, want)
	}
	var back dto.DashboardSummary
	if err := json.Unmarshal(got, &back); err != nil || back.Cards != nil || back.Deployments.Other != 2 {
		t.Fatalf("round trip = %+v (%v)", back, err)
	}
	cards, err := json.Marshal(dto.DashboardCards{OverviewSummary: dto.OverviewSummary{Nodes: 2, OnlineNodes: 2, Cards: 8, Awake: 3, Asleep: 2, Free: 2, Unhealthy: 1}, Allocated: 5})
	if err != nil {
		t.Fatal(err)
	}
	const wantCards = `{"nodes":2,"online_nodes":2,"cards":8,"awake":3,"asleep":2,"free":2,"held_outside":0,"unhealthy":1,"offline":0,"contention":{"waiting_wakes":0,"held_requests":0,"yields_last_hour":0},"allocated":5}`
	if string(cards) != wantCards {
		t.Fatalf("cards wire JSON\n got: %s\nwant: %s", cards, wantCards)
	}
}
