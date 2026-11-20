package planning

import (
	"context"
	"errors"
	"math"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

var ErrNoDistributedLayout = errors.New("no layout satisfies the stated degrees and node capacities")

// DistributedTrial is how one candidate layout planned on every rank.
type DistributedTrial struct {
	// Preference is the lowest catalog placement preference among the ranks.
	Preference int64
	// ContextShort reports that a rank's KV estimate cannot hold the context
	// length. The layout stays admissible (WarningPlanningKVBelowContext is
	// deliberately non-blocking), but one without the shortfall is preferred.
	ContextShort bool
}

func (t DistributedTrial) beats(other DistributedTrial) bool {
	if t.ContextShort != other.ContextShort {
		return !t.ContextShort
	}
	return t.Preference > other.Preference
}

// ContextShort reports the plan's non-blocking KV-below-context warning.
func (o Output) ContextShort() bool {
	for _, warning := range o.Warnings {
		if warning.Code == messages.WarningPlanningKVBelowContext {
			return true
		}
	}
	return false
}

// PlanDistributedParallelism tries complete layouts in increasing card count,
// doubling the cards per node (1, 2, 4, ...) as the scheduler sizes a single
// node. At equal size prefer a pipeline stage per node, keeping tensor
// communication local. A layout whose KV estimate holds the context beats one that does not;
// a catalog preference, when present, then takes precedence over packing.
func PlanDistributedParallelism(ctx context.Context, hint ParallelismSpec, cardsPerNode, layers int, catalogPreference bool, trial func(ParallelismSpec) (DistributedTrial, error)) (ParallelismSpec, error) {
	if hint.NNodes < 2 || cardsPerNode < 1 || cardsPerNode > math.MaxInt/hint.NNodes {
		return ParallelismSpec{}, ErrNoDistributedLayout
	}
	hint.DP = max(hint.DP, 1)
	var best ParallelismSpec
	var chosen DistributedTrial
	var last error
	for cards := 1; cards > 0 && cards <= cardsPerNode; cards *= 2 {
		if err := ctx.Err(); err != nil {
			return ParallelismSpec{}, err
		}
		world := cards * hint.NNodes
		if world%hint.DP != 0 {
			continue
		}
		shards := world / hint.DP
		try := func(pp int) bool {
			if pp < 1 || (layers > 0 && pp > layers) || shards%pp != 0 || (hint.PP > 0 && hint.PP != pp) {
				return false
			}
			spec := hint
			spec.TP, spec.PP = shards/pp, pp
			if hint.TP > 0 && hint.TP != spec.TP {
				return false
			}
			result, err := trial(spec)
			if err != nil {
				last = err
				return false
			}
			if !best.Stated() || result.beats(chosen) {
				best, chosen = spec, result
			}
			return !catalogPreference && !result.ContextShort
		}
		if try(hint.NNodes) {
			return best, ctx.Err()
		}
		for pp := 1; pp <= shards; pp++ {
			if err := ctx.Err(); err != nil {
				return ParallelismSpec{}, err
			}
			if pp != hint.NNodes && try(pp) {
				return best, ctx.Err()
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return ParallelismSpec{}, err
	}
	if best.Stated() {
		return best, nil
	}
	if last == nil {
		last = ErrNoDistributedLayout
	}
	return ParallelismSpec{}, last
}
