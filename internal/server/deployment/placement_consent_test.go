package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The cards a manual draft names under "gpus" are the operator's choice; an
// automatic draft's cards are the scheduler's, whatever it echoes back.
func TestEvaluationPlacementConsentsOnlyForManualCards(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  dto.DeployRequest
		want bool
	}{
		{"manual, gpus", dto.DeployRequest{PlacementMode: "manual", Placement: map[string]any{"gpus": []any{float64(0)}}}, true},
		{"manual, echoed gpu_indexes", dto.DeployRequest{PlacementMode: "manual", Placement: map[string]any{"gpu_indexes": []any{float64(0)}}}, false},
		{"automatic, gpus", dto.DeployRequest{PlacementMode: "auto", Placement: map[string]any{"gpus": []any{float64(0)}}}, false},
		{"manual, no cards", dto.DeployRequest{PlacementMode: "manual", Placement: map[string]any{"tp": float64(1)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evaluationPlacement(&tc.req).OperatorPinned(); got != tc.want {
				t.Fatalf("operator pinned = %v, want %v", got, tc.want)
			}
		})
	}
}

// The create's launch preflight carries the placement the create revalidates
// an automatic draft with, so it judges consent as the evaluation did: an
// automatic draft's echoed cards are not the operator's choice, else the
// create would admit a held card the evaluation and the controller refuse.
func TestCreatePreflightPlacementConsentsOnlyForManualCards(t *testing.T) {
	st, err := store.Open("file:deployment_preflight_consent?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Deps{Store: st})
	for _, tc := range []struct {
		name string
		mode string
		want bool
	}{
		{"manual", "manual", true},
		{"automatic", "auto", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := dto.DeployRequest{PlacementMode: tc.mode, ServedName: "consent", Placement: map[string]any{"tp": float64(1), "gpus": []any{float64(0)}}}
			preflight, err := s.prepareLaunchPreflight(nil, &req, templateSelection{}, deploy.Materialized{}, 1, "nvidia", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := preflight.placement.OperatorPinned(); got != tc.want {
				t.Fatalf("operator pinned = %v, want %v", got, tc.want)
			}
		})
	}
}
