package gpu

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// InjectionError preserves structured rejection facts across error wrapping.
// Reason remains useful to callers that only understand error/string.
type InjectionError struct {
	Failure proto.RuntimeFailure
	Reason  string
}

func (e *InjectionError) Error() string { return e.Reason }

func rejectedInjection(failure proto.RuntimeFailure, reason string) RuntimeInjection {
	return RuntimeInjection{UnsupportedReason: reason, Failure: &failure}
}

// RuntimeRequirementError uses the shared host requirement table for both
// desired-state admission and fresh injection. Nil means the host fits.
func RuntimeRequirementError(vendor string, actual accelerator.HostRuntime) *InjectionError {
	mismatch := accelerator.RuntimeMismatchFor(vendor, actual)
	if mismatch == nil {
		return nil
	}
	requiredRuntime := strings.Join(mismatch.RequiredDrivers, "/")
	code, cause := proto.ErrorAcceleratorRuntimeUnsupported, "runtime_unsupported"
	if mismatch.MissingFacts {
		code, cause = proto.ErrorAcceleratorRuntimeUnreported, "runtime_unreported"
	}
	reason := fmt.Sprintf("%s: requires %s/%s with %s; observed os=%q arch=%q runtime=%q", cause, mismatch.RequiredOS, mismatch.RequiredArch, requiredRuntime, actual.OS, actual.Arch, actual.Driver)
	return &InjectionError{
		Failure: proto.RuntimeFailure{
			Code: code, Vendor: vendor,
			OS: actual.OS, Arch: actual.Arch, Runtime: actual.Driver,
			RequiredOS: mismatch.RequiredOS, RequiredArch: mismatch.RequiredArch,
			RequiredRuntime: requiredRuntime,
		},
		Reason: reason,
	}
}

func DeviceSelectionError(vendor string) *InjectionError {
	return &InjectionError{Failure: proto.RuntimeFailure{Code: proto.ErrorAcceleratorDeviceSelectionRequired, Vendor: vendor}, Reason: fmt.Sprintf("%s requires an explicit nonempty GPU selection", accelerator.VendorBrand(vendor))}
}
