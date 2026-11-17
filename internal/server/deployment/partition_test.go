package deployment

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
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
		wantCode  string
	}{
		{name: "unnamed on ascend", node: ascend, limitGiB: 8, want: accelerator.BackendAscendLogical},
		{name: "unnamed on nvidia", node: nvidia, limitGiB: 8, want: accelerator.BackendNVIDIALogical},
		{name: "named matches", node: ascend, limitGiB: 8, requested: accelerator.BackendAscendLogical, want: accelerator.BackendAscendLogical},
		{name: "named elsewhere", node: ascend, limitGiB: 8, requested: accelerator.BackendNVIDIALogical, wantErr: "does not match the selected node", wantCode: messages.ErrorPartitionBackendNotOnNode},
		{name: "no quota", node: nvidia, want: ""},
		{name: "backend without quota", node: nvidia, requested: accelerator.BackendNVIDIALogical, wantErr: "requires a positive vram_limit_gib"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolvePartitionBackend(tc.node, tc.limitGiB, tc.requested)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				var product *messages.ProductError
				if tc.wantCode != "" && (!errors.As(err, &product) || product.Code() != tc.wantCode) {
					t.Fatalf("err = %v, want code %s", err, tc.wantCode)
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
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorPartitionAgentUpgradeRequired || !strings.Contains(err.Error(), accelerator.BackendAscendLogical) {
		t.Fatalf("err = %v, want the ascend backend's own unavailability", err)
	}
}

// Another member of a distributed group refusing the backend chosen on the
// first node keeps the group code, with that member's reason code and params.
func TestDistributedPartitionRefusalCarriesTheMembersReason(t *testing.T) {
	ascend := partitionTestNode(t, "ascend", accelerator.BackendAscendLogical, "910B4")
	ascend.UUID, ascend.Name = "node-2", "npu-2"
	ascend.PartitionProbes = `[{"backend":"ascend-dgpu","gpu_index":0,"status":"failed","detail":"quota mismatch"}]`
	_, refusal := ResolvePartitionBackend(ascend, 8, accelerator.BackendAscendLogical)
	err := distributedPartitionRefusal(ascend, accelerator.BackendAscendLogical, refusal)
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorDistributedPartitionBackendUnavailable {
		t.Fatalf("err = %v", err)
	}
	params := product.Params()
	if params["node"] != "npu-2" || params["reason_code"] != messages.ErrorPartitionProbeFailed || params["probe_detail"] != "quota mismatch" || params["backend"] == "" {
		t.Fatalf("params = %#v", params)
	}
	if _, leaked := params["detail"]; leaked {
		t.Fatalf("params carry an English detail: %#v", params)
	}
}
