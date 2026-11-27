package enginelog

import (
	"strings"
	"testing"
)

// Three captured starts from gpuworks-runner CUDA images, each failed for a
// different reason: the event must name that reason, not the server's
// reaction to the child's signal.
func TestACapturedCudaStartFailsForItsOwnReason(t *testing.T) {
	for name, want := range map[string]string{
		"sglang_cuda_docker_model_attribute.txt": "AttributeError: 'MiniCPMV4_6' object has no attribute 'shared_experts_config'",
		"sglang_cuda_docker_warmup_oom.txt":      "CUDA error: out of memory",
		"sglang_cuda_docker_capture_assert.txt":  "RuntimeError: num_active_captures_ > 0 INTERNAL ASSERT FAILED",
	} {
		if got := RootCause(fieldLog(t, name)); !strings.Contains(got, want) {
			t.Errorf("%s: RootCause = %q, want it to state %q", name, got, want)
		}
	}
	// The capture runs the card out of memory during the start, so the start
	// is an aborted start by the engine's own statement, with the assert as
	// its cause.
	cause, ok := StartAborted(DialectSGLang, fieldLog(t, "sglang_cuda_docker_capture_assert.txt"))
	if !ok || !strings.Contains(cause, "markCaptureEnd called with no captures in progress") {
		t.Errorf("the capture assert: aborted=%v cause=%q", ok, cause)
	}
}
