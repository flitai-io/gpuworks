package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A label run records and returns the label it selected nodes by, as two
// fields; a nodes or all run carries neither, and no run carries scope_detail.
// Every run returns the nodes under test as node_uuids, sorted as stored,
// already while it is running and has no pairs.
func TestNetworkTestRunCarriesItsLabelOnly(t *testing.T) {
	n := newNetworkAPI(t)
	for uuid, pool := range map[string]string{"a": "a=b", "b": "a=b", "c": "other"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		if _, err := n.store.SetNodeLabels(uuid, map[string]string{"site": "x", "pool": pool}); err != nil {
			t.Fatal(err)
		}
	}
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"lan","roles":["storage_sync"],"type":"host_ethernet","cidr":"10.30.0.0/16"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"label_key":"site","label_value":"x"}`, http.StatusCreated)
	tests := "/api/v1/networks/1/tests"
	finish := func() *store.NetworkTestRun {
		t.Helper()
		run, err := n.store.LatestNetworkTestRun(1)
		if err != nil {
			t.Fatal(err)
		}
		if err := n.store.FinishNetworkTestRun(run.ID, "[]", 0, 0); err != nil {
			t.Fatal(err)
		}
		return run
	}
	runKeys := []string{"bandwidth", "id", "kind", "node_count", "node_uuids", "pair_count", "pairs", "partial_count", "passed_count", "pattern", "rail_count", "rail_passed_count", "scope", "started_at", "status"}
	labelKeys := []string{"bandwidth", "id", "kind", "label_key", "label_value", "node_count", "node_uuids", "pair_count", "pairs", "partial_count", "passed_count", "pattern", "rail_count", "rail_passed_count", "scope", "started_at", "status"}

	// start starts a run and checks the nodes under test on the wire, literally:
	// a running run has them before it has any pairs.
	start := func(body, wantNodes string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, tests, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+n.admin)
		rec := httptest.NewRecorder()
		n.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST %s = %d %s", body, rec.Code, rec.Body.String())
		}
		if want := `"node_uuids":` + wantNodes + `,"pairs":[],`; !strings.Contains(rec.Body.String(), want) || !strings.Contains(rec.Body.String(), `"status":"running"`) {
			t.Fatalf("POST %s = %s, want it to contain %s", body, rec.Body.String(), want)
		}
		var run map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		return run
	}
	latestNodes := func() any {
		t.Helper()
		return n.expect(n.member, http.MethodGet, "/api/v1/networks/1", "", http.StatusOK)["latest_test"].(map[string]any)["node_uuids"]
	}

	// A label key is still required.
	n.expectCode(http.MethodPost, tests, `{"scope":"label","label_value":"a=b"}`, http.StatusBadRequest, ErrorNetworkTestScopeInvalid)
	n.expectCode(http.MethodPost, tests, `{"scope":"label","label_key":"  ","label_value":"a=b"}`, http.StatusBadRequest, ErrorNetworkTestScopeInvalid)

	// A value containing "=" goes through unchanged.
	run := start(`{"scope":"label","label_key":"pool","label_value":"a=b"}`, `["a","b"]`)
	if !reflect.DeepEqual(keys(run), labelKeys) || run["scope"] != "label" || run["label_key"] != "pool" || run["label_value"] != "a=b" || run["node_count"] != float64(2) {
		t.Fatalf("label run = %v", run)
	}
	if stored := finish(); stored.Scope != "label" || stored.LabelKey != "pool" || stored.LabelValue != "a=b" || stored.Nodes != `["a","b"]` {
		t.Fatalf("stored label run = %+v", stored)
	}
	latest := n.expect(n.member, http.MethodGet, "/api/v1/networks/1", "", http.StatusOK)["latest_test"].(map[string]any)
	if latest["label_key"] != "pool" || latest["label_value"] != "a=b" || latest["scope_detail"] != nil || !reflect.DeepEqual(latest["node_uuids"], []any{"a", "b"}) {
		t.Fatalf("latest label run = %v", latest)
	}

	run = start(`{"scope":"nodes","node_uuids":["c","a"]}`, `["a","c"]`)
	if !reflect.DeepEqual(keys(run), runKeys) || run["scope"] != "nodes" {
		t.Fatalf("nodes run = %v", run)
	}
	if stored := finish(); stored.LabelKey != "" || stored.LabelValue != "" || stored.Nodes != `["a","c"]` {
		t.Fatalf("stored nodes run = %+v", stored)
	}
	if got := latestNodes(); !reflect.DeepEqual(got, []any{"a", "c"}) {
		t.Fatalf("finished nodes run node_uuids = %v", got)
	}

	run = start(`{"scope":"all"}`, `["a","b","c"]`)
	if !reflect.DeepEqual(keys(run), runKeys) || run["scope"] != "all" || run["node_count"] != float64(3) {
		t.Fatalf("all run = %v", run)
	}
	if stored := finish(); stored.LabelKey != "" || stored.LabelValue != "" || stored.Nodes != `["a","b","c"]` {
		t.Fatalf("stored all run = %+v", stored)
	}
	if got := latestNodes(); !reflect.DeepEqual(got, []any{"a", "b", "c"}) {
		t.Fatalf("finished all run node_uuids = %v", got)
	}

	// A run whose stored node list cannot be read still answers, with none.
	if _, err := n.store.StartNetworkTestRun(&store.NetworkTestRun{NetworkID: 1, Kind: "tcp", Scope: "nodes", Nodes: "not json", NodeCount: 2}); err != nil {
		t.Fatal(err)
	}
	finish()
	if got := latestNodes(); !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("unreadable node list = %#v", got)
	}
}

// node_uuids is always a list: a stored node list that is empty, null or
// unreadable (a historical row) is [] on the wire, never null or an error.
func TestNetworkTestRunNodeUUIDsOfUnreadableRows(t *testing.T) {
	for _, nodes := range []string{"", "null", "[]", "not json", `{"a":1}`, `"a"`} {
		run := networkTestDTO(&store.NetworkTestRun{ID: 1, Status: "done", Scope: "all", Nodes: nodes, Pairs: "[]"}, nil)
		encoded, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"node_uuids":[],"pairs":[],`) {
			t.Fatalf("nodes %q = %s", nodes, encoded)
		}
	}
	run := networkTestDTO(&store.NetworkTestRun{ID: 1, Status: "done", Scope: "nodes", Nodes: `["a","c"]`}, nil)
	if !reflect.DeepEqual(run.NodeUUIDs, []string{"a", "c"}) {
		t.Fatalf("node_uuids = %#v", run.NodeUUIDs)
	}
}

func TestNetworkTestPairAverageRailBandwidth(t *testing.T) {
	for _, tc := range []struct {
		name, kind, rails, status string
		passed                    int
		storedTotal, wantMean     float64
	}{
		{"mixed rates, error and zero", "rdma_tool", `[{"gbps":80},{"gbps":100},{"gbps":5,"error":"tool failed"},{"gbps":0}]`, "partial", 2, 185, 90},
		{"all failed", "rdma_tool", `[{"gbps":0},{"code":"no_peer_rail"}]`, "failed", 0, 0, 0},
		{"historic aggregate only", "rdma_tool", `[]`, "passed", 0, 180, 0},
		{"TCP aggregate", "tcp", `[]`, "passed", 0, 33, 0},
		{"card connectivity", "card_ping", `[{"rail":"npu0","target_rail":"npu0","reachable":true}]`, "passed", 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := &store.NetworkTestRun{Kind: tc.kind, Bandwidth: tc.kind != "card_ping", Pairs: fmt.Sprintf(`[{"reachable":true,"gbps":%g,"rails":%s}]`, tc.storedTotal, tc.rails)}
			run := networkTestDTO(stored, nil)
			pair := run.Pairs[0]
			if pair.Status != tc.status || run.RailPassedCount != tc.passed || run.RailCount != len(pair.Rails) {
				t.Fatalf("status %q, passing rails %d/%d: %+v", pair.Status, run.RailPassedCount, run.RailCount, pair.Rails)
			}
			wantPartial := 0
			if tc.status == "partial" {
				wantPartial = 1
			}
			if run.PartialCount != wantPartial || pair.Passed != (tc.status == "passed") {
				t.Fatalf("partial_count %d, passed %t, status %q", run.PartialCount, pair.Passed, tc.status)
			}
			if pair.AvgRailGbps != tc.wantMean || pair.Gbps != tc.storedTotal {
				t.Fatalf("mean %v and stored aggregate %v", pair.AvgRailGbps, pair.Gbps)
			}
			raw, err := json.Marshal(pair)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"avg_rail_gbps":`) != (tc.wantMean > 0) {
				t.Fatalf("unexpected average field in %s", raw)
			}
		})
	}
}
