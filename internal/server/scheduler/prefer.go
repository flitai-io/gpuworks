package scheduler

// Card preferences: kinds of card a placement avoids while it has a choice.
//
// A preference is not an admission rule. A card it avoids still takes the
// request; the planner only tries the cards without it first, and uses it
// when nothing else fits, saying so in the plan. Future preferences join the
// table; its order is their precedence.

// cardPreference is one kind of card placement avoids while it has a choice.
type cardPreference struct {
	// avoid is whether this card is of the kind, as the planner sees it for
	// this request.
	avoid func(GPU, Request) bool
	// reason is the Reason code a plan carries when it uses such cards.
	reason string
	// params states what the plan used: the cards of this kind in it.
	params func([]GPU) map[string]any
	// poolWide says the concession is worth looking at other nodes for
	// before accepting it (Concession.PoolWide).
	poolWide bool
}

// cardPreferences is ordered by precedence: entry 0 is avoided longest, so
// the tiers relax the last entry first. Sharing a card with processes
// outside the platform is avoided longest: the platform cannot see or bound
// that load, while a degraded card is one it watches (ADR 0068). Only it is
// worth trying other nodes for.
var cardPreferences = []cardPreference{
	{avoid: func(g GPU, _ Request) bool { return g.sharesForeignNow() }, reason: ReasonForeignSharedGPUsUsed, params: foreignUsedParams, poolWide: true},
	{avoid: func(g GPU, _ Request) bool { return g.Degraded }, reason: ReasonDegradedGPUsUsed, params: cardListParams},
}

// maxCardPreferences bounds the table: planPreferring may try 2^n tiers.
const maxCardPreferences = 4

// Concession is one preference a plan gave up: it uses cards of that kind.
// PoolWide marks one worth trying other nodes for (FitPicker in planning).
type Concession struct {
	Code     string
	PoolWide bool
}

// cardListParams names the cards of a kind a plan uses.
func cardListParams(used []GPU) map[string]any {
	indexes := make([]int, len(used))
	for i, g := range used {
		indexes[i] = g.Index
	}
	return map[string]any{"gpus": indexes, "count": len(used)}
}

// planPreferring plans on the cards no preference avoids whenever they can
// take the request, and relaxes the preferences one tier at a time
// otherwise. Tiers are the subsets of avoided kinds in lexicographic order of
// the table: with n entries, mask bit n-1-i stands for entry i, and masks run
// from all set down to none, so relaxing entry k keeps every entry before it
// avoided. A tier that leaves the same cards as one already tried is skipped.
func planPreferring(req Request, node NodeSnapshot) ([]Plan, []Rejected) {
	n := len(cardPreferences)
	kinds := make([]uint, len(node.GPUs))
	var present uint
	for i, g := range node.GPUs {
		for k, pref := range cardPreferences {
			if pref.avoid(g, req) {
				kinds[i] |= 1 << uint(n-1-k)
			}
		}
		present |= kinds[i]
	}
	if present == 0 {
		return plan(req, node)
	}
	tried := map[uint]bool{}
	var plans []Plan
	var rejected []Rejected
	for mask := uint(1)<<uint(n) - 1; ; mask-- {
		effective := mask & present
		if !tried[effective] {
			tried[effective] = true
			tier := node
			tier.GPUs = nil
			for i, g := range node.GPUs {
				if kinds[i]&effective == 0 {
					tier.GPUs = append(tier.GPUs, g)
				}
			}
			if plans, rejected = plan(req, tier); len(plans) > 0 {
				break
			}
		}
		if mask == 0 {
			break
		}
	}
	byIndex := make(map[int]GPU, len(node.GPUs))
	for _, g := range node.GPUs {
		byIndex[g.Index] = g
	}
	for i := range plans {
		for _, pref := range cardPreferences {
			var used []GPU
			for _, index := range plans[i].GPUIndexes {
				if g := byIndex[index]; pref.avoid(g, req) {
					used = append(used, g)
				}
			}
			if len(used) == 0 {
				continue
			}
			plans[i].Reasons = append(plans[i].Reasons, Reason{Code: pref.reason, Params: pref.params(used)})
			plans[i].Concessions = append(plans[i].Concessions, Concession{Code: pref.reason, PoolWide: pref.poolWide})
		}
	}
	return plans, rejected
}

// foreignUsedParams names the shared cards a plan uses and the most the
// processes outside the platform hold on any of them.
func foreignUsedParams(used []GPU) map[string]any {
	params := cardListParams(used)
	var most int64
	for _, g := range used {
		most = max(most, g.ForeignBytes)
	}
	params["foreign_gib"] = gib(most)
	return params
}
