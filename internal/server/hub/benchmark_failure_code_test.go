package hub

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
)

// Reports exactly as agents send them: a current agent carries the code, an
// agent older than codes spelled it into the message.
func TestBenchmarkFailureCodeReadsTheCodeAndOlderAgentsMessage(t *testing.T) {
	for _, tc := range []struct {
		name, wire, code, dataset string
	}{
		{"coded", `{"id":1,"state":"failed","message":"load the built-in multiturn dataset: EOF","code":"BENCHMARK_DATASET_LOAD_FAILED","params":{"dataset":"multiturn"}}`, dataset.LoadErrorCode, "multiturn"},
		{"lower-case code", `{"id":1,"state":"failed","message":"x","code":"benchmark_dataset_load_failed","params":{"dataset":"multiturn"}}`, dataset.LoadErrorCode, "multiturn"},
		{"older agent", `{"id":1,"state":"failed","message":"BENCHMARK_DATASET_LOAD_FAILED: multiturn: open data/multiturn.jsonl: file does not exist"}`, dataset.LoadErrorCode, "multiturn"},
		{"older agent, no samples", `{"id":1,"state":"failed","message":"BENCHMARK_DATASET_LOAD_FAILED: multiturn: dataset contains no samples"}`, dataset.LoadErrorCode, "multiturn"},
		{"uncoded failure", `{"id":1,"state":"failed","message":"all 3 warmup requests failed"}`, "", ""},
		{"running", `{"id":1,"state":"running","message":"BENCHMARK_DATASET_LOAD_FAILED: multiturn: x"}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st proto.BenchmarkStatus
			if err := json.Unmarshal([]byte(tc.wire), &st); err != nil {
				t.Fatal(err)
			}
			code, params := benchmarkFailureCode(st)
			if code != tc.code {
				t.Fatalf("code = %q, want %q", code, tc.code)
			}
			if got, _ := params["dataset"].(string); got != tc.dataset {
				t.Fatalf("params = %v", params)
			}
		})
	}
}
