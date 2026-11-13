//go:build docker

package l3

import (
	"context"
	"io"
	"strings"
	"sync"

	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
)

// probeStandInDriver is the real docker driver except for one thing: the
// partition probe container. The probe runs the quota-enforcing probe image
// against the node's partition libraries; the L3 cases install placeholder
// libraries (they exercise injection into real containers, not a real vcann-rt or
// HGGC), so a real probe can only fail, and the agent would report that failure
// at every status report - replacing any passing result a test wrote into the
// store by hand, whenever the report happens to land (a state sync, a catalog
// change, the minute ticker). The stand-in makes the agent itself report a
// pass for the workload it built, which the test records and asserts on, while
// every other container and image goes to real docker.
type probeStandInDriver struct {
	*dockerdrv.Driver
	mu       sync.Mutex
	launched []runtime.WorkloadSpec
	ids      map[string]bool
}

func newProbeStandInDriver() *probeStandInDriver {
	return &probeStandInDriver{Driver: dockerdrv.New(), ids: map[string]bool{}}
}

func isProbeName(name string) bool { return strings.HasPrefix(name, "gpuworks-partition-probe-") }

func (d *probeStandInDriver) standIn(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ids[id] || isProbeName(id)
}

func (d *probeStandInDriver) ImageExists(ctx context.Context, image string) (bool, error) {
	if image == assets.PartitionProbeImage {
		return true, nil
	}
	return d.Driver.ImageExists(ctx, image)
}

func (d *probeStandInDriver) Run(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	if spec.Image != assets.PartitionProbeImage {
		return d.Driver.Run(ctx, spec)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.launched = append(d.launched, spec)
	id := "l3-probe-" + spec.Name
	d.ids[id] = true
	return id, nil
}

func (d *probeStandInDriver) Status(ctx context.Context, id string) (runtime.Status, error) {
	if d.standIn(id) {
		return runtime.Status{Exists: true}, nil // exited with code 0
	}
	return d.Driver.Status(ctx, id)
}

func (d *probeStandInDriver) Logs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	if d.standIn(id) {
		return io.NopCloser(strings.NewReader("partition probe stand-in: exited 0\n")), nil
	}
	return d.Driver.Logs(ctx, id, follow, tail)
}

func (d *probeStandInDriver) Stop(ctx context.Context, id string) error {
	if d.standIn(id) {
		return nil
	}
	return d.Driver.Stop(ctx, id)
}

func (d *probeStandInDriver) Remove(ctx context.Context, id string) error {
	if d.standIn(id) {
		return nil
	}
	return d.Driver.Remove(ctx, id)
}

// probeLaunches is a copy of the probe workloads the agent has started.
func (d *probeStandInDriver) probeLaunches() []runtime.WorkloadSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]runtime.WorkloadSpec(nil), d.launched...)
}
