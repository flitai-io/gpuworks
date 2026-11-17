package planning

import "gpuworks.ai/gpuworks/internal/server/scheduler"

// FitPicker chooses the node a pool placement takes among the candidates it
// plans in rank order. Placement is first fit: a plan that gives up no card
// preference worth looking elsewhere for (scheduler.Concession.PoolWide) is
// taken at once. A plan that does is kept as a fallback while the search
// goes on within the trial budget, so a node that fits on preferred cards
// wins over a better-ranked one that fits only by a pool-wide concession.
// The controller's pool placement and the deploy form's automatic preview
// both pick through it, so they agree on the node.
type FitPicker[T any] struct {
	fallback T
	weight   int
	has      bool
}

// Offer reports whether the candidate whose plan made concessions is taken
// now. When it is not, the picker keeps it if it is the best fallback so
// far: the fewest pool-wide concessions, the earlier offer on a tie.
func (p *FitPicker[T]) Offer(candidate T, concessions []scheduler.Concession) bool {
	weight := 0
	for _, concession := range concessions {
		if concession.PoolWide {
			weight++
		}
	}
	if weight == 0 {
		return true
	}
	if !p.has || weight < p.weight {
		p.fallback, p.weight, p.has = candidate, weight, true
	}
	return false
}

// Fallback is the best candidate offered that was not taken at once; ok is
// false when there is none. A caller takes it once the search is over,
// before waiting for holders to yield: a node that fits now beats one that
// fits later.
func (p *FitPicker[T]) Fallback() (candidate T, ok bool) {
	return p.fallback, p.has
}
