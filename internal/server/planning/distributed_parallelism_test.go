package planning

import (
	"context"
	"errors"
	"testing"
)

func TestDistributedParallelismRecommendation(t *testing.T) {
	for _, tc := range []struct {
		name                                              string
		tp, pp, dp, minCards, preferCards, wantTP, wantPP int
	}{
		{"small model", 0, 0, 1, 1, 0, 1, 2},
		{"larger model needs four GPUs", 0, 0, 1, 2, 0, 2, 2},
		{"fixed pipeline across nodes", 0, 1, 1, 1, 0, 2, 1},
		{"fixed tensor degree", 2, 0, 1, 1, 0, 2, 1},
		{"data parallel replicas", 0, 0, 2, 1, 0, 1, 1},
		{"template preference", 0, 0, 1, 1, 4, 4, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanDistributedParallelism(t.Context(), ParallelismSpec{NNodes: 2, DP: tc.dp, TP: tc.tp, PP: tc.pp}, 8, 28, tc.preferCards > 0, func(spec ParallelismSpec) (DistributedTrial, error) {
				width, err := spec.GPUsPerNode()
				if err != nil || width < tc.minCards {
					return DistributedTrial{}, errors.New("insufficient memory")
				}
				if width == tc.preferCards {
					return DistributedTrial{Preference: 1}, nil
				}
				return DistributedTrial{}, nil
			})
			if err != nil || got.TP != tc.wantTP || got.PP != tc.wantPP {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

// A KV estimate below the context length does not refuse a layout, but a
// larger one that holds the context wins over packing and catalog preference.
func TestDistributedRecommendationPrefersLayoutHoldingContext(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		contextCards, preferCards, wantTP, wantPP int
	}{
		{"context needs two cards per node", 2, 0, 2, 2},
		{"no layout holds the context", 99, 0, 1, 2},
		{"preferred layout is short of context", 4, 2, 4, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanDistributedParallelism(t.Context(), ParallelismSpec{NNodes: 2, DP: 1}, 8, 28, tc.preferCards > 0, func(spec ParallelismSpec) (DistributedTrial, error) {
				width, err := spec.GPUsPerNode()
				if err != nil {
					return DistributedTrial{}, err
				}
				result := DistributedTrial{ContextShort: width < tc.contextCards}
				if width == tc.preferCards {
					result.Preference = 1
				}
				return result, nil
			})
			if err != nil || got.TP != tc.wantTP || got.PP != tc.wantPP {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestDistributedRecommendationStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	_, err := PlanDistributedParallelism(ctx, ParallelismSpec{NNodes: 2, DP: 1}, 8, 28, false, func(ParallelismSpec) (DistributedTrial, error) {
		calls++
		cancel()
		return DistributedTrial{}, errors.New("occupied")
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
