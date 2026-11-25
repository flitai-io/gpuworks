package gpu

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// A backend is not asked about a runtime whose view of the devices is not
// stated, and the refusal says which case it is: no runtime at all, or one
// that does not say.
func TestResolveInjectionSaysWhyItCannotDescribeTheRuntime(t *testing.T) {
	granting := vendorOnlyProber{vendor: accelerator.VendorNVIDIA, inj: RuntimeInjection{Grant: runtime.DeviceGrant{Devices: []runtime.Device{{Path: "/dev/card"}}}}}
	for _, tc := range []struct {
		target runtime.Target
		want   string
	}{
		{runtime.Target{}, "this agent has no container runtime"},
		{runtime.TargetNamed("podman"), `runtime "podman"`},
	} {
		inj := ResolveInjection(context.Background(), granting, InjectionRequest{Indexes: []int{0}, Runtime: tc.target})
		if !strings.Contains(inj.UnsupportedReason, tc.want) || !inj.Grant.Empty() {
			t.Errorf("target %+v: injection = %+v, want a refusal naming %q", tc.target, inj, tc.want)
		}
	}
}
