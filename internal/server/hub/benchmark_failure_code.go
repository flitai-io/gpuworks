package hub

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
)

// benchmarkFailureCode is the read boundary for a failed benchmark report's
// code. A current agent sends it (normalized to upper case here). An agent
// older than codes spelled the dataset-load failure into the message as
// "BENCHMARK_DATASET_LOAD_FAILED: <dataset>: <cause>" (external text: that
// agent's wire output); mixed-version fleets are permanent, so that one shape
// is recognized here and nowhere downstream.
func benchmarkFailureCode(st proto.BenchmarkStatus) (string, map[string]any) {
	if code := proto.NormalizeCode(st.Code); code != "" || st.State != "failed" {
		return code, st.Params
	}
	if rest, ok := strings.CutPrefix(st.Message, dataset.LoadErrorCode+": "); ok {
		name, _, _ := strings.Cut(rest, ":")
		return dataset.LoadErrorCode, map[string]any{"dataset": strings.TrimSpace(name)}
	}
	return "", nil
}
