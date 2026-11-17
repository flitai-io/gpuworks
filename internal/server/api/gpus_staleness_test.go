package api

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A change to a node or a card - its card reporting an error, an operator
// stopping the card, the node going offline - is on the GPU page past the
// refresh floor, as an instance or deployment change is, not up to the
// twenty-second cache later.
func TestGPUInventoryRebuildsAfterANodeOrCardChange(t *testing.T) {
	events := bus.New()
	st, err := store.Open("file:gpu_inventory_node_change?mode=memory&cache=shared", events)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gpu-node", store.HashToken("gpu-node"), "gpu-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	report := func(health string) {
		t.Helper()
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: "gpu-node", GPUVendor: "nvidia"},
			GPUs: []proto.GPUInfo{{Index: 0, UUID: "GPU-stale", Name: "NVIDIA A100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: health == "OK", HealthStatus: health}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	report("OK")
	h := apiHandlers{d: Deps{Store: st, Bus: events}}
	card := func() (health string, stopped bool, nodeState string) {
		t.Helper()
		inv, err := h.currentGPUInventory()
		if err != nil {
			t.Fatal(err)
		}
		if len(inv.physical.Items) != 1 {
			t.Fatalf("cards = %+v", inv.physical.Items)
		}
		c := inv.physical.Items[0]
		return c.HealthStatus, c.SchedulingStopped, c.NodeState
	}
	if health, _, _ := card(); health != "OK" {
		t.Fatalf("first read health = %q", health)
	}
	pastFloor := func() { h.gpuCacheAt = time.Now().Add(-gpuInventoryRefreshFloor - time.Second) }

	report("Error")
	pastFloor()
	if health, _, _ := card(); health != "Error" {
		t.Errorf("card reported Error; past the refresh floor the page shows %q", health)
	}
	stored, err := st.NodeWithGPUs(node.UUID)
	if err != nil || len(stored.GPUs) != 1 {
		t.Fatalf("node cards = %+v %v", stored, err)
	}
	if _, err := st.SetCardScheduling(node.UUID, stored.GPUs[0].Ident, store.CardSchedulingStopped); err != nil {
		t.Fatal(err)
	}
	pastFloor()
	if _, stopped, _ := card(); !stopped {
		t.Error("card stopped by an operator; past the refresh floor the page does not show it stopped")
	}
	if err := st.SetNodeState(node.UUID, store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	pastFloor()
	if _, _, state := card(); state != store.NodeStateOffline {
		t.Errorf("node went offline; past the refresh floor the page shows its state %q", state)
	}
}

// A rebuild charges the whole fleet in one pass: the statements it runs do
// not grow with the nodes and instances, now that a node report can rebuild
// it once per refresh floor.
func TestGPUInventoryRebuildReadsAFixedNumberOfStatements(t *testing.T) {
	measure := func(nodes int) int64 {
		t.Helper()
		st, err := store.Open("file:gpu_inventory_statements_"+strconv.Itoa(nodes)+"?mode=memory&cache=shared", bus.New())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/gpus-" + strconv.Itoa(nodes), Revision: "main", Name: "m"})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < nodes; i++ {
			uuid := fmt.Sprintf("stmt-%d-%d", nodes, i)
			node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
				Sys:  proto.SysInfo{Hostname: uuid, GPUVendor: "nvidia"},
				GPUs: []proto.GPUInfo{{Index: 0, UUID: "GPU-" + uuid, Name: "NVIDIA A100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true}},
			}); err != nil {
				t.Fatal(err)
			}
			dep, err := st.CreateDeployment(&store.Deployment{Name: uuid, ServedName: uuid, ModelID: model.ID, NodeID: node.ID, Engine: "vllm", Replicas: 1, State: store.DeployActive})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateInstance(&store.Instance{UUID: uuid + "-inst", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]",
				DesiredStateJSON: `{"vram_limit_bytes":8589934592}`}); err != nil {
				t.Fatal(err)
			}
		}
		h := apiHandlers{d: Deps{Store: st}}
		var n atomic.Int64
		count := func(*gorm.DB) { n.Add(1) }
		if err := st.DB().Callback().Query().After("gorm:query").Register("test:gpu_statements", count); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Callback().Row().After("gorm:row").Register("test:gpu_statement_rows", count); err != nil {
			t.Fatal(err)
		}
		inv, err := h.currentGPUInventory()
		if err != nil {
			t.Fatal(err)
		}
		if len(inv.physical.Items) != nodes || len(inv.dgpu.Items) != nodes {
			t.Fatalf("%d nodes: %d cards, %d slices", nodes, len(inv.physical.Items), len(inv.dgpu.Items))
		}
		return n.Load()
	}
	small, large := measure(3), measure(15)
	if large != small {
		t.Fatalf("a rebuild ran %d statements for 3 nodes and %d for 15: it reads per node or per deployment", small, large)
	}
}

// As the server runs it, the GPU page reads the shared node snapshot. A card
// reporting a fault inside the snapshot's report window - another reader
// rebuilt it a moment ago - is still on the page past the refresh floor: a
// rebuild that took the snapshot's older rows would keep showing the card OK
// until the cache's twenty-second limit, with no event left to rebuild it.
func TestGPUInventoryOnTheNodeSnapshotShowsACardFaultPastTheFloor(t *testing.T) {
	events := bus.New()
	st, err := store.Open("file:gpu_inventory_fleet_snapshot?mode=memory&cache=shared", events)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gpu-snap", store.HashToken("gpu-snap"), "gpu-snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	report := func(health string) {
		t.Helper()
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: "gpu-snap", GPUVendor: "nvidia"},
			GPUs: []proto.GPUInfo{{Index: 0, UUID: "GPU-snap", Name: "NVIDIA A100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: health == "OK", HealthStatus: health}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	report("OK")
	// The snapshot's clock stands still: every read below is inside its
	// report window.
	frozen := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	st.EnableFleetNodeSnapshot(func() time.Time { return frozen })
	h := apiHandlers{d: Deps{Store: st, Bus: events}}
	health := func() string {
		t.Helper()
		inv, err := h.currentGPUInventory()
		if err != nil {
			t.Fatal(err)
		}
		if len(inv.physical.Items) != 1 {
			t.Fatalf("cards = %+v", inv.physical.Items)
		}
		return inv.physical.Items[0].HealthStatus
	}
	if got := health(); got != "OK" {
		t.Fatalf("first read health = %q", got)
	}
	report("Error")
	h.gpuCacheAt = time.Now().Add(-gpuInventoryRefreshFloor - time.Second)
	if got := health(); got != "Error" {
		t.Errorf("card reported Error inside the snapshot's window; past the refresh floor the page shows %q", got)
	}
}
