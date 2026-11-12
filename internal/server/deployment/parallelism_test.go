package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func TestRequestPlacementDPUsesQuickAndFinalFlags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		req      dto.DeployRequest
		template []string
		want     int
	}{
		{"quick", dto.DeployRequest{Engine: "vllm", QuickParams: map[string]any{"data_parallel_size": 2}}, nil, 2},
		{"explicit", dto.DeployRequest{Engine: "vllm", ExtraArgs: "--data-parallel-size 4"}, nil, 4},
		{"quick overrides template", dto.DeployRequest{Engine: "vllm", QuickParams: map[string]any{"data_parallel_size": 1}}, []string{"--data-parallel-size=4"}, 1},
		{"unset template", dto.DeployRequest{Engine: "vllm", QuickParamsUnset: []string{"data_parallel_size"}}, []string{"--data-parallel-size=4"}, 1},
		{"attention shares TP", dto.DeployRequest{Engine: "sglang", QuickParams: map[string]any{"data_parallel_size": 2}, ExtraArgs: "--enable-dp-attention"}, nil, 1},
		{"template", dto.DeployRequest{Engine: "vllm"}, []string{"--data-parallel-size=2"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requestPlacementDP(&tc.req, tc.template)
			if err != nil || got != tc.want {
				t.Fatalf("got %d,%v want %d", got, err, tc.want)
			}
		})
	}
}

func TestRequestPlacementDPRejectsConflictingExplicitValues(t *testing.T) {
	req := dto.DeployRequest{Engine: "vllm", QuickParams: map[string]any{"data_parallel_size": 2}, ExtraArgs: "--data-parallel-size 4"}
	if _, err := requestPlacementDP(&req, nil); err == nil {
		t.Fatal("conflicting quick and explicit DP accepted during node selection")
	}
}
