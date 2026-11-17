package scheduler

import "gpuworks.ai/gpuworks/internal/accelerator"

// runFilters applies the hard admission filters in order. Returns the first
// failure reason, or ok=true.
func runFilters(req Request, node NodeSnapshot) (bool, Reason) {
	if node.ServingUnavailable {
		return false, Reason{Code: ReasonServingUnavailable}
	}

	if missing := accelerator.Unsupported(node.Vendor, req.Features...); len(missing) > 0 {
		return false, UnsupportedFeature(node.Vendor, missing[0])
	}
	if req.StorageRefusal != nil {
		return false, *req.StorageRefusal
	}
	if !node.DtypeSupported {
		if node.DtypeReason != "" {
			return false, Reason{Code: ReasonDtypeUnsupportedDetail, Params: map[string]any{"detail": node.DtypeReason}}
		}
		return false, Reason{Code: ReasonDtypeUnsupported}
	}
	// A pin is what the request may use; the cards it does not name say
	// nothing about it (pinnedGPUs).
	gpus := pinnedGPUs(node.GPUs, req)
	if req.PerGPULimitBytes > 0 {
		partitionReady := false
		for _, gpu := range gpus {
			if gpu.takesWorkFor(req) && contains(gpu.PartitionBackends, req.PartitionBackend) {
				partitionReady = true
				break
			}
		}
		if req.PartitionBackend == "" {
			return false, Reason{Code: ReasonPartitionUnavailable}
		}
		if !partitionReady {
			if countTaking(gpus, req) == 0 {
				// None of the cards takes this request at all: that, not
				// the backend, is what the operator must act on.
				return false, noSchedulableGPUReason(req, node.UUID, gpus)
			}
			return false, Reason{Code: ReasonPartitionUnavailable}
		}
	}
	if req.DiskNeedBytes > 0 && node.DiskAvailBytes > 0 && node.DiskAvailBytes < req.DiskNeedBytes {
		return false, Reason{Code: ReasonInsufficientDisk}
	}
	if req.RuntimeDiskNeedBytes > 0 && node.RuntimeDiskAvailBytes > 0 && node.RuntimeDiskAvailBytes < req.RuntimeDiskNeedBytes {
		return false, Reason{Code: ReasonInsufficientRuntimeDisk}
	}
	if countTaking(gpus, req) == 0 {
		return false, noSchedulableGPUReason(req, node.UUID, gpus)
	}
	if countFree(gpus, req) == 0 {
		return false, Reason{Code: ReasonNoFreeGPU}
	}
	return true, Reason{}
}

// featureReason names each platform feature's refusal.
var featureReason = map[accelerator.Feature]string{
	accelerator.FeatureEngineSleep:      ReasonSleepUnsupportedOnNode,
	accelerator.FeatureMultiNode:        ReasonMultiNodeUnsupportedOnNode,
	accelerator.FeaturePD:               ReasonPDUnsupportedOnNode,
	accelerator.FeatureKVDiskTier:       ReasonKVDiskUnsupportedOnNode,
	accelerator.FeaturePipelineParallel: ReasonAcceleratorFeatureUnsupported,
	accelerator.FeatureDataParallel:     ReasonAcceleratorFeatureUnsupported,
	accelerator.FeatureExternalKVCache:  ReasonKVBackendUnsupportedOnNode,
}

// FeatureReason returns the scheduler refusal code for an unsupported
// platform feature. Planning uses the same mapping when explaining candidate
// nodes before running a full schedule.
func FeatureReason(feature accelerator.Feature) string {
	return featureReason[feature]
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countFree(gpus []GPU, req Request) int {
	n := 0
	for _, g := range gpus {
		if g.takesWorkFor(req) && g.unclaimed() > 0 {
			n++
		}
	}
	return n
}

// countTaking counts the cards that may host this request at all
// (GPU.takesWorkFor).
func countTaking(gpus []GPU, req Request) int {
	n := 0
	for _, g := range gpus {
		if g.takesWorkFor(req) {
			n++
		}
	}
	return n
}

func countSchedulable(gpus []GPU) int {
	n := 0
	for _, g := range gpus {
		if g.Schedulable {
			n++
		}
	}
	return n
}

// noSchedulableGPUReason says why a node none of whose cards takes work
// cannot take this request, naming what an operator would act on. A card
// held outside the platform is named before a fault: freeing it is the
// quicker remedy, and it is not something a health alert would show. A
// partition that could share such a card but sleeps is told that instead.
func noSchedulableGPUReason(req Request, node string, gpus []GPU) Reason {
	if len(gpus) == 0 {
		return Reason{Code: ReasonNoOwnedGPU}
	}
	for _, g := range gpus {
		if g.HeldOutsidePlatform || g.withheld > 0 {
			return heldReason(req, node, gpus, g)
		}
	}
	return Reason{Code: ReasonNoHealthyGPU}
}

// usableGPUs returns the GPUs that can host the request at the given per-GPU
// need. A need of zero asks for structural eligibility only, which is what the
// planner starts from before a placement contract has had its say.
func usableGPUs(gpus []GPU, req Request, need int64) []GPU {
	var out []GPU
	for _, g := range gpus {
		if g.UsableFor(req, need) {
			out = append(out, g)
		}
	}
	return out
}

// UnsupportedFeature is the shared structured refusal for a target capability.
func UnsupportedFeature(vendor string, feature accelerator.Feature) Reason {
	code := FeatureReason(feature)
	if code == "" {
		code = ReasonAcceleratorFeatureUnsupported
	}
	return Reason{Code: code, Params: map[string]any{"vendor": accelerator.VendorBrand(vendor), "feature": string(feature)}}
}
