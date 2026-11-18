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
			got, err := PlanDistributedParallelism(t.Context(), ParallelismSpec{NNodes: 2, DP: tc.dp, TP: tc.tp, PP: tc.pp}, 8, 28, tc.preferCards > 0, func(spec ParallelismSpec) (int64, error) {
				width, err := spec.GPUsPerNode()
				if err != nil || width < tc.minCards {
					return 0, errors.New("insufficient memory")
				}
				if width == tc.preferCards {
					return 1, nil
				}
				return 0, nil
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
	_, err := PlanDistributedParallelism(ctx, ParallelismSpec{NNodes: 2, DP: 1}, 8, 28, false, func(ParallelismSpec) (int64, error) {
		calls++
		cancel()
		return 0, errors.New("occupied")
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
