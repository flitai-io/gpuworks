package accelerator

import "testing"

func TestRunnerImageChipFamily(t *testing.T) {
	tests := []struct{ ref, want string }{
		{"gpuworks/gpuworks-runner:cann9.2-910b-vllm0.30.0rc2-r1", "910b"},
		{"gpuworks/gpuworks-runner:cann9.2-a3-vllm0.30.0rc2-r1", "910c"},
		{"gpuworks/gpuworks-runner:cann8.5-a3-sglang0.5.12.post1-r2", "910c"},
		{"quay.io/gpuworks/gpuworks-runner:cann9.1-310p-vllm0.26.0rc1-r1", "310p"},
		{"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cann9.1-950-vllm0.27.1rc1-r1", "950"},
		{"registry.example:5000/gpuworks/gpuworks-runner:cann9.0-a2-vllm0.23.0-r1", "910b"},
		{"GPUWorks/GPUWorks-Runner:CANN9.0-910B-vllm-omni0.23.0rc1-r1", "910b"},
		{"gpuworks/gpuworks-runner:cann9.0-a3-vllm-omni0.23.0rc1-r1@sha256:abc", "910c"},
		// Generic images, other vendors and foreign repositories say nothing.
		{"gpuworks/gpuworks-runner:cann8.5-vllm0.18.0-r1", ""},
		{"gpuworks/gpuworks-runner:cuda12.8-v100-vllm1cat1.5.1-r1", ""},
		{"gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2", ""},
		{"gpuworks/gpuworks-runner:metal15.0-vllm0.31.0-r1", ""},
		{"quay.io/ascend/vllm-ascend:v0.30.0rc2-a3", ""},
		{"catalog/ascend-a3:8", ""},
		{"gpuworks/gpuworks-runner", ""},
		{"gpuworks/gpuworks-runner:latest", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := RunnerImageChipFamily(tt.ref); got != tt.want {
			t.Errorf("RunnerImageChipFamily(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

// Every family a runner tag names (but 950, whose chip identity the probe has
// not reported yet) is one a node's cards can report, so the exclusion rule
// can actually match.
func TestRunnerImageChipFamilyMatchesNodeFamilies(t *testing.T) {
	for ref, id := range map[string]string{
		"gpuworks/gpuworks-runner:cann9.2-910b-vllm0.30.0rc2-r1": "910B4",
		"gpuworks/gpuworks-runner:cann9.2-a3-vllm0.30.0rc2-r1":   "910C",
		"gpuworks/gpuworks-runner:cann9.1-310p-vllm0.26.0rc1-r1": "310P3",
	} {
		if got, want := RunnerImageChipFamily(ref), GPUArch(VendorAscend, id, "").Family; got == "" || got != want {
			t.Errorf("%s family = %q, node %s family = %q", ref, got, id, want)
		}
	}
}
