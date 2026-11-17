package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Replay the issue's 71-card distribution through the real inventory and HTTP
// contract: 41 free, 2 partial, 5 fully allocated, and 23 externally occupied.
func TestGPUAllocationCountsPartitionFleet(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	node, err := rig.st.RegisterNode("allocation-counts", store.HashToken("allocation-counts"), "allocation-counts")
	if err != nil {
		t.Fatal(err)
	}
	cards := make([]proto.GPUInfo, 71)
	for i := range cards {
		cards[i] = proto.GPUInfo{Index: i, UUID: fmt.Sprintf("GPU-count-%d", i), Name: "H100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true, HealthStatus: "OK"}
		if i >= 7 && i < 30 {
			cards[i].ForeignProcesses = []proto.CardProcess{{PID: 4242, Name: "external", MemoryBytes: 1 << 30}}
		}
	}
	if err := rig.st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUs: cards}); err != nil {
		t.Fatal(err)
	}
	dep, err := rig.st.CreateDeployment(&store.Deployment{Name: "allocation-counts", NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		spec := proto.InstanceDesiredState{UUID: fmt.Sprintf("instance-count-%d", i), Engine: "vllm"}
		if i >= 5 {
			spec.VRAMLimitBytes = 16 << 30
		}
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rig.st.CreateInstance(&store.Instance{UUID: spec.UUID, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: fmt.Sprintf("[%d]", i), DesiredStateJSON: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/api/v1/gpus", "/api/v1/gpus?page=1&page_size=20&allocation_state=partial"} {
		rec := rig.do(rig.session, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		var response dto.GPUListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Overview struct {
				Allocation json.RawMessage `json:"allocation"`
			} `json:"overview"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		want := `{"allocated":5,"allocatable":43,"unusable":23,"unknown":0}`
		if string(wire.Overview.Allocation) != want {
			t.Errorf("%s allocation=%s want %s", path, wire.Overview.Allocation, want)
		}
		counts := response.Overview.Allocation
		if counts.Allocated+counts.Allocatable+counts.Unusable+counts.Unknown != response.Overview.Total {
			t.Errorf("counts %+v do not partition total %d", counts, response.Overview.Total)
		}
		for _, gpu := range response.Items {
			if gpu.AllocationState == "partial" && (gpu.AllocatedBytes <= 0 || gpu.AvailableBytes <= 0) {
				t.Errorf("partial card lost actual capacity: %+v", gpu)
			}
		}
	}
}

func TestGPUAllocationOverviewUsesExclusiveClassification(t *testing.T) {
	for _, tc := range []struct {
		name, nodeState, ownership string
		foreign                    bool
		awake, sleeping            int64
		state                      string
		want                       dto.GPUAllocationOverview
	}{
		{name: "partial dGPU", nodeState: store.NodeStateOnline, awake: 16 << 30, state: "partial", want: dto.GPUAllocationOverview{Allocatable: 1}},
		{name: "full reservation", nodeState: store.NodeStateOnline, awake: 80 << 30, state: "allocated", want: dto.GPUAllocationOverview{Allocated: 1}},
		{name: "sleeping residue with capacity", nodeState: store.NodeStateOnline, sleeping: 1 << 30, state: "partial", want: dto.GPUAllocationOverview{Allocatable: 1}},
		{name: "sleeping residue fills device", nodeState: store.NodeStateOnline, sleeping: 80 << 30, state: "allocated", want: dto.GPUAllocationOverview{Allocated: 1}},
		{name: "offline reserved", nodeState: store.NodeStateOffline, awake: 16 << 30, state: "unknown", want: dto.GPUAllocationOverview{Unknown: 1}},
		{name: "connecting reserved", nodeState: store.NodeStateConnecting, awake: 16 << 30, state: "unknown", want: dto.GPUAllocationOverview{Unknown: 1}},
		{name: "externally occupied with reservation", nodeState: store.NodeStateOnline, foreign: true, awake: 16 << 30, state: "unusable", want: dto.GPUAllocationOverview{Unusable: 1}},
		{name: "passed through with stale reservation", nodeState: store.NodeStateOnline, ownership: accelerator.CardPassedThrough, awake: 16 << 30, state: "unusable", want: dto.GPUAllocationOverview{Unusable: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := store.Node{State: tc.nodeState}
			gpu := store.GPU{MemoryTotal: 80 << 30, HealthStatus: "OK", Ownership: tc.ownership}
			if tc.foreign {
				raw, _ := json.Marshal([]proto.CardProcess{{PID: 4242, Name: "external", MemoryBytes: 1 << 30}})
				gpu.ForeignProcesses = string(raw)
			}
			item := physicalGPU(&node, partitioncap.Reports{}, &gpu, planning.GPUAllocation{AllocatedBytes: tc.awake, SleepingBytes: tc.sleeping}, func(uint) *store.Deployment { return nil }, nil)
			if item.AllocationState != tc.state {
				t.Fatalf("state=%s want %s", item.AllocationState, tc.state)
			}
			overview := dto.GPUOverview{}
			accumulateGPUOverview(&overview, &gpu, &item)
			if overview.Allocation != tc.want || overview.Total != 1 {
				t.Fatalf("overview=%+v want counts %+v", overview, tc.want)
			}
		})
	}
}

// A card shared with processes outside the platform (ADR 0068) shows as free
// only what a partition could still be admitted against: the foreign memory
// and the headroom kept beside it are not free.
func TestGPUAvailableBytesLeavesOutForeignMemory(t *testing.T) {
	node := store.Node{State: store.NodeStateOnline, PartitionSafetyProtocol: proto.PartitionSafetyForeignTracked}
	raw, _ := json.Marshal([]proto.CardProcess{{PID: 41235, Name: "python3", MemoryBytes: 62 << 30}})
	gpu := store.GPU{MemoryTotal: 140 << 30, HealthStatus: "OK", ForeignProcesses: string(raw)}
	item := physicalGPU(&node, partitioncap.Reports{}, &gpu, planning.GPUAllocation{AllocatedBytes: 50 << 30}, func(uint) *store.Deployment { return nil }, nil)
	if want := int64(140-62-7-50) << 30; item.AvailableBytes != want {
		t.Fatalf("available = %d, want %d (total less foreign, headroom and claims)", item.AvailableBytes, want)
	}
}

// A held card whose foreign memory is not known shows nothing free: what the
// outside processes hold is not zero just because it cannot be read, and no
// partition is admitted onto such a card.
func TestGPUAvailableBytesIsZeroOnAHeldCardWhoseForeignMemoryIsUnknown(t *testing.T) {
	node := store.Node{State: store.NodeStateOnline, PartitionSafetyProtocol: proto.PartitionSafetyForeignTracked}
	for _, holder := range []proto.CardProcess{{Name: "python3", MemoryBytes: 60 << 30}, {PID: 41235, Name: "python3"}} {
		raw, _ := json.Marshal([]proto.CardProcess{holder})
		gpu := store.GPU{MemoryTotal: 140 << 30, HealthStatus: "OK", ForeignProcesses: string(raw)}
		item := physicalGPU(&node, partitioncap.Reports{}, &gpu, planning.GPUAllocation{AllocatedBytes: 16 << 30}, func(uint) *store.Deployment { return nil }, nil)
		if item.AvailableBytes != 0 {
			t.Fatalf("holder %+v: available = %d, want 0 on a held card whose foreign memory is unknown", holder, item.AvailableBytes)
		}
	}
}
