package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestGPUCapacityFactMatchesNodeAndFleetWire(t *testing.T) {
	for _, vendor := range []string{"amd", "nvidia", "ascend"} {
		for _, total := range []int64{0, 24 << 30} {
			t.Run(fmt.Sprintf("%s/%d", vendor, total), func(t *testing.T) {
				st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = st.Close() })
				node, err := st.RegisterNode("capacity-node", store.HashToken("capacity-test"), "capacity")
				if err != nil {
					t.Fatal(err)
				}
				err = st.UpdateNodeStatus(node.UUID, proto.NodeStatus{Sys: proto.SysInfo{GPUVendor: vendor}, GPUs: []proto.GPUInfo{{Index: 0, UUID: "capacity-gpu", Vendor: vendor, Name: "test", Healthy: true, MemoryTotalBytes: total}}})
				if err != nil {
					t.Fatal(err)
				}
				node, err = st.NodeWithGPUs(node.UUID)
				if err != nil {
					t.Fatal(err)
				}
				h := apiHandlers{d: Deps{Store: st}}
				inventory, err := h.buildGPUInventory()
				if err != nil {
					t.Fatal(err)
				}
				if len(inventory.physical.Items) != 1 || len(node.GPUs) != 1 {
					t.Fatal("missing card")
				}
				for _, view := range []any{nodeDTO(node, false).GPUs[0], inventory.physical.Items[0]} {
					data, err := json.Marshal(view)
					if err != nil {
						t.Fatal(err)
					}
					var wire map[string]json.RawMessage
					if err = json.Unmarshal(data, &wire); err != nil {
						t.Fatal(err)
					}
					if got := string(wire["memory_capacity_known"]); got != fmt.Sprint(total > 0) {
						t.Fatalf("capacity flag = %s", got)
					}
					if got := string(wire["memory_total_bytes"]); got != fmt.Sprint(total) {
						t.Fatalf("capacity = %s", got)
					}
				}
			})
		}
	}
}
