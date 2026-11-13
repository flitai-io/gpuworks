package parallelism

import (
	"errors"
	"testing"
)

func TestGPUsPerNode(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want int
		err  error
	}{
		{"single node sized by the scheduler", Spec{}, 0, nil},
		{"single node TP", Spec{TP: 4}, 4, nil},
		{"single node TP×PP", Spec{TP: 2, PP: 2}, 4, nil},
		{"TP spans two one-GPU nodes", Spec{TP: 2, NNodes: 2}, 1, nil},
		{"one pipeline stage per node", Spec{TP: 8, PP: 2, NNodes: 2}, 8, nil},
		{"TP and PP both span nodes", Spec{TP: 4, PP: 2, NNodes: 4}, 2, nil},
		{"multi-node needs TP", Spec{PP: 2, NNodes: 2}, 0, ErrTPRequired},
		{"ranks not shared equally", Spec{TP: 3, NNodes: 2}, 0, ErrIndivisible},
		{"defaults below one", Spec{TP: 2, PP: -1, NNodes: 0}, 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.GPUsPerNode()
			if got != tc.want || !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Fatalf("GPUsPerNode(%+v) = %d, %v; want %d, %v", tc.spec, got, err, tc.want, tc.err)
			}
		})
	}
}

func TestDefaulted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  Spec
		nodes []int
		want  Spec
	}{
		{"a stage per node, TP over the fewest GPUs", Spec{}, []int{8, 4}, Spec{TP: 4, PP: 2, DP: 1, NNodes: 2}},
		{"one-GPU nodes", Spec{}, []int{1, 1}, Spec{TP: 1, PP: 2, DP: 1, NNodes: 2}},
		{"a stated PP keeps the per-node share", Spec{PP: 1}, []int{2, 2}, Spec{TP: 4, PP: 1, DP: 1, NNodes: 2}},
		{"a PP that cannot split the GPUs falls back", Spec{PP: 3}, []int{2, 2}, Spec{TP: 2, PP: 2, DP: 1, NNodes: 2}},
		{"a stated TP is kept", Spec{TP: 2, NNodes: 2}, []int{8, 8}, Spec{TP: 2, PP: 1, DP: 1, NNodes: 2}},
		{"a single node is left to the scheduler", Spec{}, []int{8}, Spec{PP: 1, DP: 1, NNodes: 1}},
		{"a node without GPUs", Spec{NNodes: 2}, []int{2, 0}, Spec{PP: 1, DP: 1, NNodes: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.Defaulted(tc.nodes); got != tc.want {
				t.Fatalf("Defaulted(%v) = %+v, want %+v", tc.nodes, got, tc.want)
			}
		})
	}
}

func TestAssignedAndWorld(t *testing.T) {
	if got := (Spec{}).Assigned(4); got != (Spec{TP: 4, PP: 1, DP: 1, NNodes: 1}) || got.World() != 4 {
		t.Fatalf("unstated on 4 GPUs = %+v", got)
	}
	if got := (Spec{PP: 2}).Assigned(4); got != (Spec{TP: 2, PP: 2, DP: 1, NNodes: 1}) {
		t.Fatalf("PP 2 on 4 GPUs = %+v", got)
	}
	if got := (Spec{PP: 3}).Assigned(4); got != (Spec{TP: 4, PP: 1, DP: 1, NNodes: 1}) {
		t.Fatalf("PP that does not divide falls back to 1: %+v", got)
	}
	if got := (Spec{TP: 2, NNodes: 2}).Assigned(1); got != (Spec{TP: 2, PP: 1, DP: 1, NNodes: 2}) || got.World() != 2 {
		t.Fatalf("a stated spec is kept: %+v", got)
	}
	if got := (Spec{}).Assigned(0); got.TP != 1 {
		t.Fatalf("no GPU count still yields one rank: %+v", got)
	}
}
