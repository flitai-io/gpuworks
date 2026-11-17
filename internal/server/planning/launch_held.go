package planning

import (
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// A launch that neither waits for holders (launchYield) nor is kept off
// sleepers' cards (launchReserved) may still not fit only because awake
// deployments hold its cards and yield policy keeps them there: the launch
// never yields, so it waits for no one, or a holder never yields to it.
// launchHeld names them so the deploy form can say which policy to change,
// rather than reading as cards in use. Only the form's evaluation asks for it
// (Input.ExplainHolders); the controller's refusal does not pay for it.

// LaunchHeld is a launch that would fit were the awake deployments on the
// cards it would take asleep, when yield policy keeps it from waiting for
// them: the cards and each holding deployment once.
type LaunchHeld struct {
	GPUIndexes []int
	Holders    []HeldBy
}

// HeldBy is an awake deployment on cards a launch would take, with its yield
// policy (never for one that does not share cards).
type HeldBy struct {
	DeploymentID uint
	Deployment   string
	Policy       yieldpolicy.Policy
}

// launchHeld re-plans req with every awake instance among insts (the
// node's) asleep but the launching deployment's own (launching, zero for a
// newcomer), as launchYield does with those that share; nil when the launch
// would not fit even so, or yield policy is not what keeps it off: a launch
// that never yields is held only when a holder would yield to one that
// shares, and one that shares only when a holder does not yield to it (else
// launchYield has it). Holders' policies come from the cached sharing
// deployments; only the names of the ones that never yield are read, by id.
func launchHeld(st *store.Store, node *store.Node, insts []store.Instance, launching uint, policy string, req scheduler.Request, dtypeOK bool, dtypeReason string, view ledgerView) *LaunchHeld {
	if st == nil {
		return nil
	}
	if policy == "" {
		policy = yieldpolicy.Never
	}
	var awake []*store.Instance
	asleep := map[string]bool{}
	for i := range insts {
		in := &insts[i]
		if (launching != 0 && in.DeploymentID == launching) || view.excludes(in) || !HoldsCardsAwake(in) {
			continue
		}
		if InstanceSleepIntent(in) == 0 {
			awake = append(awake, in)
		}
		asleep[in.UUID] = true
	}
	if len(awake) == 0 {
		return nil
	}
	view.alsoAsleep = func(in *store.Instance) bool { return asleep[in.UUID] }
	snap := nodeSnapshot(st, node, dtypeOK, dtypeReason, view)
	result := scheduler.Schedule(req, snap)
	if !result.OK || result.Plan == nil {
		return nil
	}
	sharing := sharingByID(st)
	var ids, unshared []uint
	seen := map[uint]bool{}
	for _, in := range instancesOnCards(awake, result.Plan) {
		if seen[in.DeploymentID] {
			continue
		}
		seen[in.DeploymentID] = true
		ids = append(ids, in.DeploymentID)
		if sharing[in.DeploymentID] == nil {
			unshared = append(unshared, in.DeploymentID)
		}
	}
	names, err := st.DeploymentNames(unshared)
	if err != nil {
		return nil
	}
	out := &LaunchHeld{GPUIndexes: result.Plan.GPUIndexes}
	held := false
	for _, id := range ids {
		h := HeldBy{DeploymentID: id, Deployment: names[id].Name, Policy: yieldpolicy.Policy{Yield: yieldpolicy.Never}}
		if dep := sharing[id]; dep != nil {
			h = HeldBy{DeploymentID: id, Deployment: dep.Name, Policy: dep.Policy()}
		}
		if policy == yieldpolicy.Never {
			held = held || h.Policy.Yield != yieldpolicy.Never
		} else {
			held = held || !yieldpolicy.SharesCardsWith(policy, h.Policy.Yield)
		}
		out.Holders = append(out.Holders, h)
	}
	if !held {
		return nil
	}
	return out
}

// HeldRefusal is the refusal of a launch of policy that awake deployments
// keep off node's cards by yield policy. A launch that never yields is
// does_not_wait_for_yield: holders are the deployments that would yield to
// one that shares, blockers those that never yield. One that shares is
// held_by_non_yielding: blockers are the deployments that do not yield to it,
// holders the ones that do, each with the terms it gives way on
// (yieldpolicy.TermsOf), as waiting_for_yield names them. Every entry names
// its deployment's yield policy.
func HeldRefusal(node *store.Node, policy yieldpolicy.Policy, held *LaunchHeld) scheduler.Reason {
	if policy.Yield == "" {
		policy.Yield = yieldpolicy.Never
	}
	holders, blockers := []map[string]any{}, []map[string]any{}
	for _, h := range held.Holders {
		entry := map[string]any{"deployment": h.Deployment, "policy": h.Policy.Yield}
		switch {
		case policy.Yield == yieldpolicy.Never && h.Policy.Yield != yieldpolicy.Never:
			holders = append(holders, entry)
		case policy.Yield != yieldpolicy.Never && yieldpolicy.SharesCardsWith(policy.Yield, h.Policy.Yield):
			addTerms(entry, yieldpolicy.TermsOf(h.Policy, policy))
			holders = append(holders, entry)
		default:
			blockers = append(blockers, entry)
		}
	}
	code := scheduler.ReasonHeldByNonYielding
	if policy.Yield == yieldpolicy.Never {
		code = scheduler.ReasonDoesNotWaitForYield
	}
	return scheduler.Reason{Code: code, Params: map[string]any{
		"node": node.DisplayName(), "gpus": held.GPUIndexes, "holders": holders, "blockers": blockers,
	}}
}
