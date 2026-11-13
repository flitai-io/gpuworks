package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"gpuworks.ai/gpuworks/internal/accelerator"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// amdPartitionInjector implements the AMD logical partition backend:
// libvrocm.so (the rocm-slicing-shim) is mounted into the
// container and activated through LD_PRELOAD plus VROCM_DEVICE_MEMORY_LIMIT_*
// env, with a per-instance usage ledger in /dev/shm (host IPC namespace,
// because inference workloads use --ipc=host; see partition_ppu.go, which
// shares the same assumption). It composes with AMD's device injection
// (internal/agent/gpu/amd_runtime.go), which has already run by the time
// apply is called: the selected card's render node and /dev/kfd are already
// in the workload spec, and this only adds the memory quota on top. There is
// no compute/SM counterpart: ROCm enforces compute quotas through
// HSA_CU_MASK, read by ROCr below anything this preloaded library can see.
type amdPartitionInjector struct{}

func (amdPartitionInjector) apply(e *Engine, _ context.Context, ws *agentruntime.WorkloadSpec, spec proto.InstanceDesiredState) (bool, string) {
	if len(spec.GPUIndexes) == 0 {
		return false, "AMD logical slicing requires at least one selected device"
	}
	backend, _ := accelerator.Lookup(accelerator.BackendAMDLogical)
	asset, err := e.scanPartitionAssets(backend, "amd", nil)
	if err != nil {
		return false, "AMD partition asset unavailable: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Join(e.stateDir, partitionStateRoot, "instances", sanitizeID(spec.UUID)), 0o755); err != nil {
		return false, "AMD partition state directory: " + err.Error()
	}
	target := accelerator.AMDLibraryTarget
	ws.Mounts = append(ws.Mounts, agentruntime.Mount{Source: asset.Files["libvrocm.so"], Target: target, ReadOnly: true})
	if ws.Env == nil {
		ws.Env = map[string]string{}
	}
	ws.Env[accelerator.EnvLDPreload] = target
	limitMiB := strconv.FormatInt((spec.VRAMLimitBytes+(1<<20)-1)/(1<<20), 10)
	for i := range spec.GPUIndexes {
		ws.Env[accelerator.AMDEnvMemoryLimitPrefix+strconv.Itoa(i)] = limitMiB
	}
	ws.Env[accelerator.AMDEnvLedgerPath] = amdLedgerPath(spec.UUID)
	return true, ""
}

func (amdPartitionInjector) assetReports(e *Engine) []proto.AssetReport {
	return scanAssetReports(e, accelerator.BackendAMDLogical, "amd", nil)
}

// libvrocm.so reads VROCM_DEVICE_MEMORY_LIMIT_* from its own process
// environment once, at HIP runtime init; a running instance keeps that value
// until it restarts.
func (amdPartitionInjector) applyLimit(*Engine, proto.InstanceDesiredState) (bool, error) {
	return false, errLiveLimitUnsupported
}

func (amdPartitionInjector) cleanupInstance(e *Engine, uuid string) error {
	err := os.RemoveAll(filepath.Join(e.stateDir, partitionStateRoot, "instances", sanitizeID(uuid)))
	// The vrocm ledger lives in the host IPC namespace because inference
	// workloads use --ipc=host. The shim unlinks a clean ledger on process
	// exit, but a failed or killed startup can leave one behind; remove the
	// exact per-instance path during teardown so retries do not accumulate
	// stale accounting files (same rationale as ppuLedgerPath's cleanup).
	if removeErr := os.Remove(amdLedgerPath(uuid)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}
	return err
}

func amdLedgerPath(uuid string) string {
	return filepath.Join("/dev/shm", "gpuworks-amd-"+sanitizeID(uuid))
}
