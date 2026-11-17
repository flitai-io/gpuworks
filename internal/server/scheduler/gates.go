package scheduler

import "gpuworks.ai/gpuworks/internal/server/nodepolicy"

// Card gates: conditions that withhold a card a request could otherwise use.
//
// A gate never changes what the snapshot says a card is (Schedulable,
// ForeignShareable and the rest are facts the planner reads); it marks the
// card withheld for this request (GPU.withheld), and the single admission
// predicate (GPU.takesWorkFor) refuses a withheld card. The refusal names the
// first gate that withheld a card, so the table's order is the order in which
// an operator is told what stands in the way. Future card-level conditions
// join the table.

// cardGate withholds a card a request could otherwise use.
type cardGate struct {
	// name is the gate's stable id, lower_snake.
	name string
	// applies is whether the card is subject to the gate for this request;
	// it may be any card, Schedulable or not.
	applies func(GPU, Request) bool
	// consented lets the card through when the node's policy or the
	// request's own consent allows it; nil for a gate nothing lets through.
	// It asks the policy's methods (nodepolicy), never its fields, so the
	// rule it applies is stated once.
	consented func(NodePolicy, Request) bool
	// refusal explains the cards on node the gate withholds.
	refusal func(req Request, node string, withheld []GPU) Reason
}

// cardGates is ordered by precedence: the first gate that applies to a card
// withholds it.
var cardGates = []cardGate{sleepBesideForeignGate, foreignSharingGate}

// sleepBesideForeignGate keeps a deployment that sleeps off a card it would
// share with processes outside the platform: its wake would be refused for
// as long as they hold the card (ADR 0068).
var sleepBesideForeignGate = cardGate{
	name: "sleep_beside_foreign",
	applies: func(g GPU, req Request) bool {
		return g.sharesForeignWith(req) && !g.Schedulable && req.sleeps()
	},
	refusal: func(Request, string, []GPU) Reason { return Reason{Code: ReasonHeldOutsidePlatformSleep} },
}

// foreignSharingGate keeps a memory-limited partition off a card processes
// outside the platform hold unless the node's placement policy allows it or
// the operator chose the card (nodepolicy.Policy.SharesForeign). The card
// can be shared - the agent reports the holders' memory - so the refusal
// names the setting that would let it (ADR 0068).
var foreignSharingGate = cardGate{
	name: "foreign_sharing",
	applies: func(g GPU, req Request) bool {
		return g.sharesForeignWith(req) && !g.Schedulable
	},
	consented: func(policy NodePolicy, req Request) bool { return policy.SharesForeign(req.Consent) },
	refusal: func(_ Request, node string, withheld []GPU) Reason {
		params := cardListParams(withheld)
		params["setting"], params["node"] = string(nodepolicy.KeyShareWithForeign), node
		return Reason{Code: ReasonHeldOutsidePlatformNotAllowed, Params: params}
	},
}

// resolveGates returns node with each card marked by the first gate that
// withholds it from req. The cards are copied; the caller's snapshot is not
// changed.
func resolveGates(req Request, node NodeSnapshot) NodeSnapshot {
	return resolveGatesConsenting(req, node, false)
}

// resolveGatesConsenting is resolveGates, with every gate that can be
// consented to treated as consented when consentAll is set: what the node
// would look like were the operator to allow it (consentRetry).
func resolveGatesConsenting(req Request, node NodeSnapshot, consentAll bool) NodeSnapshot {
	gpus := make([]GPU, len(node.GPUs))
	for i, g := range node.GPUs {
		g.withheld = 0
		for k, gate := range cardGates {
			if !gate.applies(g, req) {
				continue
			}
			if gate.consented != nil && (consentAll || gate.consented(node.Policy, req)) {
				continue
			}
			g.withheld = k + 1
			break
		}
		gpus[i] = g
	}
	node.GPUs = gpus
	return node
}

// consentRetry explains a refusal through the gates that consent would
// lift, by planning once more in memory as if consented - only for a request
// that was refused, so a placement that fits costs nothing more. When cards
// were withheld by such a gate and the request would then be placed, the
// refusal is that gate's, naming the setting, rather than the shortage the
// withheld cards caused (a busy clean card beside held ones). When it would
// not be placed even so, a refusal that named the gate gives way to the
// reason that would remain: allowing it would not help.
func consentRetry(req Request, node, gated NodeSnapshot, refused Reason) (Reason, bool) {
	first := 0
	for _, g := range gated.GPUs {
		if g.withheld > 0 && cardGates[g.withheld-1].consented != nil && (first == 0 || g.withheld < first) {
			first = g.withheld
		}
	}
	if first == 0 {
		return Reason{}, false
	}
	retried := schedule(req, resolveGatesConsenting(req, node, true))
	if retried.OK {
		return gateRefusal(req, node.UUID, pinnedGPUs(gated.GPUs, req), GPU{withheld: first}), true
	}
	for _, gate := range cardGates {
		if gate.consented != nil && refused.Code == gate.refusal(req, node.UUID, nil).Code {
			return retried.Message, true
		}
	}
	return Reason{}, false
}

// gateRefusal explains the cards of gpus that the gate which withheld first
// withholds.
func gateRefusal(req Request, node string, gpus []GPU, first GPU) Reason {
	gate := cardGates[first.withheld-1]
	var withheld []GPU
	for _, g := range gpus {
		if g.withheld == first.withheld {
			withheld = append(withheld, g)
		}
	}
	return gate.refusal(req, node, withheld)
}
