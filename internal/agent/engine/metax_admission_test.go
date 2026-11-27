package engine

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type metaxAdmissionDriver struct {
	*headlessTestDriver
	imageCalls, pullCalls, prepareCalls, runCalls int
}

func (d *metaxAdmissionDriver) ImageExists(context.Context, string) (bool, error) {
	d.imageCalls++
	return false, nil
}
func (d *metaxAdmissionDriver) PullImage(context.Context, string, func(string)) error {
	d.pullCalls++
	return nil
}
func (d *metaxAdmissionDriver) Prepare(context.Context, runtime.WorkloadSpec) error {
	d.prepareCalls++
	return nil
}
func (d *metaxAdmissionDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	d.runCalls++
	return "unexpected", nil
}

func TestMetaXNewAndRepeatedLaunchAreRefusedBeforeImageWork(t *testing.T) {
	driver := &metaxAdmissionDriver{headlessTestDriver: &headlessTestDriver{name: "docker"}}
	var reports []proto.InstanceStatus
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reports = append(reports, st) }, nil, t.TempDir(), &gpu.MetaXProber{})
	// A legacy desired state has no requested-feature snapshot.
	spec := proto.InstanceDesiredState{UUID: "metax-new", Image: "example.invalid/never-pull"}
	for range 2 {
		e.startOrAdoptEpoch(context.Background(), spec, 0)
	}
	if len(reports) != 2 || reports[0].State != proto.InstFailed || reports[1].State != proto.InstFailed {
		t.Fatalf("reports=%+v", reports)
	}
	if driver.imageCalls != 0 || driver.pullCalls != 0 || driver.prepareCalls != 0 || driver.runCalls != 0 {
		t.Fatalf("image=%d pull=%d prepare=%d run=%d", driver.imageCalls, driver.pullCalls, driver.prepareCalls, driver.runCalls)
	}
}

func TestMetaXRunningContainerIsAdopted(t *testing.T) {
	driver := &headlessTestDriver{name: "docker", status: runtime.Status{Exists: true, Running: true}}
	var reports []proto.InstanceStatus
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reports = append(reports, st) }, nil, t.TempDir(), &gpu.MetaXProber{})
	e.headlessStableChecks = 1
	spec := proto.InstanceDesiredState{UUID: "metax-existing", Headless: true}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e.startOrAdoptEpoch(ctx, spec, 0)
	if driver.calls == 0 || len(reports) == 0 {
		t.Fatalf("container inspections=%d reports=%+v", driver.calls, reports)
	}
	for _, report := range reports {
		if report.State == proto.InstFailed {
			t.Fatalf("running container rejected during adoption: %+v", reports)
		}
	}
}
