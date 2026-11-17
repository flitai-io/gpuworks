package dto

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestEvaluateManualCommandDraftWireContract(t *testing.T) {
	response := EvaluateResponse{
		OK: true, PlanScope: "per_replica",
		ManualCommandDraft:     "set -eu\ncase \"${GPUWORKS_HEADLESS}\" in\n  0)\n    exec vllm serve /model\n    ;;\nesac",
		ManualCommandVariables: []string{"GPUWORKS_HEADLESS", "GPUWORKS_NODE_RANK"},
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	literal := string(raw)
	for _, field := range []string{
		`"manual_command_draft":"set -eu\ncase \"${GPUWORKS_HEADLESS}\" in\n  0)\n    exec vllm serve /model\n    ;;\nesac"`,
		`"manual_command_variables":["GPUWORKS_HEADLESS","GPUWORKS_NODE_RANK"]`,
	} {
		if !strings.Contains(literal, field) {
			t.Fatalf("response missing literal wire field %s: %s", field, literal)
		}
	}
	var round EvaluateResponse
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	if round.ManualCommandDraft != response.ManualCommandDraft || !slices.Equal(round.ManualCommandVariables, response.ManualCommandVariables) {
		t.Fatalf("round trip = %q %v", round.ManualCommandDraft, round.ManualCommandVariables)
	}

	// A single launch has neither: the keys are absent, not empty.
	raw, err = json.Marshal(EvaluateResponse{OK: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"manual_command_draft"`, `"manual_command_variables"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("single-launch response states %s: %s", key, raw)
		}
	}
}
