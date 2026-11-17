package planning

import (
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// A deployment that shares cards by yield policy keeps them while asleep, for
// a request's wake, against a deployment that does not share with it
// (reservedAgainst). When that is the only reason a launch does not fit, the
// refusal says so and names the sleepers, rather than reading as cards in use.

// LaunchReserved is a launch that would fit but for sleepers keeping their
// cards: the cards it would take and the deployments asleep on them.
type LaunchReserved struct {
	GPUIndexes []int
	Sleepers   []ReservedSleeper
}

// ReservedSleeper is a deployment asleep on cards it keeps for a request's
// wake.
type ReservedSleeper struct {
	DeploymentID uint
	Deployment   string
	Policy       yieldpolicy.Policy
}

// launchReserved re-plans req without the reservation; nil when nothing is
// reserved, or the launch does not fit even so. insts are the node's.
func launchReserved(st *store.Store, node *store.Node, insts []store.Instance, req scheduler.Request, dtypeOK bool, dtypeReason string, view ledgerView) *LaunchReserved {
	reserved := view.reserved
	if reserved == nil {
		return nil
	}
	view.reserved = nil
	snap := nodeSnapshot(st, node, dtypeOK, dtypeReason, view)
	result := scheduler.Schedule(req, snap)
	if !result.OK || result.Plan == nil {
		return nil
	}
	// A reserving sleeper is of a deployment that shares by yield policy
	// (reservedAgainst), so the cached sharing deployments name it.
	sharing := sharingByID(st)
	var candidates []*store.Instance
	for i := range insts {
		in := &insts[i]
		if sharing[in.DeploymentID] != nil && !view.excludes(in) && reserved(in) && chargesResidue(st, in) {
			candidates = append(candidates, in)
		}
	}
	out := &LaunchReserved{GPUIndexes: result.Plan.GPUIndexes}
	seen := map[uint]bool{}
	for _, in := range instancesOnCards(candidates, result.Plan) {
		if seen[in.DeploymentID] {
			continue
		}
		seen[in.DeploymentID] = true
		dep := sharing[in.DeploymentID]
		out.Sleepers = append(out.Sleepers, ReservedSleeper{DeploymentID: dep.ID, Deployment: dep.Name, Policy: dep.Policy()})
	}
	if len(out.Sleepers) == 0 {
		return nil
	}
	return out
}

// ReservedRefusal is the refusal of a launch that only sleepers' cards would
// take: the node, the cards, and each deployment asleep on them with its
// yield policy.
func ReservedRefusal(node *store.Node, reserved *LaunchReserved) scheduler.Reason {
	sleepers := make([]map[string]any, 0, len(reserved.Sleepers))
	for _, d := range reserved.Sleepers {
		sleepers = append(sleepers, map[string]any{"deployment": d.Deployment, "policy": d.Policy.Yield})
	}
	return scheduler.Reason{Code: scheduler.ReasonReservedForSleepers, Params: map[string]any{
		"node": node.DisplayName(), "gpus": reserved.GPUIndexes, "sleepers": sleepers,
	}}
}
