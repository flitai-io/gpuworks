package scheduler

import (
	"slices"

	"gpuworks.ai/gpuworks/internal/accelerator"
	serverestimate "gpuworks.ai/gpuworks/internal/server/estimate"
)

// Sharing a card with processes outside the platform (ADR 0068).
//
// A card somebody else's workload holds takes no whole-card work: it is not
// free. A memory-limited partition asks for less than a card, though, and
// when the agent says how much the outside processes hold, what they leave
// - less a headroom, since their figure is a reading rather than a limit -
// is room the partition can be admitted against like any other claim.

// takesWorkFor is whether the card may host this request at all: a card no
// gate withholds (resolveGates) that takes work, or one held outside the
// platform that a partition may share by memory.
func (g GPU) takesWorkFor(req Request) bool {
	if g.withheld > 0 {
		return false
	}
	if g.Schedulable {
		return true
	}
	return g.sharesForeignWith(req)
}

// sharesForeignWith is whether this is a held card a partition request may
// share by memory, before the sleep rule.
func (g GPU) sharesForeignWith(req Request) bool {
	return g.ForeignShareable && req.PerGPULimitBytes > 0
}

// sharesForeignNow is whether placing on this card shares it with processes
// outside the platform: a held card no gate withholds and that takes no
// work otherwise. A card an operator allowed work on is not shared by this
// rule (ADR 0068).
func (g GPU) sharesForeignNow() bool {
	return g.ForeignShareable && !g.Schedulable && g.withheld == 0
}

// foreignHold is the memory a partition may not claim because processes
// outside the platform hold it: their figure and the headroom kept beside it.
func (g GPU) foreignHold() int64 {
	if g.ForeignBytes <= 0 {
		return 0
	}
	return g.ForeignBytes + serverestimate.ForeignHeadroomBytes(g.MemoryTotal)
}

// sleeps is whether the deployment puts its engine to sleep.
func (r Request) sleeps() bool {
	return slices.Contains(r.Features, accelerator.FeatureEngineSleep)
}

// pinnedGPUs narrows the filters to the cards a pin names, so a verdict
// about "the node's cards" is about the ones the request may use. A pin
// that names none of the node's cards keeps them all: the planner then
// reports the count it could not meet.
func pinnedGPUs(gpus []GPU, req Request) []GPU {
	if len(req.AllowedGPUIndexes) == 0 {
		return gpus
	}
	if pinned := allowedGPUs(gpus, req.AllowedGPUIndexes); len(pinned) > 0 {
		return pinned
	}
	return gpus
}

// heldReason says why a card that does not take this request cannot: the
// gate that withholds it (gateRefusal), otherwise the holder outside the
// platform. gpus are the cards the verdict is about, which a gate's refusal
// may name.
func heldReason(req Request, node string, gpus []GPU, g GPU) Reason {
	if g.withheld > 0 {
		return gateRefusal(req, node, gpus, g)
	}
	return Reason{Code: ReasonGPUsHeldOutsidePlatform}
}
