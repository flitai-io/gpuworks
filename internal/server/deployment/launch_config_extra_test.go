package deployment

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Template deviation is judged on values, not on how they were written. These
// are the three answers a deploy form needs: the same value stated twice is not
// a change, a different value is, and an argument the dictionary does not
// describe is a change of a different kind.
func TestTemplateDeviationComparesMaterializedValues(t *testing.T) {
	selection := templateSelection{
		Spec:      &store.InferenceSpecRow{Name: "balanced", Engine: "vllm", Image: "image"},
		ExtraArgs: []string{"--gpu-memory-utilization", "0.95"},
	}
	for name, test := range map[string]struct {
		req  dto.DeployRequest
		want []string
	}{
		"form repeats the template value": {
			req:  dto.DeployRequest{QuickParams: map[string]any{"gpu_memory_utilization": 0.95}},
			want: nil,
		},
		"form states a different value": {
			req:  dto.DeployRequest{QuickParams: map[string]any{"gpu_memory_utilization": 0.9}},
			want: []string{"params"},
		},
		"only an argument outside the dictionary": {
			req:  dto.DeployRequest{ExtraArgs: "--load-format fastsafetensors"},
			want: []string{"extra_args"},
		},
		"nothing at all": {
			req:  dto.DeployRequest{},
			want: nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := test.req
			got := flatTemplateRequestDiff("", "", "", "", &request, selection)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("diff=%#v, want %#v", got, test.want)
			}
		})
	}
}
