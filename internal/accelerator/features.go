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
	// FeatureExternalKVCache: a component outside the engine keeps the
	// instance's KV cache (a KV offload backend, ADR 0069). Every vendor
	// that lists features lists it; which vendors a backend runs on is the
	// backend's own allow-list, so this is only the requested-feature
	// identity.
	FeatureExternalKVCache Feature = "external_kv_cache"
)

var Features = []Feature{FeatureEngineSleep, FeatureMultiNode, FeaturePD, FeatureKVDiskTier, FeaturePipelineParallel, FeatureDataParallel, FeatureExternalKVCache}

// Supports reports whether a vendor's definition declares a feature. A vendor
// supports what it lists and nothing else, so one added without a list
// offers plain inference only. A name that is no vendor's is not restricted
// here; it is refused elsewhere for what it is.
func Supports(vendor string, feature Feature) bool {
	spec, known := vendorSpecOf(vendor)
	if !known {
		return true
	}
	for _, own := range spec.supports {
		if own == feature {
			return true
		}
	}
	return false
}

// SleepPoolOnHostMemory reports that an engine started with sleep mode keeps
// host memory behind the GPU memory it takes, for as long as it runs. Hygon's
// DTK does: vLLM's sleep allocator maps its pool through the CUDA
// virtual-memory calls, and DTK backs every byte mapped that way with host
// RAM. On a K100_AI (64 GiB) node with 62.4 GiB of RAM, host memory in use
// rose from 13 GiB to 57.5 GiB while a sleepable vLLM 0.18.1 allocated its
// 55 GiB KV cache, and the kernel killed the engine at each of three starts;
// the same launch without sleep mode held no such memory.
func SleepPoolOnHostMemory(vendor string) bool {
	spec, _ := vendorSpecOf(vendor)
	return spec.sleepPoolOnHostMemory
}

// SharedByMemory is an admission policy, not a description of physical UMA.
// AMD continues to allocate whole cards even on hosts with shared system RAM.
func SharedByMemory(vendor string) bool {
	spec, _ := vendorSpecOf(vendor)
	return spec.sharedByMemory
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
	spec, _ := vendorSpecOf(vendor)
	return spec.explicitDevices
}

// RequiresKnownLaunchFeatures marks explicit partial launch restrictions. A
// vendor that supports none (Apple) rejects requested features but preserves
// ordinary unknown-dialect inference; it does not claim an engine dialect.
func RequiresKnownLaunchFeatures(vendor string) bool {
	spec, known := vendorSpecOf(vendor)
	return known && len(spec.supports) > 0 && len(spec.supports) < len(Features)
}

// RequiresCollectiveInterface reports whether the target's collective runtime
// needs a real host interface rather than the single-process loopback default.
func RequiresCollectiveInterface(vendor string, nNodes, devices int) bool {
	spec, _ := vendorSpecOf(vendor)
	return spec.collectiveInterface && (nNodes > 1 || devices > 1)
}
