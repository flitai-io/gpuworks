package scheduler

// Card-level causes do not claim that the whole node has no healthy/owned GPUs.
const (
	ReasonGPUNotSchedulable   = "gpu_not_schedulable"
	ReasonGPUIndexUnavailable = "gpu_index_unavailable"
	ReasonGPUWholeAllocated   = "gpu_whole_allocated"
	// ReasonForeignSharedCard notes an offered card that processes outside
	// the platform also use ({foreign_gib}): its memory is isolated, its
	// compute is not.
	ReasonForeignSharedCard = "foreign_shared_card"
)

// GPUCandidate describes current resource availability, not permission to
// select a card or a promise that the model fits on it alone.
type GPUCandidate struct {
	Index     int
	Available bool
	Reason    *Reason
	// Note is what an available card's choice entails: sharing it with
	// processes outside the platform. Nil for a card with nothing to note.
	Note *Reason
}

// GPUCandidates explains every card of node before the selected GPU range is
// applied, through the same card gates as Schedule. The list is what an
// operator picks cards from, and picking a card consents to sharing it with
// processes outside the platform (Consent), so it is judged with
// that consent. Model fit, parallelism and topology remain the complete
// plan's responsibility.
func GPUCandidates(req Request, node NodeSnapshot) []GPUCandidate {
	req.Consent.CardsChosen = true
	var invalid Reason
	if err := req.Validate(); err != nil {
		invalid = Reason{Code: ReasonInvalidRequest, Message: err.Error()}
	}
	need := int64(0)
	if req.PerGPULimitBytes > 0 {
		need = admitNeed(req, 1, 0)
	}
	node = resolveGates(req, node)
	items := make([]GPUCandidate, 0, len(node.GPUs))
	for _, gpu := range node.GPUs {
		reason := invalid
		if reason.Code == "" {
			cardNeed := need
			if gpu.SharedByMemory && req.PerGPULimitBytes == 0 {
				cardNeed = reserveFor(req)
			}
			reason = gpuResourceRefusal(req, node.UUID, gpu, cardNeed)
		}
		item := GPUCandidate{Index: gpu.Index, Available: reason.Code == ""}
		if !item.Available {
			item.Reason = &reason
		} else if gpu.sharesForeignNow() {
			item.Note = &Reason{Code: ReasonForeignSharedCard, Params: map[string]any{"foreign_gib": gib(gpu.ForeignBytes)}}
		}
		items = append(items, item)
	}
	return items
}

func gpuResourceRefusal(req Request, node string, gpu GPU, need int64) Reason {
	if gpu.Index < 0 {
		return Reason{Code: ReasonGPUIndexUnavailable}
	}
	if !gpu.takesWorkFor(req) {
		if gpu.HeldOutsidePlatform || gpu.withheld > 0 {
			return heldReason(req, node, []GPU{gpu}, gpu)
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
		return reasonMemoryShortfall(req, []GPU{gpu}, 1, need, false)
	}
	if !gpu.SharedByMemory {
		if reason, blocked := sleepResidueBlock(req, []GPU{gpu}, 1, need); blocked {
			return reason
		}
	}
	if gpu.unclaimed() <= 0 {
		return Reason{Code: ReasonNoFreeGPU}
	}
	return Reason{}
}
