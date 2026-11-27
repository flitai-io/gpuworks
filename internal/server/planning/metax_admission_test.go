package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// MetaX is discoverable, but no workload runtime has been verified. A mixed
// node must still offer the independently compatible NVIDIA card.
func TestMetaXRuntimeAdmissionOnMixedNode(t *testing.T) {
	node := &store.Node{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{
		{Index: 0, Vendor: "metax", Name: "MXC550"},
		{Index: 1, Vendor: "nvidia", Name: "H100"},
	}}
	for _, tc := range []struct {
		name      string
		selected  []int
		wantIndex int
		refuse    bool
	}{
		{"automatic", nil, 1, false},
		{"explicit NVIDIA", []int{1}, 1, false},
		{"explicit MetaX", []int{0}, 0, true},
		{"explicit mixed", []int{0, 1}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Node: node, GPUIndexes: tc.selected, RequestedFeatures: &proto.RequestedFeatures{CommandKnown: true}}
			snap := scheduler.NodeSnapshot{GPUs: []scheduler.GPU{{Index: 0}, {Index: 1}}}
			refusal := constrainTargetGPUs(in, &snap)
			if tc.refuse {
				if refusal == nil || refusal.Code != scheduler.ReasonRuntimeUnsupported {
					t.Fatalf("refusal=%+v, want unsupported runtime", refusal)
				}
				return
			}
			if refusal != nil || len(snap.GPUs) != 1 || snap.GPUs[0].Index != tc.wantIndex {
				t.Fatalf("refusal=%+v candidates=%+v", refusal, snap.GPUs)
			}
		})
	}
}
