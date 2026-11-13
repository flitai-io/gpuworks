package engine

import (
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestInjectionFailureReportsFactsWithoutInventingRuntimeState(t *testing.T) {
	var got proto.InstanceStatus
	e := NewWithProber(&headlessTestDriver{name: "docker"}, func(s proto.InstanceStatus) { got = s }, nil, t.TempDir(), &gpu.KunlunProber{})
	err := e.applyDeviceInjectionForLaunch(t.Context(), &runtime.WorkloadSpec{}, nil, "docker", 1)
	var refusal *gpu.InjectionError
	if !errors.As(err, &refusal) || refusal.Failure.Code != proto.ErrorAcceleratorDeviceSelectionRequired {
		t.Fatalf("err=%v", err)
	}
	e.reportInjectionFailure(proto.InstanceDesiredState{UUID: "refusal", APIPort: 8000}, fmt.Errorf("launch: %w", err))
	if got.State != proto.InstFailed || got.DirectAddr != "" || got.ContainerID != "" || got.RuntimeFailure == nil || got.RuntimeFailure.Code != proto.ErrorAcceleratorDeviceSelectionRequired || got.Message != "launch: "+err.Error() {
		t.Fatalf("status=%+v", got)
	}
	e.reportInjectionFailure(proto.InstanceDesiredState{UUID: "legacy"}, errors.New("legacy backend diagnostic"))
	if got.RuntimeFailure != nil || got.Message != "legacy backend diagnostic" {
		t.Fatalf("legacy=%+v", got)
	}
}

func TestDesiredAndFreshInjectionUseTheSameRuntimeDiagnostic(t *testing.T) {
	for _, actual := range []accelerator.HostRuntime{{OS: "linux", Arch: "amd64", Driver: ""}, {OS: "linux", Arch: "arm64", Driver: "docker"}} {
		e := NewWithProber(&headlessTestDriver{name: actual.Driver}, nil, nil, t.TempDir(), &gpu.KunlunProber{}, WithHostRuntimeFacts(func() accelerator.HostRuntime { return actual }))
		got := e.validateTargetDesiredForDriver(proto.InstanceDesiredState{GPUIndexes: []int{0}}, actual.Driver)
		want := gpu.RuntimeRequirementError("kunlunxin", actual)
		var refusal *gpu.InjectionError
		if !errors.As(got, &refusal) || refusal.Failure != want.Failure {
			t.Fatalf("got=%v want=%v", got, want)
		}
	}
}
