package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// Exercise the literal public wire response, not DTO fields: a wrong-vendor
// first failure must not hide whether the node met the user's other labels.
func TestEvaluateRequirementDiagnosticsWire(t *testing.T) {
	h, id := eligibleNodeFixture(t)
	for _, tc := range []struct {
		name, requirements string
		present            bool
		matches            map[string]bool
	}{
		{"labels", `{"pool":["inference"]}`, true, map[string]bool{"eligible-node": true, "chip-old-node": true, "offline-node": true, "vendor-node": true, "unlabeled-node": false}},
		{"no explicit constraints", `{}`, false, nil},
		{"none match", `{"pool":["missing"]}`, true, map[string]bool{}},
		{"selected node intersects labels", `{"pool":["inference"],"gpuworks.ai/node":["chip-old-node"]}`, true, map[string]bool{"chip-old-node": true}},
		{"invalid", `{"gpuworks.ai/gpu_model":["A100"]}`, false, nil},
		{"unresolved network", `{"gpuworks.ai/compute-network":["999999"]}`, true, map[string]bool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			body := fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","placement_mode":"auto","target_vendor":"nvidia","node_requirements":%s}`, id, tc.requirements)
			h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
			}
			var response map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			var entries []map[string]json.RawMessage
			if err := json.Unmarshal(response["eligible_nodes"], &entries); err != nil {
				t.Fatal(err)
			}
			if len(entries) != 5 {
				t.Fatalf("entries=%d", len(entries))
			}
			for _, entry := range entries {
				var uuid string
				_ = json.Unmarshal(entry["uuid"], &uuid)
				encoded, _ := json.Marshal(entry)
				var wire dto.EligibleNode
				if err := json.Unmarshal(encoded, &wire); err != nil {
					t.Fatal(err)
				}
				roundTrip, _ := json.Marshal(wire)
				var roundMap map[string]json.RawMessage
				_ = json.Unmarshal(roundTrip, &roundMap)
				if len(roundMap) != len(entry) {
					t.Fatalf("wire fields lost in round trip: %s", roundTrip)
				}
				value, exists := entry["requirements_match"]
				if exists != tc.present {
					t.Fatalf("%s requirements_match present=%v; response %s", uuid, exists, rec.Body.String())
				}
				if exists && string(value) != fmt.Sprint(tc.matches[uuid]) {
					t.Errorf("%s match=%s", uuid, value)
				}
				if tc.name == "labels" && uuid == "chip-old-node" {
					var reasons []map[string]any
					_ = json.Unmarshal(entry["reasons"], &reasons)
					found := false
					for _, reason := range reasons {
						if reason["code"] != "chip_mismatch" {
							continue
						}
						found = true
						params := reason["params"].(map[string]any)
						if params["required_vendor"] != "nvidia" || params["required_cc"] == nil {
							t.Fatalf("required facts=%v", params)
						}
						gpus, ok := params["actual_gpus"].([]any)
						if !ok || len(gpus) != 1 {
							t.Fatalf("actual GPU facts=%v", params)
						}
						gpu := gpus[0].(map[string]any)
						if gpu["name"] != "A100" || gpu["cc"] != "8.0" || gpu["vendor"] != "nvidia" {
							t.Fatalf("reported facts=%v", gpu)
						}
					}
					if !found {
						t.Fatal("missing chip refusal")
					}
				}
			}
		})
	}
}
