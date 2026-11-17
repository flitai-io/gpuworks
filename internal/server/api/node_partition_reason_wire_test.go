package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An unavailable dGPU capability reaches the node and GPU DTOs with its code
// and typed params next to the English fallback, under exactly these keys.
func TestNodeDTOPartitionReasonWire(t *testing.T) {
	n := &store.Node{
		UUID: "node-1", GPUVendor: "nvidia", RuntimeDriver: "docker", Runtimes: `["docker"]`,
		PartitionAssets: `[{"backend":"nvidia-dgpu","root":"/opt/dgpu","files":[]}]`, PartitionSafetyProtocol: 1,
		GPUs: []store.GPU{{Index: 0, UUID: "gpu-1", Vendor: "nvidia", ArchID: "sm_90", MemoryTotal: 80 << 30}},
	}
	d := toNodeDTO(n)
	if len(d.PartitionCapabilities) != 1 || len(d.GPUs) != 1 || len(d.GPUs[0].PartitionCapabilities) != 1 {
		t.Fatalf("capabilities = %+v / %+v", d.PartitionCapabilities, d.GPUs)
	}
	for _, capability := range []proto.PartitionCapability{d.PartitionCapabilities[0], d.GPUs[0].PartitionCapabilities[0]} {
		raw, err := json.Marshal(capability)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if wire["status"] != "unavailable" || wire["reason_code"] != "PARTITION_ASSETS_MISSING" || wire["reason"] != "missing dGPU assets: libvgpu.so" {
			t.Fatalf("wire = %s", raw)
		}
		if !reflect.DeepEqual(wire["reason_params"], map[string]any{"missing_assets": []any{"libvgpu.so"}}) {
			t.Fatalf("reason_params = %#v", wire["reason_params"])
		}
		var back proto.PartitionCapability
		if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, capability) {
			t.Fatalf("round trip = %+v (%v), want %+v", back, err, capability)
		}
	}
}
