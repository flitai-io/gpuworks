package scheduler

// Card-level causes do not claim that the whole node has no healthy/owned GPUs.
const (
	ReasonGPUNotSchedulable   = "gpu_not_schedulable"
	ReasonGPUIndexUnavailable = "gpu_index_unavailable"
	ReasonGPUWholeAllocated   = "gpu_whole_allocated"
)

// GPUCandidate describes current resource availability, not permission to
// select a card or a promise that the model fits on it alone.
type GPUCandidate struct {
	Index     int
	Available bool
	Reason    *Reason
}

// GPUCandidates explains every card before the selected GPU range is applied.
// Model fit, parallelism and topology remain the complete plan's responsibility.
func GPUCandidates(req Request, gpus []GPU) []GPUCandidate {
	items := make([]GPUCandidate, 0, len(gpus))
	var invalid Reason
	if err := req.Validate(); err != nil {
		invalid = Reason{Code: ReasonInvalidRequest, Message: err.Error()}
	}
	need := int64(0)
	if req.PerGPULimitBytes > 0 {
		need = admitNeed(req, 1, 0)
	}
	for _, gpu := range gpus {
		reason := invalid
		if reason.Code == "" {
			cardNeed := need
			if gpu.SharedByMemory && req.PerGPULimitBytes == 0 {
				cardNeed = reserveFor(req)
			}
			reason = gpuResourceRefusal(req, gpu, cardNeed)
		}
		item := GPUCandidate{Index: gpu.Index, Available: reason.Code == ""}
		if !item.Available {
			item.Reason = &reason
		}
		items = append(items, item)
	}
	return items
}

func gpuResourceRefusal(req Request, gpu GPU, need int64) Reason {
	if gpu.Index < 0 {
		return Reason{Code: ReasonGPUIndexUnavailable}
	}
	if !gpu.Schedulable {
		if gpu.HeldOutsidePlatform {
			return Reason{Code: ReasonGPUsHeldOutsidePlatform}
		}
		return Reason{Code: ReasonGPUNotSchedulable}
	}
	if !gpu.QualifiesFor(req) {
		if req.PerGPULimitBytes > 0 {
			return Reason{Code: ReasonPartitionUnavailable}
		}
		if gpu.WholeGPUAllocated {
			return Reason{Code: ReasonGPUWholeAllocated}
		}
		return Reason{Code: ReasonNoFreeGPU}
	}
	if !gpu.UsableFor(req, need) {
		return reasonMemoryShortfall(req, []GPU{gpu}, 1, need)
	}
	if !gpu.SharedByMemory {
		if reason, blocked := sleepResidueBlock(req, []GPU{gpu}, 1, need); blocked {
			return reason
		}
	}
	if gpu.Available() <= 0 {
		return Reason{Code: ReasonNoFreeGPU}
	}
	return Reason{}
}
