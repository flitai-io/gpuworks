package seed

import (
	"strings"
	"testing"
)

func TestLagunaSLeavesHeadroomForA100KernelWarmup(t *testing.T) {
	_, spec := catalogSpec(t, "poolside/laguna-s-2.1", "int4-sglang-nvidia-general")
	args := strings.Join(spec.Args, " ")
	for _, want := range []string{
		"--context-length 262144",
		"--mem-fraction-static 0.75",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("launch arguments do not contain %q: %v", want, spec.Args)
		}
	}
	// CUDA graph capture works with this headroom on SGLang 0.5.21 (issue #35).
	if strings.Contains(args, "--disable-cuda-graph") {
		t.Errorf("launch arguments force eager decode: %v", spec.Args)
	}
}
