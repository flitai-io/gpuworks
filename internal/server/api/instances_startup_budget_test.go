package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Old desired states already record both the platform budget and an explicit
// engine environment. Expose that evidence without inventing its source or
// rewriting the old attempt to today's automatic policy.
func TestLegacyInstanceStartupBudgetIsExplainedOverHTTP(t *testing.T) {
	h := newLaunchFactsHarness(t, "legacy-startup-budget")
	_, err := h.store.CreateInstance(&store.Instance{UUID: "legacy-startup", DeploymentID: 999, State: proto.InstFailed,
		DesiredStateJSON: `{"uuid":"legacy-startup","engine":"vllm","start_timeout_seconds":1800,"launch":{"command_mode":"engine","entrypoint":["vllm","serve","/model"],"argv":[],"env":{"VLLM_ENGINE_READY_TIMEOUT_S":"36000"}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response := h.request(http.MethodGet, "/api/v1/instances/legacy-startup", true)
		if response.Code != http.StatusOK {
			t.Fatalf("GET=%d body=%s", response.Code, response.Body.String())
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"start_timeout_sec": "1800", "engine_start_timeout_sec": "36000", "start_timeout_conflict": "true", "env": `{"VLLM_ENGINE_READY_TIMEOUT_S":"36000"}`} {
			if string(wire[key]) != want {
				t.Fatalf("legacy %s=%s want=%s (body=%s)", key, wire[key], want, response.Body.String())
			}
		}
		if _, present := wire["start_timeout_source"]; present {
			t.Fatalf("old budget source was invented: %s", response.Body.String())
		}
	}
}
