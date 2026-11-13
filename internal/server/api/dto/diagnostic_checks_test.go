package dto

import (
	"encoding/json"
	"testing"
)

func TestDiagnosticCheckWireShape(t *testing.T) {
	report := DiagnosticReport{Checks: []DiagnosticCheck{{ID: "read:7:3", Status: "blocked", Message: ProductMessage{Code: "MODEL_SOURCE_UNAVAILABLE", Params: map[string]any{"reason": "storage_access_required"}, Message: "Not checked"}, BlockedBy: []string{"mount:7:3"}}}, Truncated: true}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"checks":[{"id":"read:7:3","status":"blocked","message":{"code":"MODEL_SOURCE_UNAVAILABLE","params":{"reason":"storage_access_required"},"message":"Not checked"},"blocked_by":["mount:7:3"]}],"truncated":true}`
	if string(encoded) != want {
		t.Fatalf("wire shape=%s", encoded)
	}
	var decoded DiagnosticReport
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Checks) != 1 || decoded.Checks[0].BlockedBy[0] != "mount:7:3" || !decoded.Truncated {
		t.Fatalf("roundtrip=%#v", decoded)
	}
	check, err := json.Marshal(DiagnosticCheck{ID: "ready", Status: "passed", Message: ProductMessage{Code: "MODEL_SOURCE_UNAVAILABLE", Message: "Ready"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(check) != `{"id":"ready","status":"passed","message":{"code":"MODEL_SOURCE_UNAVAILABLE","message":"Ready"}}` {
		t.Fatalf("optional fields not omitted: %s", check)
	}
}
