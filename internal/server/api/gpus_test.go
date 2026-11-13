package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestGPUInventoryDerivesPhysicalAndVGPUAllocation(t *testing.T) {
	st, err := store.Open("file:gpu_inventory_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-gpu", store.HashToken("t"), "node-gpu")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "gpu3", GPUVendor: "nvidia", GPUDriver: "550.90"},
		GPUs: []proto.GPUInfo{{
			Index: 0, UUID: "GPU-a", Name: "NVIDIA H100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30,
			PowerLimitWatts: 700, Healthy: true, HealthStatus: "OK",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	node, _ = st.NodeWithGPUs(node.UUID)
	dep, err := st.CreateDeployment(&store.Deployment{Name: "qwen", NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "instance-running", Engine: "vllm", VRAMLimitBytes: 11 << 30, SMLimitPct: 50})
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "instance-running", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
		GPUIndexes: "[0]", DesiredStateJSON: string(spec),
	}); err != nil {
		t.Fatal(err)
	}
	stoppedSpec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "instance-stopped", Engine: "vllm", VRAMLimitBytes: 20 << 30})
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "instance-stopped", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstStopped,
		GPUIndexes: "[0]", DesiredStateJSON: string(stoppedSpec),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	h := apiHandlers{d: Deps{
		Store: st,
		MetricsLatest: func(metric string, _ map[string]string, _ int64, _ ...int64) ([]MetricLatest, error) {
			labels := map[string]string{"node": "node-gpu", "gpu": "0"}
			switch metric {
			case "gpuworks_gpu_utilization":
				return []MetricLatest{{Labels: labels, Value: 72, TimeMS: now}}, nil
			case "gpuworks_dgpu_memory_used_bytes":
				return []MetricLatest{{Labels: map[string]string{"node": "node-gpu", "gpu": "0", "instance": "instance-running"}, Value: 7 << 30, TimeMS: now}}, nil
			default:
				return nil, nil
			}
		},
	}}
	inventory, err := h.buildGPUInventory()
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.physical.Items) != 1 {
		t.Fatalf("physical = %+v", inventory.physical.Items)
	}
	physical := inventory.physical.Items[0]
	if physical.PowerLimitWatts != 700 || physical.AllocationState != "partial" || physical.Load.UtilizationPct == nil || *physical.Load.UtilizationPct != 72 {
		t.Fatalf("physical = %+v", physical)
	}
	if len(physical.Occupants) != 1 || physical.Occupants[0].InstanceUUID != "instance-running" {
		t.Fatalf("occupants = %+v", physical.Occupants)
	}
	if physical.AllocatedBytes <= 11<<30 || physical.AvailableBytes >= 69<<30 {
		t.Fatalf("allocation = %d available = %d", physical.AllocatedBytes, physical.AvailableBytes)
	}
	if len(inventory.dgpu.Items) != 1 || inventory.dgpu.Items[0].MemoryUsed == nil || *inventory.dgpu.Items[0].MemoryUsed != 7<<30 {
		t.Fatalf("vgpus = %+v", inventory.dgpu.Items)
	}
	if inventory.physical.Overview.Total != 1 || inventory.physical.Overview.Health.OK != 1 || inventory.physical.Overview.Allocation.Allocated != 0 || inventory.physical.Overview.Allocation.Allocatable != 1 {
		t.Fatalf("overview = %+v", inventory.physical.Overview)
	}
	if physical.AllocationState != "partial" {
		t.Fatalf("allocation state = %q, want partial: the card is in use with room left", physical.AllocationState)
	}

	// A node nobody can reach says so, rather than offering its cards'
	// last reading as what is on them now. The cards' health is still
	// what the last report found: that is a fact about the card, not
	// about the session.
	if err := st.SetNodeState(node.UUID, store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	inventory, err = h.buildGPUInventory()
	if err != nil {
		t.Fatal(err)
	}
	if inventory.physical.Items[0].AllocationState != "unknown" {
		t.Fatalf("offline inventory = %+v", inventory.physical.Items[0])
	}
	if inventory.physical.Overview.Health.OK != 1 {
		t.Fatalf("offline overview = %+v", inventory.physical.Overview)
	}
}

func TestGPUInventoryCachesSnapshotForTwentySeconds(t *testing.T) {
	st, err := store.Open("file:gpu_inventory_cache_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := apiHandlers{d: Deps{Store: st, MetricsLatest: func(string, map[string]string, int64, ...int64) ([]MetricLatest, error) {
		calls++
		return nil, nil
	}}}
	if _, err := h.currentGPUInventory(); err != nil {
		t.Fatal(err)
	}
	firstCalls := calls
	if _, err := h.currentGPUInventory(); err != nil {
		t.Fatal(err)
	}
	if calls != firstCalls {
		t.Fatalf("cache miss: calls %d -> %d", firstCalls, calls)
	}
	h.gpuCacheAt = time.Now().Add(-11 * time.Second)
	if _, err := h.currentGPUInventory(); err != nil {
		t.Fatal(err)
	}
	if calls != firstCalls {
		t.Fatalf("20-second cache expired after 11 seconds: calls %d -> %d", firstCalls, calls)
	}
	h.gpuCacheAt = time.Now().Add(-21 * time.Second)
	if _, err := h.currentGPUInventory(); err != nil {
		t.Fatal(err)
	}
	if calls <= firstCalls {
		t.Fatalf("expired cache was reused: calls = %d", calls)
	}
}

// A card that takes no work must not read as available, whatever the
// reason. Only health used to be able to say this, so a card lent to a
// virtual machine, or one held by somebody's own process, was offered in
// the card list as free.
func TestCardsThatTakeNoWorkAreNotShownAvailable(t *testing.T) {
	st, err := store.Open("file:gpu_unusable_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("node-unusable", store.HashToken("t"), "node-unusable")
	if err != nil {
		t.Fatal(err)
	}
	ok := true
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "h", GPUVendor: "nvidia"}, GPUProbeOK: &ok,
		GPUs: []proto.GPUInfo{
			{Index: 0, UUID: "GPU-free", Name: "NVIDIA H100", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 80 << 30, Healthy: true, HealthStatus: "OK"},
			{Index: 1, UUID: "GPU-busy", Name: "NVIDIA H100", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", MemoryTotalBytes: 80 << 30, Healthy: true, HealthStatus: "OK",
				ForeignProcesses: []proto.CardProcess{{PID: 4242, Name: "python3", MemoryBytes: 70 << 30}}},
			{Index: -1, Name: "NVIDIA H100", Vendor: "nvidia", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
			{Index: 2, UUID: "GPU-broken", Name: "NVIDIA H100", Vendor: "nvidia", PCIeBDF: "0000:41:00.0", MemoryTotalBytes: 80 << 30, Healthy: false, HealthStatus: "Error", HealthCode: accelerator.HealthMemoryRemapFailed},
		},
	}); err != nil {
		t.Fatal(err)
	}

	h := apiHandlers{d: Deps{Store: st}}
	inventory, err := h.buildGPUInventory()
	if err != nil {
		t.Fatal(err)
	}
	byUUID := map[string]string{}
	byBDF := map[string]string{}
	for _, item := range inventory.physical.Items {
		byUUID[item.UUID] = item.AllocationState
		byBDF[item.PCIeBDF] = item.AllocationState
	}
	if got := byUUID["GPU-free"]; got != "available" {
		t.Fatalf("the free card = %q, want available", got)
	}
	for _, tc := range []struct{ key, why string }{
		{"GPU-busy", "held by a process outside the platform"},
		{"GPU-broken", "faulty"},
	} {
		if got := byUUID[tc.key]; got != "unusable" {
			t.Errorf("a card %s = %q, want unusable", tc.why, got)
		}
	}
	// A card owned elsewhere IS in this view: it is hardware somebody has
	// to be able to find, and leaving it out of the one page called "GPU"
	// sent operators looking for cards the platform could see all along.
	// It is marked unusable and its ownership says why; it still does not
	// count towards anybody's capacity.
	if got, found := byBDF["0000:1b:00.0"]; !found || got != "unusable" {
		t.Errorf("a passed-through card = %q found=%v, want it listed as unusable", got, found)
	}
}

// A slice is listed from the moment it holds its quota on the card, not only
// once its engine answers: a starting slice already shows on the card as
// allocated, and leaving it off the dGPU list for the minutes it spends
// starting read as "partitioned, but nothing to see". A stopped one holds
// nothing and stays off.
func TestDGPUListsSlicesFromTheMomentTheyHoldTheirQuota(t *testing.T) {
	st, err := store.Open("file:dgpu_slice_states_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("node-slices", store.HashToken("t"), "node-slices")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "slices", GPUVendor: "nvidia"},
		GPUs: []proto.GPUInfo{{Index: 0, UUID: "GPU-s", Name: "RTX 2080 Ti", Vendor: "nvidia", MemoryTotalBytes: 22 << 30, Healthy: true, HealthStatus: "OK"}},
	}); err != nil {
		t.Fatal(err)
	}
	node, _ = st.NodeWithGPUs(node.UUID)
	dep, err := st.CreateDeployment(&store.Deployment{Name: "slices", NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	for _, instance := range []struct {
		uuid, state string
		sleepLevel  int
	}{
		{"slice-starting", proto.InstStarting, 0},
		{"slice-running", proto.InstRunning, 0},
		{"slice-asleep", proto.InstRunning, 1},
		{"slice-stopped", proto.InstStopped, 0},
	} {
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: instance.uuid, Engine: "vllm", VRAMLimitBytes: 4 << 30, PartitionBackend: accelerator.BackendNVIDIALogical})
		if _, err := st.CreateInstance(&store.Instance{UUID: instance.uuid, DeploymentID: dep.ID, NodeID: node.ID, State: instance.state,
			SleepLevel: instance.sleepLevel, GPUIndexes: "[0]", DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
	}
	h := apiHandlers{d: Deps{Store: st}}
	inventory, err := h.buildGPUInventory()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, row := range inventory.dgpu.Items {
		got[row.InstanceUUID] = row.State
		if row.InstanceUUID == "slice-asleep" && row.SleepLevel != 1 {
			t.Fatalf("asleep row = %+v, want its sleep level", row)
		}
		if row.DeploymentName != "slices" || row.GPUName != "RTX 2080 Ti" || row.MemoryLimit != 4<<30 {
			t.Fatalf("row = %+v", row)
		}
	}
	want := map[string]string{"slice-starting": proto.InstStarting, "slice-running": proto.InstRunning, "slice-asleep": proto.InstRunning}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for uuid, state := range want {
		if got[uuid] != state {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}
}

// An instance or deployment change makes the next read rebuild once the refresh floor
// has passed, instead of answering the page's event-driven refresh with the
// card as it was before the change for up to twenty seconds. Without a change
// the snapshot is kept for its full lifetime.
func TestGPUInventoryRebuildsAfterAnInstanceChange(t *testing.T) {
	events := bus.New()
	st, err := store.Open("file:gpu_inventory_generation_test?mode=memory&cache=shared", events)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	calls := 0
	h := apiHandlers{d: Deps{Store: st, Bus: events, MetricsLatest: func(string, map[string]string, int64, ...int64) ([]MetricLatest, error) {
		calls++
		return nil, nil
	}}}
	read := func() int {
		t.Helper()
		before := calls
		if _, err := h.currentGPUInventory(); err != nil {
			t.Fatal(err)
		}
		return calls - before
	}
	if read() == 0 {
		t.Fatal("the first read built nothing")
	}
	h.gpuCacheAt = time.Now().Add(-5 * time.Second)
	if read() != 0 {
		t.Fatal("an unchanged snapshot was rebuilt before its twenty seconds")
	}
	events.PublishChange("instance", "i1", bus.ReasonProgress)
	if read() != 0 {
		t.Fatal("a progress notice rebuilt the snapshot")
	}
	events.PublishChange("instance", "i1", "state:running")
	h.gpuCacheAt = time.Now().Add(-1 * time.Second)
	if read() != 0 {
		t.Fatal("a change rebuilt the snapshot inside the refresh floor")
	}
	h.gpuCacheAt = time.Now().Add(-3 * time.Second)
	if read() == 0 {
		t.Fatal("a change was not picked up after the refresh floor")
	}
	h.gpuCacheAt = time.Now().Add(-3 * time.Second)
	if read() != 0 {
		t.Fatal("the same change rebuilt the snapshot twice")
	}
	// A deployment change (here its dGPU limit) is shown on its slices too.
	events.PublishChange("deployment", "1", "dgpu_quota")
	h.gpuCacheAt = time.Now().Add(-3 * time.Second)
	if read() == 0 {
		t.Fatal("a deployment change was not picked up after the refresh floor")
	}
}

// TestGPUInventoryMatchesUIFixtures pins the GPU page's two answers - one page
// of cards and the dGPU list - from the real handlers, for the GPU page's
// tests to render: a whole card, a card carved into slices (two running, one
// starting), and a card passed through to a virtual machine. Regenerate with
// GPUWORKS_UPDATE_UI_FIXTURES=1.
func TestGPUInventoryMatchesUIFixtures(t *testing.T) {
	const dir = "../../../ui/tests/fixtures/gpu-inventory"
	st, err := store.Open("file:gpu_inventory_ui_fixture?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("node-fixture", store.HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "gpu3", GPUVendor: "nvidia", GPUDriver: "580.126.09", Arch: "amd64", Framework: "cuda", FrameworkVersion: "13.0", RuntimeDriver: "docker",
			PartitionAssets: []proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}}, PartitionSafetyProtocol: 1},
		GPUs: []proto.GPUInfo{
			{Index: 0, UUID: "GPU-quadro", Name: "Quadro RTX 8000", Vendor: "nvidia", ArchID: "sm_75", ComputeCapability: "7.5", MemoryTotalBytes: 45 << 30, PowerLimitWatts: 250, Healthy: true, HealthStatus: "OK", PCIeBDF: "0000:17:00.0"},
			{Index: 1, UUID: "GPU-2080ti", Name: "GeForce RTX 2080 Ti", Vendor: "nvidia", ArchID: "sm_75", ComputeCapability: "7.5", MemoryTotalBytes: 22 << 30, PowerLimitWatts: 250, Healthy: true, HealthStatus: "OK", PCIeBDF: "0000:65:00.0"},
			{Index: -1, Name: "GeForce RTX 3090", Vendor: "nvidia", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
		},
	}); err != nil {
		t.Fatal(err)
	}
	node, _ = st.NodeWithGPUs(node.UUID)
	instance := func(name, uuid, state string, gpu int, limit, deploymentLimit int64) {
		t.Helper()
		dep, err := st.CreateDeployment(&store.Deployment{Name: name, NodeID: node.ID, State: "active", VRAMLimitBytes: deploymentLimit})
		if err != nil {
			t.Fatal(err)
		}
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, Engine: "vllm", VRAMLimitBytes: limit, PartitionBackend: map[bool]string{true: accelerator.BackendNVIDIALogical}[limit > 0]})
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: state,
			GPUIndexes: fmt.Sprintf("[%d]", gpu), DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
	}
	instance("qwen3-fullcard", "inst-whole", proto.InstRunning, 0, 0, 0)
	instance("qwen3-dgpu-a", "inst-slice-a", proto.InstRunning, 1, 8<<30, 8<<30)
	// Lowered to 6 GiB while the slice held more: it keeps 8 until it restarts.
	instance("qwen3-dgpu-b", "inst-slice-b", proto.InstRunning, 1, 8<<30, 6<<30)
	instance("qwen3-dgpu-c", "inst-slice-c", proto.InstStarting, 1, 4<<30, 4<<30)
	const at = int64(1792302000000)
	h := &apiHandlers{d: Deps{Store: st, MetricsLatest: func(metric string, _ map[string]string, _ int64, _ ...int64) ([]MetricLatest, error) {
		card := func(gpu string, value float64) MetricLatest {
			return MetricLatest{Labels: map[string]string{"node": "node-fixture", "gpu": gpu}, Value: value, TimeMS: at}
		}
		slice := func(instance string, value float64) MetricLatest {
			return MetricLatest{Labels: map[string]string{"node": "node-fixture", "gpu": "1", "instance": instance}, Value: value, TimeMS: at}
		}
		switch metric {
		case "gpuworks_gpu_utilization":
			return []MetricLatest{card("0", 0), card("1", 100)}, nil
		case "gpuworks_gpu_memory_used_bytes":
			return []MetricLatest{card("0", 40775<<20), card("1", 15021<<20)}, nil
		case "gpuworks_dgpu_memory_used_bytes":
			return []MetricLatest{slice("inst-slice-a", 7192<<20), slice("inst-slice-b", 7064<<20)}, nil
		case "gpuworks_dgpu_sm_util":
			return []MetricLatest{slice("inst-slice-a", 99), slice("inst-slice-b", 0)}, nil
		}
		return nil, nil
	}}}
	update := os.Getenv(evaluateFixturesEnv) != ""
	rec := httptest.NewRecorder()
	h.listGPUs(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpus?page=1&page_size=20", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("gpus status = %d body = %s", rec.Code, rec.Body.String())
	}
	checkUIFixture(t, dir, "gpus-page", rec.Body.Bytes(), update)
	rec = httptest.NewRecorder()
	h.listDGPUs(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dgpus", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dgpus status = %d body = %s", rec.Code, rec.Body.String())
	}
	checkUIFixture(t, dir, "dgpus", rec.Body.Bytes(), update)
	if update {
		t.Fatalf("regenerated fixtures under %s; rerun without %s", dir, evaluateFixturesEnv)
	}
}
