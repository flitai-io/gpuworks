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

// ppuPartitionInjector implements the PPU logical partition backend: the
// ppu-slicing-shim quota/hook libraries (hooking the HGGC runtime) are mounted into the container and activated through
// LD_PRELOAD plus HGGC_DEVICE_* env, with per-instance quota ledgers in
// /dev/shm (host IPC namespace, because inference workloads use --ipc=host).
type ppuPartitionInjector struct{}

func (ppuPartitionInjector) apply(e *Engine, _ context.Context, ws *agentruntime.WorkloadSpec, spec proto.InstanceDesiredState) (bool, string) {
	if len(spec.GPUIndexes) == 0 {
		return false, "PPU logical slicing requires at least one selected device"
	}
	backend, _ := accelerator.Lookup(accelerator.BackendPPULogical)
	asset, err := e.scanPartitionAssets(backend, "ppu", nil)
	if err != nil {
		return false, "PPU partition asset unavailable: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Join(e.stateDir, partitionStateRoot, "instances", sanitizeID(spec.UUID)), 0o755); err != nil {
		return false, "PPU partition state directory: " + err.Error()
	}
	quotaTarget, hgmlTarget := accelerator.PPUQuotaLibraryTarget, accelerator.PPUHookLibraryTarget
	ws.Mounts = append(ws.Mounts,
		agentruntime.Mount{Source: asset.Files["hggc_quota.so"], Target: quotaTarget, ReadOnly: true},
		agentruntime.Mount{Source: asset.Files["hgml_dlsym_hook.so"], Target: hgmlTarget, ReadOnly: true},
	)
	if ws.Env == nil {
		ws.Env = map[string]string{}
	}
	ws.Env[accelerator.EnvLDPreload] = hgmlTarget + ":" + quotaTarget
	ws.Env[accelerator.EnvNVIDIAVisibleDevices] = "void"
	smLimit := spec.SMLimitPct
	if smLimit <= 0 {
		smLimit = 100
	}
	for i := range spec.GPUIndexes {
		ordinal := strconv.Itoa(i)
		ws.Env[accelerator.PPUEnvMemoryLimitPrefix+ordinal] = strconv.FormatInt((spec.VRAMLimitBytes+(1<<20)-1)/(1<<20), 10)
		ws.Env[accelerator.PPUEnvSMLimitPrefix+ordinal] = strconv.Itoa(smLimit)
	}
	ws.Env[accelerator.PPUEnvLedgerPath] = ppuLedgerPath(spec.UUID)
	return true, ""
}

func (ppuPartitionInjector) assetReports(e *Engine) []proto.AssetReport {
	return scanAssetReports(e, accelerator.BackendPPULogical, "ppu", nil)
}

// The quota reaches the backend through its start-time environment and
// config only.
func (ppuPartitionInjector) applyLimit(*Engine, proto.InstanceDesiredState) (bool, error) {
	return false, errLiveLimitUnsupported
}

func (ppuPartitionInjector) cleanupInstance(e *Engine, uuid string) error {
	err := os.RemoveAll(filepath.Join(e.stateDir, partitionStateRoot, "instances", sanitizeID(uuid)))
	// PPU quota ledgers live in the host IPC namespace because inference
	// workloads use --ipc=host. The shim normally unlinks a clean ledger on
	// process exit, but a failed or killed startup can leave one behind. Remove
	// the exact per-instance path during teardown so retries and deletions do not
	// accumulate stale accounting files.
	if removeErr := os.Remove(ppuLedgerPath(uuid)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}
	return err
}

func ppuLedgerPath(uuid string) string {
	return filepath.Join("/dev/shm", "gpuworks-ppu-"+sanitizeID(uuid))
}
