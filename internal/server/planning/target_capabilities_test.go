package planning

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestTargetCardFilteringKeepsCompatibleMixedVendorCandidate(t *testing.T) {
	node := &store.Node{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{
		{Index: 0, Name: "AMD Instinct MI300X", Vendor: "amd", ArchID: "gfx942"},
		{Index: 1, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90"},
	}}
	launch := &proto.RequestedFeatures{EngineSleep: true}
	in := Input{Node: node, RequestedFeatures: launch, Sleepable: true}
	snap := scheduler.NodeSnapshot{Vendor: "amd", GPUs: []scheduler.GPU{{Index: 0}, {Index: 1}}}
	if refusal := constrainTargetGPUs(in, &snap); refusal != nil {
		t.Fatalf("mixed node refusal=%+v", refusal)
	}
	// AMD sleeps and needs no recognizable command, so both cards stay.
	if len(snap.GPUs) != 2 || snap.Vendor != "" {
		t.Fatalf("filtered candidates=%+v vendor=%q", snap.GPUs, snap.Vendor)
	}
}

func TestNeedsTargetPreflightUsesOwnedPhysicalCards(t *testing.T) {
	cases := []struct {
		name string
		node *store.Node
		want bool
	}{
		{name: "nil node"},
		{name: "nvidia", node: &store.Node{GPUVendor: "nvidia", GPUs: []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100"}}}, want: true},
		{name: "huawei", node: &store.Node{GPUVendor: "ascend", GPUs: []store.GPU{{Index: 0, Vendor: "ascend", Name: "910B"}}}, want: true},
		{name: "apple", node: &store.Node{GPUVendor: "apple", GPUs: []store.GPU{{Index: 0, Vendor: "apple", Name: "Apple GPU"}}}, want: true},
		{name: "AMD owned", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "amd", Name: "MI300X"}}}, want: true},
		{name: "mixed includes AMD", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100"}, {Index: 1, Vendor: "amd", Name: "MI300X"}}}, want: true},
		{name: "AMD passed through", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "amd", Name: "MI300X", Ownership: "passthrough"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsTargetPreflight(tc.node); got != tc.want {
				t.Fatalf("NeedsTargetPreflight()=%t want %t", got, tc.want)
			}
		})
	}
}

func TestResolvedFeaturesAreAppliedToEveryVendor(t *testing.T) {
	in := Input{
		Engine:            "vllm",
		RequestedFeatures: &proto.RequestedFeatures{EngineSleep: true},
	}
	features := requestedInputFeaturesForVendor(in, accelerator.VendorAMD)
	found := false
	for _, feature := range features {
		found = found || feature == accelerator.FeatureEngineSleep
	}
	if !found {
		t.Fatalf("AMD did not infer the requested launch feature: %v", features)
	}
	apple := &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "apple", Name: "Apple GPU"}}}
	appleInput := Input{Node: apple, Engine: "vllm", Sleepable: true, RequestedFeatures: in.RequestedFeatures}
	snapshot := scheduler.NodeSnapshot{GPUs: []scheduler.GPU{{Index: 0}}}
	if refusal := constrainTargetGPUs(appleInput, &snapshot); refusal == nil || refusal.Code != scheduler.ReasonSleepUnsupportedOnNode {
		t.Fatalf("Apple's declared sleep restriction was not preserved: %+v", refusal)
	}
}

func TestExplicitSelectionRejectsOneIncompatibleGPU(t *testing.T) {
	// Apple is the vendor that refuses requested features.
	node := &store.Node{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{{Index: 0, Name: "Apple M4 Max", Vendor: "apple"}, {Index: 1, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90"}}}
	in := Input{Node: node, GPUIndexes: []int{0, 1}, Sleepable: true}
	snap := scheduler.NodeSnapshot{GPUs: []scheduler.GPU{{Index: 0}, {Index: 1}}}
	refusal := constrainTargetGPUs(in, &snap)
	if refusal == nil || refusal.Code != scheduler.ReasonSleepUnsupportedOnNode {
		t.Fatalf("explicit incompatible selection refusal=%+v", refusal)
	}
}

func TestAutomaticTargetGateCanKeepOneCompatibleCard(t *testing.T) {
	// Apple is the vendor that refuses requested features.
	node := &store.Node{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{{Index: 0, Name: "Apple M4 Max", Vendor: "apple"}, {Index: 1, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90"}}}
	req := LaunchRequest{Node: node, Model: &store.Model{}, Engine: "vllm", Sleepable: true}
	if refusal, err := TargetCandidateRefusal(req, deploy.CatalogSelection{}, node); err != nil || refusal != nil {
		t.Fatalf("compatible auto candidate refused: %+v", refusal)
	}
	node.GPUs = node.GPUs[:1]
	if refusal, err := TargetCandidateRefusal(req, deploy.CatalogSelection{}, node); err != nil || refusal == nil || refusal.Code != scheduler.ReasonSleepUnsupportedOnNode {
		t.Fatalf("unsupported auto candidate refusal=%+v err=%v", refusal, err)
	}
	// Neither sleep nor data parallelism rules an AMD card out.
	node.GPUs = []store.GPU{{Index: 0, Name: "AMD Instinct MI300X", Vendor: "amd", ArchID: "gfx942"}}
	sleepy := LaunchRequest{Node: node, Model: &store.Model{}, Engine: "vllm", Sleepable: true, Quick: deploy.LaunchConfig{DP: 2}}
	if refusal, err := TargetCandidateRefusal(sleepy, deploy.CatalogSelection{}, node); err != nil || refusal != nil {
		t.Fatalf("sleepable AMD candidate refused: %+v err=%v", refusal, err)
	}
}

func TestTargetCandidateRefusalDefersMixedExplicitTemplateMismatch(t *testing.T) {
	layer := deploy.CatalogSelection{GPUs: []byte(`{"vendor":"amd"}`)}
	orders := []struct {
		name string
		gpus []store.GPU
	}{
		{
			name: "unsupported matching card first",
			gpus: []store.GPU{
				{Index: 0, Name: "AMD Instinct MI300X", Vendor: "amd", ArchID: "gfx942"},
				{Index: 1, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90"},
			},
		},
		{
			name: "template mismatch first",
			gpus: []store.GPU{
				{Index: 1, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90"},
				{Index: 0, Name: "AMD Instinct MI300X", Vendor: "amd", ArchID: "gfx942"},
			},
		},
	}
	for _, tc := range orders {
		t.Run(tc.name, func(t *testing.T) {
			node := &store.Node{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: tc.gpus}
			req := LaunchRequest{
				Node: node, Model: &store.Model{}, Engine: "vllm", Sleepable: true,
				Placement: Placement{GPUIndexes: []int{0, 1}},
			}
			refusal, err := TargetCandidateRefusal(req, layer, node)
			if err != nil || refusal != nil {
				t.Fatalf("explicit mixed candidate should defer template mismatch to trial: refusal=%+v err=%v", refusal, err)
			}

			validator, err := TemplateGPUValidator(layer)
			if err != nil {
				t.Fatal(err)
			}
			in := Input{Node: node, Engine: "vllm", GPUIndexes: []int{0, 1}, Sleepable: true, ValidateGPU: validator}
			// Exercise the production evaluation path in the node's actual
			// owned-card order; template validation must finish before any
			// selected card can produce a target-capability refusal.
			snapGPUs := make([]scheduler.GPU, 0, len(tc.gpus))
			for _, gpu := range tc.gpus {
				snapGPUs = append(snapGPUs, scheduler.GPU{Index: gpu.Index})
			}
			snap := scheduler.NodeSnapshot{GPUs: snapGPUs}
			trialRefusal := constrainTargetGPUs(in, &snap)
			if trialRefusal == nil || trialRefusal.Code != scheduler.ReasonPlacementRejected {
				t.Fatalf("bounded placement trial refusal=%+v, want template placement rejection", trialRefusal)
			}
		})
	}
}

func TestTargetCandidateRefusalReturnsInvalidTemplateError(t *testing.T) {
	node := &store.Node{GPUVendor: "amd", GPUs: []store.GPU{{Index: 0, Name: "MI300X", Vendor: "amd"}}}
	req := LaunchRequest{Node: node, Model: &store.Model{}, Engine: "vllm"}
	refusal, err := TargetCandidateRefusal(req, deploy.CatalogSelection{GPUs: []byte("{")}, node)
	if err == nil || refusal != nil {
		t.Fatalf("invalid template must propagate as an error, got refusal=%+v err=%v", refusal, err)
	}
}

func TestSleepDeclarableUsesFeatureRefusalWithoutGPUSelectionGate(t *testing.T) {
	err := SleepDeclarable(true, "vllm", "apple")
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorAcceleratorFeatureUnsupported {
		t.Fatalf("Apple sleep declaration error=%v, want feature refusal", err)
	}
	if product.Params()["feature"] != "engine_sleep" {
		t.Fatalf("Apple sleep refusal params=%v", product.Params())
	}
	if err := SleepDeclarable(true, "vllm", "amd"); err != nil {
		t.Fatalf("AMD sleep declaration refused: %v", err)
	}
	if err := SleepDeclarable(false, "vllm", "amd"); err != nil {
		t.Fatalf("non-sleepable AMD request should not require target selection: %v", err)
	}
}

func TestTargetCardFilteringKeepsNVIDIAPeerOnKunlunMixedNode(t *testing.T) {
	node := &store.Node{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{
		{Index: 0, Name: "P800 PCIe", Vendor: "kunlunxin", ArchID: "kl3"},
		{Index: 1, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90"},
	}}
	in := Input{Node: node, RequestedFeatures: &proto.RequestedFeatures{CommandKnown: true, EngineSleep: true}, Sleepable: true}
	snap := scheduler.NodeSnapshot{Vendor: "kunlunxin", GPUs: []scheduler.GPU{{Index: 0}, {Index: 1}}}
	if refusal := constrainTargetGPUs(in, &snap); refusal != nil {
		t.Fatalf("mixed node refusal=%+v", refusal)
	}
	if len(snap.GPUs) != 1 || snap.GPUs[0].Index != 1 || snap.Vendor != "nvidia" {
		t.Fatalf("filtered candidates=%+v vendor=%q", snap.GPUs, snap.Vendor)
	}
}

func TestKunlunTargetAdmissionInspectsHandwrittenParallelModes(t *testing.T) {
	node := &store.Node{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUVendor: "kunlunxin", GPUs: []store.GPU{{Index: 0, Name: "P800 PCIe", Vendor: "kunlunxin", ArchID: "kl3"}}}
	for _, args := range [][]string{{"-pp", "2"}, {"--data-parallel-size", "2"}} {
		req := LaunchRequest{Node: node, Model: &store.Model{}, Engine: "vllm", ExtraArgs: args, Source: LaunchSourceFromRequest("registry.example/kunlun:v1", deploy.ContainerOverrides{})}
		if refusal, err := TargetCandidateRefusal(req, deploy.CatalogSelection{}, node); err != nil || refusal != nil {
			t.Fatalf("supported handwritten args %v: refusal=%+v err=%v", args, refusal, err)
		}
	}
	req := LaunchRequest{Node: node, Model: &store.Model{}, Engine: "vllm", Source: LaunchSourceFromRequest("registry.example/kunlun:v1", deploy.ContainerOverrides{
		CommandMode: string(deploy.CommandManual), CmdDisplay: "/bin/sh -c 'vllm serve -pp 2'",
	})}
	refusal, err := TargetCandidateRefusal(req, deploy.CatalogSelection{}, node)
	if err != nil || refusal == nil || refusal.Code != scheduler.ReasonLaunchFeaturesUnknown {
		t.Fatalf("unknown shell refusal=%+v err=%v", refusal, err)
	}
}
