package accelerator

// Feature is a platform capability requested beyond ordinary engine inference.
type Feature string

const (
	FeatureEngineSleep      Feature = "engine_sleep"
	FeatureMultiNode        Feature = "multi_node"
	FeaturePD               Feature = "prefill_decode_disaggregation"
	FeatureKVDiskTier       Feature = "kv_disk_tier"
	FeaturePipelineParallel Feature = "pipeline_parallelism"
	FeatureDataParallel     Feature = "data_parallelism"
)

var Features = []Feature{FeatureEngineSleep, FeatureMultiNode, FeaturePD, FeatureKVDiskTier, FeaturePipelineParallel, FeatureDataParallel}

// A missing entry imposes no additional platform restriction. It does not
// promise engine or partition-backend support. Apple retains its existing
// all-features restriction; its ordinary shared-memory inference is separate.
type targetConstraint struct {
	unsupported     []Feature
	denyAll         bool
	runtime         hostRequirements
	explicitDevices bool
}

var targetConstraints = map[string]targetConstraint{
	VendorApple:     {denyAll: true},
	VendorKunlunxin: kunlunTargetConstraint,
	// AMD runs every launch feature NVIDIA does: multi-node, pipeline and
	// data parallelism ride RCCL over the same NCCL_* socket and IB
	// variables, vLLM sleep mode uses CuMemAllocator on HIP VMM, and PD uses
	// NIXL over UCX. What remains AMD-specific is the host runtime and the
	// explicit device selection below.
	VendorAMD: {
		runtime:         hostRequirements{os: "linux", arch: "amd64", drivers: []string{"docker", "nerdctl", "udocker", "kube"}},
		explicitDevices: true,
	},
}

func Supports(vendor string, feature Feature) bool {
	c := targetConstraints[NormalizeVendor(vendor)]
	if c.denyAll {
		return false
	}
	for _, blocked := range c.unsupported {
		if blocked == feature {
			return false
		}
	}
	return true
}

// SharedByMemory is an admission policy, not a description of physical UMA.
// AMD continues to allocate whole cards even on hosts with shared system RAM.
func SharedByMemory(vendor string) bool {
	return NormalizeVendor(vendor) == VendorApple
}

func Unsupported(vendor string, want ...Feature) []Feature {
	var missing []Feature
	for _, feature := range want {
		if !Supports(vendor, feature) {
			missing = append(missing, feature)
		}
	}
	return missing
}

// RequiresDeviceSelection requires explicit allocation on both server and agent.
func RequiresDeviceSelection(vendor string) bool {
	return targetConstraints[NormalizeVendor(vendor)].explicitDevices
}

// RequiresKnownLaunchFeatures marks explicit partial launch restrictions. A
// blanket denyAll declaration (Apple) rejects requested features but preserves
// ordinary unknown-dialect inference; it does not claim an engine dialect.
func RequiresKnownLaunchFeatures(vendor string) bool {
	return len(targetConstraints[NormalizeVendor(vendor)].unsupported) > 0
}

// RequiresCollectiveInterface reports whether the target's collective runtime
// needs a real host interface rather than the single-process loopback default.
func RequiresCollectiveInterface(vendor string, nNodes, devices int) bool {
	return NormalizeVendor(vendor) == VendorKunlunxin && (nNodes > 1 || devices > 1)
}
