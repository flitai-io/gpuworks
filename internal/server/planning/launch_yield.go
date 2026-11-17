package planning

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// A replica of a deployment that shares cards by yield policy may start on
// cards other such deployments hold awake: it waits for them as a request
// waits for a sleeping deployment, and the residency arbiter puts them to
// sleep by the same rule (docs/explanation/service-levels.md). launchYield
// is the planning side: whether the launch fits with them asleep, and which.

// LaunchYield is how a launch that does not fit would: the awake instances
// on the cards it would take, of deployments that share them with it, put to
// sleep. No holders is a launch that waits only for sleeps already ordered;
// the cards are its all the same.
type LaunchYield struct {
	Holders []YieldHolder
	// NodeID and GPUIndexes are the cards it would take.
	NodeID     uint
	GPUIndexes []int
}

// HolderInstances are the awake instances the launch waits for.
func (y *LaunchYield) HolderInstances() []string {
	out := make([]string, 0, len(y.Holders))
	for _, h := range y.Holders {
		out = append(out, h.Instance)
	}
	return out
}

// YieldHolder is an awake instance a launch waits for, and its deployment.
type YieldHolder struct {
	DeploymentID uint
	Deployment   string
	Policy       yieldpolicy.Policy
	Instance     string
}

// InstanceStarting says the instance has not started serving yet: it is
// placed and on its way up (pending, pulling, starting, optimizing).
func InstanceStarting(in *store.Instance) bool {
	switch in.State {
	case proto.InstPending, proto.InstPulling, proto.InstStarting, proto.InstOptimizing:
		return true
	}
	return false
}

// HoldsCardsAwake says the instance holds its cards awake for a launch to
// wait for: running or still starting, and reporting no sleep. One still
// starting is waited for all the same, as a wake waits for a starting
// occupant (YieldTargets); the arbiter asks it nothing until it serves.
func HoldsCardsAwake(in *store.Instance) bool {
	return (in.State == proto.InstRunning || InstanceStarting(in)) && in.SleepLevel == 0
}

// sharingByID are the deployments that share cards by yield policy
// (store.Store.SharingDeployments, cached), by id; nil when there are none
// or they cannot be read.
func sharingByID(st *store.Store) map[uint]*store.Deployment {
	sharing, err := st.SharingDeployments()
	if err != nil || len(sharing) == 0 {
		return nil
	}
	out := make(map[uint]*store.Deployment, len(sharing))
	for i := range sharing {
		out[sharing[i].ID] = &sharing[i]
	}
	return out
}

// instancesOnCards are the instances of candidates that hold one of the
// cards a plan took, in candidates' order.
func instancesOnCards(candidates []*store.Instance, plan *scheduler.Plan) []*store.Instance {
	cards := make(map[int]bool, len(plan.GPUIndexes))
	for _, index := range plan.GPUIndexes {
		cards[index] = true
	}
	var out []*store.Instance
	for _, in := range candidates {
		for _, index := range parseInts(in.GPUIndexes) {
			if cards[index] {
				out = append(out, in)
				break
			}
		}
	}
	return out
}

// launchYield re-plans req with the awake instances among insts (the
// node's) of the deployments that share cards with a deployment of policy
// asleep, as the ledger will charge them once they have slept; nil when
// policy shares with none, nothing would sleep, or the launch would not fit
// even so. Sharing deployments are single-node, so an instance is a whole
// replica. The launching deployment's own instances (launching, zero for a
// newcomer) give way to nothing of their own: a replica never waits for its
// sibling, so they are neither holders nor counted asleep.
func launchYield(st *store.Store, node *store.Node, insts []store.Instance, launching uint, policy string, req scheduler.Request, dtypeOK bool, dtypeReason string, view ledgerView) *LaunchYield {
	if policy == "" || policy == yieldpolicy.Never {
		return nil
	}
	shares := sharingByID(st)
	for id, dep := range shares {
		if id == launching || !yieldpolicy.SharesCardsWith(policy, dep.YieldPolicy) {
			delete(shares, id)
		}
	}
	if len(shares) == 0 {
		return nil
	}
	// The awake ones are what it waits for; one already ordered to sleep is
	// counted asleep too, as it will be in a moment, but waits for nothing.
	var awake []*store.Instance
	asleep := map[string]bool{}
	for i := range insts {
		in := &insts[i]
		if shares[in.DeploymentID] == nil || view.excludes(in) || !HoldsCardsAwake(in) {
			continue
		}
		if InstanceSleepIntent(in) == 0 {
			awake = append(awake, in)
		}
		asleep[in.UUID] = true
	}
	if len(asleep) == 0 {
		return nil
	}
	view.alsoAsleep = func(in *store.Instance) bool { return asleep[in.UUID] }
	snap := nodeSnapshot(st, node, dtypeOK, dtypeReason, view)
	result := scheduler.Schedule(req, snap)
	if !result.OK || result.Plan == nil {
		return nil
	}
	out := &LaunchYield{NodeID: node.ID, GPUIndexes: result.Plan.GPUIndexes}
	for _, in := range instancesOnCards(awake, result.Plan) {
		dep := shares[in.DeploymentID]
		out.Holders = append(out.Holders, YieldHolder{DeploymentID: dep.ID, Deployment: dep.Name, Policy: dep.Policy(), Instance: in.UUID})
	}
	return out
}

// YieldRefusal is the refusal of a launch that waits for holders to give up
// node's cards: each holder deployment once, with the terms it gives way to
// a deployment of policy on (yieldpolicy.TermsOf), and the instances. The
// params are for display; what the launch waits for is the LaunchYield
// itself (LaunchPlan.YieldWait).
func YieldRefusal(node *store.Node, policy yieldpolicy.Policy, yield *LaunchYield) scheduler.Reason {
	holders := []map[string]any{}
	seen := map[uint]bool{}
	for _, h := range yield.Holders {
		if seen[h.DeploymentID] {
			continue
		}
		seen[h.DeploymentID] = true
		entry := map[string]any{"deployment": h.Deployment}
		addTerms(entry, yieldpolicy.TermsOf(h.Policy, policy))
		holders = append(holders, entry)
	}
	return scheduler.Reason{Code: scheduler.ReasonWaitingForYield, Params: map[string]any{
		"node": node.DisplayName(), "holders": holders, "instances": yield.HolderInstances(),
	}}
}

// addTerms states in a holder's entry the terms it gives way on: at_once, or
// idle_sec and, taking turns, max_serve_sec.
func addTerms(entry map[string]any, terms yieldpolicy.Terms) {
	entry["at_once"] = terms.AtOnce
	if !terms.AtOnce {
		entry["idle_sec"] = int(terms.Idle / time.Second)
		if terms.Turn > 0 {
			entry["max_serve_sec"] = int(terms.Turn / time.Second)
		}
	}
}
