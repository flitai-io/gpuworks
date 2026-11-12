package deployment

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func partitionTestNode(t *testing.T, vendor, backend, archID string) *store.Node {
	t.Helper()
	assets, err := json.Marshal([]proto.AssetReport{{Backend: backend, Root: "/assets", Files: []string{"libvgpu.so", "libvruntime.so"}}})
	if err != nil {
		t.Fatal(err)
	}
	probes, err := json.Marshal([]proto.PartitionProbeResult{{Backend: backend, GPUIndex: new(int), Status: proto.PartitionProbePassed}})
	if err != nil {
		t.Fatal(err)
	}
	node := &store.Node{GPUVendor: vendor, Arch: "amd64", Framework: "cann", FrameworkVersion: "8.5.1", RuntimeDriver: "docker",
		PartitionAssets: string(assets), PartitionProbes: string(probes), PartitionSafetyProtocol: 1}
	node.GPUs = []store.GPU{{Index: 0, Vendor: vendor, ArchID: archID}}
	return node
}

// The backend of a quota: named, it is held to exactly that one; unnamed, it is
// the node's own ready one. Automatic placement never names one (it has no node
// to name it for), so an unnamed quota answering NVIDIA's refused every Ascend
// node in that mode.
func TestResolvePartitionBackend(t *testing.T) {
	ascend := partitionTestNode(t, "ascend", accelerator.BackendAscendLogical, "910B4")
	nvidia := partitionTestNode(t, "nvidia", accelerator.BackendNVIDIALogical, "sm_80")
	for _, tc := range []struct {
		name      string
		node      *store.Node
		limitGiB  float64
		requested string
		want      string
		wantErr   string
	}{
		{name: "unnamed on ascend", node: ascend, limitGiB: 8, want: accelerator.BackendAscendLogical},
		{name: "unnamed on nvidia", node: nvidia, limitGiB: 8, want: accelerator.BackendNVIDIALogical},
		{name: "named matches", node: ascend, limitGiB: 8, requested: accelerator.BackendAscendLogical, want: accelerator.BackendAscendLogical},
		{name: "named elsewhere", node: ascend, limitGiB: 8, requested: accelerator.BackendNVIDIALogical, wantErr: "does not match the selected node"},
		{name: "no quota", node: nvidia, want: ""},
		{name: "backend without quota", node: nvidia, requested: accelerator.BackendNVIDIALogical, wantErr: "requires a positive vram_limit_gib"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolvePartitionBackend(tc.node, tc.limitGiB, tc.requested)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("backend = %q err = %v, want %q", got, err, tc.want)
			}
		})
	}

	// Nothing ready: the refusal is the node's own backend's reason, not a
	// complaint that NVIDIA's does not match an Ascend node.
	ascend.PartitionProbes = "null"
	_, err := ResolvePartitionBackend(ascend, 8, "")
	if err == nil || !strings.Contains(err.Error(), accelerator.BackendAscendLogical) || strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want the ascend backend's own unavailability", err)
	}
}
