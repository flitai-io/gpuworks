package engine

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRunningContainerAdoptionDoesNotApplyNewLaunchAdmission(t *testing.T) {
	driver := &headlessTestDriver{name: "docker", status: runtime.Status{Exists: true, Running: true}}
	var reports []proto.InstanceStatus
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reports = append(reports, st) }, nil, t.TempDir(), &gpu.AMDProber{})
	// Headless AMD is refused for a new launch, but a running container must
	// keep its observed state instead of becoming Failed while it still serves.
	spec := proto.InstanceDesiredState{UUID: "existing-amd", Headless: true}
	if err := e.validateTargetDesired(spec); err == nil {
		t.Fatal("fixture is not refused for new launches")
	}
	e.startOrAdoptEpoch(context.Background(), spec, 0)
	if driver.calls == 0 {
		t.Fatal("running container was not inspected")
	}
	if len(reports) == 0 {
		t.Fatal("adoption did not report observed state")
	}
	for _, report := range reports {
		if report.State == proto.InstFailed {
			t.Fatalf("adoption was rejected by launch admission: %+v", reports)
		}
	}
}

func TestMissingContainerIsAdmittedBeforePreparation(t *testing.T) {
	driver := &preflightTrackingDriver{headlessTestDriver: &headlessTestDriver{name: "docker"}}
	var reports []proto.InstanceStatus
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reports = append(reports, st) }, nil, t.TempDir(), &gpu.AMDProber{})
	spec := proto.InstanceDesiredState{UUID: "new-amd", Headless: true}
	e.startOrAdoptEpoch(context.Background(), spec, 0)
	if driver.statusCalls != 1 || driver.runCalls != 0 {
		t.Fatalf("status=%d run=%d", driver.statusCalls, driver.runCalls)
	}
	if len(reports) != 1 || reports[0].State != proto.InstFailed {
		t.Fatalf("reports=%+v", reports)
	}
}
