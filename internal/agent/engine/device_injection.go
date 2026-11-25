package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func (e *Engine) applyDeviceInjection(ws *runtime.WorkloadSpec, indexes []int) error {
	return e.applyDeviceInjectionForDriver(ws, indexes, runtime.TargetOf(e.Driver()))
}

func (e *Engine) applyDeviceInjectionForDriver(ws *runtime.WorkloadSpec, indexes []int, target runtime.Target) error {
	return e.applyDeviceInjectionForLaunch(context.Background(), ws, indexes, target, 1)
}

func (e *Engine) applyDeviceInjectionForLaunch(ctx context.Context, ws *runtime.WorkloadSpec, indexes []int, target runtime.Target, nNodes int) error {
	if e.prober == nil {
		return nil
	}
	if len(indexes) == 0 {
		if accelerator.RequiresDeviceSelection(e.prober.Vendor()) {
			return gpu.DeviceSelectionError(e.prober.Vendor())
		}
		return nil
	}
	inj := gpu.ResolveInjection(ctx, e.prober, gpu.InjectionRequest{Indexes: indexes, Runtime: target, NNodes: nNodes, DockerArgs: ws.DockerArgs, Env: ws.Env})
	if inj.Failure != nil {
		return &gpu.InjectionError{Failure: *inj.Failure, Reason: inj.UnsupportedReason}
	}
	if inj.UnsupportedReason != "" {
		return fmt.Errorf("accelerator runtime unavailable: %s", inj.UnsupportedReason)
	}
	applyRuntimeInjection(ws, inj, indexes, target)
	return nil
}

// applyRuntimeInjection applies only a fully validated injection. Unsets and
// additions are built on copies, then committed together so callers never see
// a partially applied device selection.
func applyRuntimeInjection(ws *runtime.WorkloadSpec, inj gpu.RuntimeInjection, indexes []int, target runtime.Target) {
	updated := *ws
	env := make(map[string]string, len(ws.Env)+len(inj.Env))
	for key, value := range ws.Env {
		env[key] = value
	}
	for _, key := range inj.ClearEnv {
		delete(env, key)
	}
	for key, value := range inj.Env {
		// NVIDIA Container Toolkit uses the host device IDs from --gpus to
		// construct a compact container-visible inventory. Reusing the host
		// indexes in CUDA_VISIBLE_DEVICES then hides non-zero selected cards
		// (for example host device 1 is CUDA device 0 inside a one-card
		// container). That holds wherever a container sees only the devices
		// it was granted. Where it sees the host's, the physical indexes
		// supplied by the prober stand.
		if key == "CUDA_VISIBLE_DEVICES" && target.View == runtime.ViewGranted && len(inj.Grant.ToolkitDevices) > 0 {
			value = compactDeviceIndexes(len(indexes))
		}
		env[key] = value
	}
	updated.Env = env

	// What the grant replaces among the workload's own options is left out
	// where they are read (WorkloadSpec.OwnDockerArgs).
	updated.Grant = inj.Grant
	if len(inj.Mounts) > 0 {
		mounts := append([]runtime.Mount(nil), ws.Mounts...)
		for _, mount := range inj.Mounts {
			mounts = append(mounts, runtime.Mount{Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly})
		}
		updated.Mounts = mounts
	}
	*ws = updated
}

func compactDeviceIndexes(count int) string {
	indexes := make([]string, count)
	for index := range indexes {
		indexes[index] = strconv.Itoa(index)
	}
	return strings.Join(indexes, ",")
}
