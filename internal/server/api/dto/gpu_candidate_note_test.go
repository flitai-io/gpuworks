package dto

import (
	"encoding/json"
	"testing"
)

// An offered card that processes outside the platform share carries a note;
// a clean card carries none on the wire.
func TestGPUCandidateNoteWire(t *testing.T) {
	shared := GPUCandidate{Index: 0, Available: true, Note: &ScheduleReason{Code: "foreign_shared_card", Params: map[string]any{"foreign_gib": "62.5"}}}
	raw, err := json.Marshal([]GPUCandidate{shared, {Index: 1, Available: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"index":0,"available":true,"note":{"code":"foreign_shared_card","params":{"foreign_gib":"62.5"}}},{"index":1,"available":true}]`
	if string(raw) != want {
		t.Fatalf("wire = %s\nwant   %s", raw, want)
	}
}
