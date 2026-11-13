// Package probe runs behavioral partition checks through the same injection
// builder used for real inference instances.
package probe

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	probeQuotaBytes = int64(4096 << 20)
	maxProbeDetail  = 4096
)

type WorkloadBuilder interface {
	BuildPartitionProbeWorkload(context.Context, string, string, int, int64, string) (runtime.WorkloadSpec, error)
	CleanupPartitionProbe(string) error
}

type Runner struct {
	driver  runtime.Driver
	builder WorkloadBuilder
	vendor  string

	mu      sync.Mutex
	ran     bool
	results []proto.PartitionProbeResult
	now     func() time.Time
	timeout time.Duration

	prepareTimeout time.Duration
}

func New(driver runtime.Driver, builder WorkloadBuilder, vendor string) *Runner {
	return &Runner{
		driver: driver, builder: builder, vendor: accelerator.NormalizeVendor(vendor), now: time.Now,
		timeout: 15 * time.Second, prepareTimeout: 2 * time.Minute,
	}
}

// kubeProbeTimeout is the behavioral budget on Kubernetes, where the probe is
// a pod: creating its sandbox and starting it alone takes 10 to 20 seconds,
// which the docker budget does not allow for.
const kubeProbeTimeout = 60 * time.Second

// probeTimeout is the behavioral budget for the current driver.
func (r *Runner) probeTimeout() time.Duration {
	if r.driver != nil && r.driver.Name() == "kube" && r.timeout < kubeProbeTimeout {
		return kubeProbeTimeout
	}
	return r.timeout
}

// SetDriver invalidates results after a runtime switch. The builder remains
// the same engine, whose driver has already been switched by the caller.
func (r *Runner) SetDriver(driver runtime.Driver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.driver = driver
	r.ran = false
	r.results = nil
}

// Results runs once per agent process and then returns a copy of the cached
// snapshot. Refresh is used by install hooks or the explicit re-probe RPC.
func (r *Runner) Results(ctx context.Context, assetsFound []proto.AssetReport, gpus []proto.GPUInfo) []proto.PartitionProbeResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ran {
		r.results = r.runLocked(ctx, assetsFound, gpus)
		r.ran = true
	}
	return append([]proto.PartitionProbeResult(nil), r.results...)
}

// Cached returns the last snapshot without running anything; empty until a
// run has happened. It serves status reports taken while the GPU inventory
// could not be read, which must neither rerun probes nor drop results.
func (r *Runner) Cached() []proto.PartitionProbeResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]proto.PartitionProbeResult{}, r.results...)
}

func (r *Runner) Refresh(ctx context.Context, assetsFound []proto.AssetReport, gpus []proto.GPUInfo) []proto.PartitionProbeResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = r.runLocked(ctx, assetsFound, gpus)
	r.ran = true
	return append([]proto.PartitionProbeResult(nil), r.results...)
}

func (r *Runner) runLocked(ctx context.Context, assetsFound []proto.AssetReport, gpus []proto.GPUInfo) []proto.PartitionProbeResult {
	var results []proto.PartitionProbeResult
	for _, backend := range accelerator.All() {
		if accelerator.SkipsBehaviorProbe(backend) || !completeAssets(backend, assetsFound) {
			continue
		}
		for _, gpu := range gpus {
			if !probeBackendMatchesGPU(backend, r.vendor, gpu) {
				continue
			}
			gpuIndex := gpu.Index
			result := proto.PartitionProbeResult{Backend: backend.ID(), GPUIndex: &gpuIndex, Status: proto.PartitionProbeFailed, CheckedAt: r.now().UTC()}
			if r.driver == nil || r.builder == nil {
				result.Status = proto.PartitionProbeSkipped
				result.Detail = "no container runtime available on the node"
			} else if _, ok := r.driver.(runtime.ImageLoader); !ok {
				result.Detail = "the container runtime cannot load the embedded probe image"
			} else {
				result.Status, result.Detail = r.runOne(ctx, backend.ID(), gpuIndex)
			}
			results = append(results, result)
		}
	}
	if results == nil {
		return []proto.PartitionProbeResult{}
	}
	return results
}

// probeBackendMatchesGPU also leaves out the cards a container cannot be
// given: one the vendor CLI did not number has no device index to inject,
// and one lent to a virtual machine or held by another driver is not this
// node's to probe.
func probeBackendMatchesGPU(backend accelerator.Backend, vendor string, gpu proto.GPUInfo) bool {
	if gpu.Index < 0 || !accelerator.CardUsable(gpu.Ownership) {
		return false
	}
	return accelerator.NormalizeVendor(gpu.Vendor) == vendor && backend.Matches(gpu.Vendor, gpu.ArchID)
}

func (r *Runner) runOne(parent context.Context, backend string, gpuIndex int) (status, detail string) {
	prepareTimeout := r.prepareTimeout
	if prepareTimeout <= 0 {
		prepareTimeout = 2 * time.Minute
	}
	prepareCtx, prepareCancel := context.WithTimeout(parent, prepareTimeout)
	present, err := r.driver.ImageExists(prepareCtx, assets.PartitionProbeImage)
	if err != nil {
		prepareCancel()
		return proto.PartitionProbeFailed, tailDetail("inspect embedded probe image: " + err.Error())
	}
	if !present {
		archive, openErr := assets.OpenPartitionProbeImage()
		if openErr != nil {
			prepareCancel()
			return proto.PartitionProbeFailed, tailDetail(openErr.Error())
		}
		loadErr := r.driver.(runtime.ImageLoader).LoadImage(prepareCtx, assets.PartitionProbeImage, archive)
		_ = archive.Close()
		if loadErr != nil {
			prepareCancel()
			return proto.PartitionProbeFailed, tailDetail("load partition probe image: " + loadErr.Error())
		}
	}
	prepareCancel()

	// Keep the behavioral assertion on its own short budget. Cold udocker
	// provisioning and offline image import can take longer, but must not make a
	// hung quota container appear acceptable.
	ctx, cancel := context.WithTimeout(parent, r.probeTimeout())
	defer cancel()
	probeID := fmt.Sprintf("probe-%s-%d", backend, gpuIndex)
	// The probe name and state key are stable per physical GPU. A prior failed
	// cleanup can therefore be retried before writing a new quota config.
	staleName := fmt.Sprintf("gpuworks-partition-probe-%s-%d", backend, gpuIndex)
	if err := r.driver.Remove(ctx, staleName); err != nil {
		return proto.PartitionProbeFailed, tailDetail("remove stale probe container: " + err.Error())
	}
	if err := r.builder.CleanupPartitionProbe(probeID); err != nil {
		return proto.PartitionProbeFailed, tailDetail("clean stale probe partition: " + err.Error())
	}
	ws, err := r.builder.BuildPartitionProbeWorkload(ctx, backend, assets.PartitionProbeImage, gpuIndex, probeQuotaBytes, probeID)
	if err != nil {
		if cleanupErr := r.builder.CleanupPartitionProbe(probeID); cleanupErr != nil {
			return proto.PartitionProbeFailed, tailDetail(err.Error() + "; cleanup: " + cleanupErr.Error())
		}
		return proto.PartitionProbeFailed, tailDetail(err.Error())
	}
	containerID := ""
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		name := ws.Name
		if containerID != "" {
			name = containerID
			_ = r.driver.Stop(cleanupCtx, name)
		}
		if removeErr := r.driver.Remove(cleanupCtx, name); removeErr != nil {
			status = proto.PartitionProbeFailed
			detail = tailDetail(detail + "; remove probe container: " + removeErr.Error())
			return // Keep the partition state while the container may still use it.
		}
		if cleanupErr := r.builder.CleanupPartitionProbe(probeID); cleanupErr != nil {
			status = proto.PartitionProbeFailed
			detail = tailDetail(detail + "; cleanup probe partition: " + cleanupErr.Error())
		}
	}()
	// A stale deterministic name can survive a killed agent; remove it before
	// retrying, then always clean up after collecting logs.
	if err := r.driver.Remove(ctx, ws.Name); err != nil {
		return proto.PartitionProbeFailed, tailDetail("remove stale probe container: " + err.Error())
	}
	id, err := r.driver.Run(ctx, ws)
	if err != nil {
		return proto.PartitionProbeFailed, tailDetail(err.Error())
	}
	containerID = id
	var last runtime.Status
	for {
		last, err = r.driver.Status(ctx, id)
		if err != nil {
			return proto.PartitionProbeFailed, tailDetail("inspect partition probe: " + err.Error())
		}
		if last.Exists && !last.Running {
			break
		}
		select {
		case <-ctx.Done():
			return proto.PartitionProbeFailed, "dGPU probe timed out"
		case <-time.After(100 * time.Millisecond):
		}
	}
	detail = readLogs(ctx, r.driver, id)
	if len(ws.PreparationNotes) > 0 {
		detail = strings.TrimSpace(strings.Join(ws.PreparationNotes, "\n") + "\n" + detail)
	}
	if last.ExitCode != 0 {
		if detail == "" {
			detail = fmt.Sprintf("probe container exited with code %d", last.ExitCode)
		}
		return proto.PartitionProbeFailed, tailDetail(detail)
	}
	return proto.PartitionProbePassed, tailDetail(detail)
}

func completeAssets(backend accelerator.Backend, reports []proto.AssetReport) bool {
	for _, report := range reports {
		if report.Backend != backend.ID() {
			continue
		}
		available := map[string]bool{}
		for _, file := range report.Files {
			available[strings.TrimSpace(file)] = true
		}
		complete := true
		for _, required := range backend.RequiredAssets() {
			complete = complete && available[required]
		}
		if complete {
			return true
		}
	}
	return false
}

func readLogs(ctx context.Context, driver runtime.Driver, id string) string {
	rc, err := driver.Logs(ctx, id, false, 100)
	if err != nil {
		return ""
	}
	defer rc.Close()
	data, _ := io.ReadAll(io.LimitReader(rc, 64*1024))
	return strings.TrimSpace(string(data))
}

func tailDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if len(detail) <= maxProbeDetail {
		return detail
	}
	return detail[len(detail)-maxProbeDetail:]
}
