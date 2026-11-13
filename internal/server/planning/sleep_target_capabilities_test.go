package planning

import (
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type sleepTargetNodeReader struct {
	node *store.Node
	err  error
}

func (r sleepTargetNodeReader) NodeByID(uint) (*store.Node, error) { return r.node, r.err }

// Apple is the vendor without engine sleep; AMD now sleeps like NVIDIA.
func TestUnsupportedSleepTargetFailsClosedOnlyForUnresolvedRestrictedTargets(t *testing.T) {
	apple := &store.Node{ID: 1, GPUs: []store.GPU{{Index: 0, Vendor: "apple", Name: "Apple M4 Max"}}}
	amd := &store.Node{ID: 1, GPUs: []store.GPU{{Index: 0, Vendor: "amd", Name: "Instinct MI300X", ArchID: "gfx942"}}}
	nvidia := &store.Node{ID: 1, GPUs: []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100", ArchID: "sm_90"}}}
	mixed := &store.Node{ID: 1, GPUs: []store.GPU{
		{Index: 0, Vendor: "apple", Name: "Apple M4 Max"},
		{Index: 1, Vendor: "nvidia", Name: "H100", ArchID: "sm_90"},
	}}
	cases := []struct {
		name    string
		node    *store.Node
		indexes string
		desired string
		wantErr bool
		wantRef string
	}{
		{name: "selected restricted card is refused", node: apple, indexes: "[0]", wantRef: messages.ErrorAcceleratorFeatureUnsupported},
		{name: "empty restricted selection is unresolved", node: apple, indexes: "[]", wantErr: true},
		{name: "restricted vendor with missing inventory is unresolved", node: &store.Node{ID: 1, GPUVendor: "apple"}, indexes: "[0]", wantErr: true},
		{name: "stale restricted index is unresolved", node: apple, indexes: "[9]", wantErr: true},
		{name: "selected AMD sleeps", node: amd, indexes: "[0]"},
		{name: "empty AMD selection keeps prior behavior", node: amd, indexes: "[]"},
		{name: "empty pure NVIDIA record keeps prior behavior", node: nvidia, indexes: "[]"},
		{name: "stale pure NVIDIA record keeps prior behavior", node: nvidia, indexes: "[9]"},
		{name: "malformed pure NVIDIA desired state is not newly parsed", node: nvidia, indexes: "[]", desired: "{"},
		{name: "mixed node explicitly selected NVIDIA remains allowed", node: mixed, indexes: "[1]"},
		{name: "mixed node stale selection is unresolved", node: mixed, indexes: "[9]", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desired := tc.desired
			if desired == "" {
				desired = `{}`
			}
			inst := store.Instance{UUID: "sleep-target", NodeID: 1, State: proto.InstRunning, GPUIndexes: tc.indexes, DesiredStateJSON: desired}
			refusal, err := UnsupportedSleepTarget("vllm", []store.Instance{inst}, sleepTargetNodeReader{node: tc.node})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%t refusal=%+v", err, tc.wantErr, refusal)
			}
			if tc.wantErr {
				var product *messages.ProductError
				if !errors.As(err, &product) || product.Code() != messages.ErrorSleepTargetUnresolved {
					t.Fatalf("unstructured unresolved target error: %v", err)
				}
			}
			if tc.wantErr && !strings.Contains(err.Error(), "cannot verify sleep target") {
				t.Fatalf("error=%q does not identify unresolved target", err)
			}
			if tc.wantRef != "" {
				if refusal == nil || refusal.Code != tc.wantRef || refusal.Params["vendor"] != "Apple" || refusal.Params["feature"] != "engine_sleep" {
					t.Fatalf("refusal=%+v, want Apple engine sleep refusal", refusal)
				}
			} else if refusal != nil {
				t.Fatalf("unexpected refusal: %+v", refusal)
			}
		})
	}
}

func TestUnsupportedSleepTargetRefusesKunlun(t *testing.T) {
	node := &store.Node{ID: 1, GPUs: []store.GPU{{Index: 0, Vendor: "kunlunxin", Name: "P800 PCIe", ArchID: "kl3"}}}
	inst := store.Instance{UUID: "sleep-target", NodeID: 1, State: proto.InstRunning, GPUIndexes: "[0]", DesiredStateJSON: `{}`}
	refusal, err := UnsupportedSleepTarget("vllm", []store.Instance{inst}, sleepTargetNodeReader{node: node})
	if err != nil || refusal == nil || refusal.Code != messages.ErrorAcceleratorFeatureUnsupported || refusal.Params["vendor"] != "Kunlunxin" || refusal.Params["feature"] != "engine_sleep" {
		t.Fatalf("refusal=%+v err=%v", refusal, err)
	}
}

func TestUnsupportedSleepTargetPropagatesInventoryReadErrors(t *testing.T) {
	readErr := errors.New("inventory read failed")
	inst := store.Instance{UUID: "sleep-target", NodeID: 1, State: proto.InstRunning, GPUIndexes: "[]", DesiredStateJSON: `{}`}
	_, err := UnsupportedSleepTarget("vllm", []store.Instance{inst}, sleepTargetNodeReader{err: readErr})
	if !errors.Is(err, readErr) {
		t.Fatalf("error=%v, want inventory read error", err)
	}
}
