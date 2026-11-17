package planning

import "testing"

// Cards an operator chose arrive under the "gpus" key; "gpu_indexes" is the
// confirmed plan an older client echoes back, which is the scheduler's own
// choice. Only the first is the operator's (ADR 0068, decision 10).
func TestOperatorPinnedOnlyForTheGPUsKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		document any
		want     bool
	}{
		{"gpus key from a request", map[string]any{"gpus": []any{float64(0)}}, true},
		{"gpus key from a stored row", `{"gpus":[1,2],"tp":2}`, true},
		{"echoed plan", map[string]any{"gpu_indexes": []any{float64(0)}}, false},
		{"echoed plan in a stored row", `{"gpu_indexes":[0]}`, false},
		{"empty gpus", map[string]any{"gpus": []any{}}, false},
		{"nothing pinned", `{"tp":2}`, false},
		{"constructed placement", Placement{GPUIndexes: []int{0}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			placement, err := ParsePlacement(tc.document)
			if err != nil {
				t.Fatal(err)
			}
			if got := placement.OperatorPinned(); got != tc.want {
				t.Fatalf("OperatorPinned = %v, want %v (%+v)", got, tc.want, placement)
			}
		})
	}
}

// Clearing the indexes - a replica the pool places elsewhere, a per-card
// capability probe - clears the operator's choice with them.
func TestClearedIndexesAreNoLongerOperatorPinned(t *testing.T) {
	placement, err := ParsePlacement(`{"gpus":[0]}`)
	if err != nil {
		t.Fatal(err)
	}
	placement.GPUIndexes = nil
	if placement.OperatorPinned() {
		t.Fatal("a placement with no indexes reads as pinned")
	}
	probe, _ := ParsePlacement(`{"gpus":[0]}`)
	probe = probe.WithoutOperatorChoice()
	probe.GPUIndexes = []int{3}
	if probe.OperatorPinned() {
		t.Fatal("a probe's indexes read as the operator's")
	}
}
