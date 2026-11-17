package store

import (
	"testing"

	"gpuworks.ai/gpuworks/pkg/bench/dataset"
)

func TestBenchmarkErrorCodesMatchTheirProducers(t *testing.T) {
	if BenchmarkErrorDatasetLoadFailed != dataset.LoadErrorCode {
		t.Fatalf("store declares %q, the dataset produces %q", BenchmarkErrorDatasetLoadFailed, dataset.LoadErrorCode)
	}
}
