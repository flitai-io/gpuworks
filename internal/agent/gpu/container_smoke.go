package gpu

import (
	"fmt"
	"strings"
)

const (
	cudaGradedOKMarker          = "CUDA_RUNTIME_SMOKE_OK"
	cudaGradedCardMarker        = "CUDA_RUNTIME_SMOKE_CARD_FAILED:"
	cudaGradedUnavailableMarker = "CUDA_RUNTIME_SMOKE_UNAVAILABLE:"
)

// ContainerSmokeVerdict grades a runtime check run inside a container with
// the vendor's own grader, and reports a vendor without one as the host's.
func ContainerSmokeVerdict(vendor string, out string, exitCode int) error {
	var exitErr error
	if exitCode != 0 {
		exitErr = fmt.Errorf("container exited with code %d", exitCode)
	}
	if grader, ok := driverOf(vendor).(ContainerRuntimeGrader); ok {
		return grader.ContainerSmokeVerdict(out, exitErr)
	}
	return &HostRuntimeUnavailableError{Reason: "no container runtime check for vendor " + vendor}
}

func cudaGradedVerdict(out string, exitErr error) error {
	if detail, ok := ascendSmokeMarkerDetail(out, cudaGradedUnavailableMarker); ok {
		return &HostRuntimeUnavailableError{Reason: "the runtime in the image could not be used: " + detail}
	}
	if detail, ok := ascendSmokeMarkerDetail(out, cudaGradedCardMarker); ok {
		return fmt.Errorf("runtime smoke failed: %s", detail)
	}
	if exitErr == nil && strings.Contains(out, cudaGradedOKMarker) {
		return nil
	}
	detail := strings.TrimSpace(out)
	if detail == "" && exitErr != nil {
		detail = exitErr.Error()
	}
	if detail == "" {
		detail = "the check printed nothing"
	}
	return &HostRuntimeUnavailableError{Reason: "the runtime check in the container did not reach the card: " + detail}
}

// ContainerSmokeVerdict grades the graded CUDA check run inside an HGGC image.
func (p *PPUProber) ContainerSmokeVerdict(out string, exitErr error) error {
	return cudaGradedVerdict(out, exitErr)
}
