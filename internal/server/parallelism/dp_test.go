package parallelism

import (
	"errors"
	"testing"
)

func TestGPUAllocationIncludesDataParallelCopies(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want int
	}{
		{"one node two copies", Spec{TP: 1, PP: 1, DP: 2}, 2},
		{"one node four cards", Spec{TP: 2, PP: 1, DP: 2}, 4},
		{"two nodes split copies", Spec{TP: 2, PP: 1, DP: 2, NNodes: 2}, 2},
		{"default unchanged", Spec{TP: 2}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.GPUsPerNode()
			if err != nil || got != tc.want {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
			if tc.spec.World() != tc.spec.Normalized().TP*tc.spec.Normalized().PP {
				t.Fatal("DP replicas diluted per-copy weights")
			}
		})
	}
}

func TestDataParallelDoesNotInventFractionalReplica(t *testing.T) {
	spec := (Spec{DP: 2, NNodes: 2}).Defaulted([]int{1, 1})
	if spec.TP != 1 || spec.PP != 1 {
		t.Fatalf("one DP replica per node should fit: %+v", spec)
	}
	if need, err := spec.GPUsPerNode(); err != nil || need != 1 {
		t.Fatalf("need = %d, %v", need, err)
	}
	blocked, err := (Spec{PP: 2, DP: 2, NNodes: 2}).DefaultedChecked([]int{1, 1})
	if !errors.Is(err, ErrIndivisible) || errors.Is(err, ErrTPRequired) {
		t.Fatalf("explicit PP2 DP2 on one-card nodes = %+v, %v; want ErrIndivisible", blocked, err)
	}
	assigned := (Spec{DP: 2}).Assigned(1)
	if assigned.TP != 1 {
		t.Fatalf("assigned fractional replica: %+v", assigned)
	}
	if need, err := assigned.GPUsPerNode(); err != nil || need != 2 {
		t.Fatalf("need = %d, %v", need, err)
	}
}

func TestDefaultedCheckedWithStatedPPAndDP(t *testing.T) {
	for _, tc := range []struct {
		name        string
		spec        Spec
		nodes       []int
		wantTP      int
		wantPerNode int
		wantErr     error
	}{
		{name: "explicit PP and DP fit", spec: Spec{PP: 2, DP: 2}, nodes: []int{2, 2}, wantTP: 1, wantPerNode: 2},
		{name: "explicit PP and DP do not fit", spec: Spec{PP: 2, DP: 2}, nodes: []int{1, 1}, wantErr: ErrIndivisible},
		{name: "three DP copies fit three nodes", spec: Spec{PP: 1, DP: 3}, nodes: []int{1, 1, 1}, wantTP: 1, wantPerNode: 1},
		{name: "three DP copies do not fit two nodes", spec: Spec{PP: 1, DP: 3}, nodes: []int{1, 1}, wantErr: ErrIndivisible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.DefaultedChecked(tc.nodes)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("DefaultedChecked = %+v, %v; want %v", got, err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if got.TP != tc.wantTP {
				t.Fatalf("TP = %d, want %d", got.TP, tc.wantTP)
			}
			perNode, err := got.GPUsPerNode()
			if err != nil || perNode != tc.wantPerNode {
				t.Fatalf("GPUsPerNode = %d, %v; want %d", perNode, err, tc.wantPerNode)
			}
		})
	}
}
