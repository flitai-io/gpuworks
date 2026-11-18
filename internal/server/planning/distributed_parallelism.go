package planning

import (
	"context"
	"errors"
	"math"
)

var ErrNoDistributedLayout = errors.New("no layout satisfies the stated degrees and node capacities")

// PlanDistributedParallelism tries complete layouts in increasing card count.
// At equal size prefer a pipeline stage per node, keeping tensor communication
// local. A catalog preference, when present, takes precedence over packing.
func PlanDistributedParallelism(ctx context.Context, hint ParallelismSpec, cardsPerNode, layers int, catalogPreference bool, trial func(ParallelismSpec) (int64, error)) (ParallelismSpec, error) {
	if hint.NNodes < 2 || cardsPerNode < 1 || cardsPerNode > math.MaxInt/hint.NNodes {
		return ParallelismSpec{}, ErrNoDistributedLayout
	}
	hint.DP = max(hint.DP, 1)
	var best ParallelismSpec
	var last error
	score := int64(math.MinInt64)
	for cards := 1; cards <= cardsPerNode; cards++ {
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
			preference, err := trial(spec)
			if err != nil {
				last = err
				return false
			}
			if !best.Stated() || preference > score {
				best, score = spec, preference
			}
			return !catalogPreference
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
