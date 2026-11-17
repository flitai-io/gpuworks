package dto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPreflightSummaryWireShape(t *testing.T) {
	summary := PreflightSummary{OK: false, Checks: []PreflightCheck{
		{ID: "node_count", Status: "pass", Code: "PREFLIGHT_NODE_COUNT_OK", Params: map[string]any{"count": 2}, Message: "2 nodes selected"},
		{ID: "node_list", Status: "fail", Code: "PREFLIGHT_DUPLICATE_NODE", Params: map[string]any{"node": "a"}, Message: "duplicate node UUID a"},
		{ID: "port_range", Status: "skip", Code: "PREFLIGHT_BLOCKED", Params: map[string]any{"blocked_by": "node_list"}, Message: "not checked", BlockedBy: []string{"node_list"}},
		{ID: "shared_network", Status: "fail", Code: "PREFLIGHT_SHARED_NETWORK_UNKNOWN", Evidence: "lookup failed"},
	}}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"ok":false,"checks":[` +
		`{"id":"node_count","status":"pass","code":"PREFLIGHT_NODE_COUNT_OK","params":{"count":2},"message":"2 nodes selected"},` +
		`{"id":"node_list","status":"fail","code":"PREFLIGHT_DUPLICATE_NODE","params":{"node":"a"},"message":"duplicate node UUID a"},` +
		`{"id":"port_range","status":"skip","code":"PREFLIGHT_BLOCKED","params":{"blocked_by":"node_list"},"message":"not checked","blocked_by":["node_list"]},` +
		`{"id":"shared_network","status":"fail","code":"PREFLIGHT_SHARED_NETWORK_UNKNOWN","evidence":"lookup failed"}]}`
	if string(encoded) != want {
		t.Fatalf("wire shape\n got %s\nwant %s", encoded, want)
	}
	var decoded PreflightSummary
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatal(err)
	}
	summary.Checks[0].Params["count"] = float64(2)
	if !reflect.DeepEqual(decoded, summary) {
		t.Fatalf("round trip\n got %#v\nwant %#v", decoded, summary)
	}
	clean, _ := json.Marshal(PreflightSummary{OK: true})
	if string(clean) != `{"ok":true}` {
		t.Fatalf("empty summary = %s", clean)
	}
}
