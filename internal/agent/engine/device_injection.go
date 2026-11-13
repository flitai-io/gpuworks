package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func (e *Engine) applyDeviceInjection(ws *runtime.WorkloadSpec, indexes []int) error {
	driverName := ""
	if d := e.Driver(); d != nil {
		driverName = d.Name()
	}
	return e.applyDeviceInjectionForDriver(ws, indexes, driverName)
}

func (e *Engine) applyDeviceInjectionForDriver(ws *runtime.WorkloadSpec, indexes []int, driverName string) error {
	return e.applyDeviceInjectionForLaunch(context.Background(), ws, indexes, driverName, 1)
}

func (e *Engine) applyDeviceInjectionForLaunch(ctx context.Context, ws *runtime.WorkloadSpec, indexes []int, driverName string, nNodes int) error {
	if e.prober == nil {
		return nil
	}
	if len(indexes) == 0 {
		if accelerator.RequiresDeviceSelection(e.prober.Vendor()) {
			return gpu.DeviceSelectionError(e.prober.Vendor())
		}
		return nil
	}
	inj := gpu.ResolveInjection(ctx, e.prober, gpu.InjectionRequest{Indexes: indexes, DriverName: driverName, NNodes: nNodes, DockerArgs: ws.DockerArgs, Env: ws.Env})
	if inj.Failure != nil {
		return &gpu.InjectionError{Failure: *inj.Failure, Reason: inj.UnsupportedReason}
	}
	if inj.UnsupportedReason != "" {
		return fmt.Errorf("accelerator runtime unavailable: %s", inj.UnsupportedReason)
	}
	applyRuntimeInjection(ws, inj, indexes, driverName)
	return nil
}

// applyRuntimeInjection applies only a fully validated injection. Unsets and
// additions are built on copies, then committed together so callers never see
// a partially applied device selection.
func applyRuntimeInjection(ws *runtime.WorkloadSpec, inj gpu.RuntimeInjection, indexes []int, driverName string) {
	updated := *ws
	env := make(map[string]string, len(ws.Env)+len(inj.Env))
	for key, value := range ws.Env {
		env[key] = value
	}
	if inj.ManagedSelection != nil {
		for _, key := range inj.ManagedSelection.Env {
			delete(env, key)
		}
	}
	for key, value := range inj.Env {
		// NVIDIA Container Toolkit uses the host device IDs from --gpus to
		// construct a compact container-visible inventory. Reusing the host
		// indexes in CUDA_VISIBLE_DEVICES then hides non-zero selected cards
		// (for example host device 1 is CUDA device 0 inside a one-card
		// container). nerdctl's --gpus goes through the same toolkit hook, and
		// the kube runtime gets the same remapping from the NVIDIA runtime
		// class. Udocker has no such remapping and continues to use the
		// physical indexes supplied by the prober.
		if key == "CUDA_VISIBLE_DEVICES" && (driverName == "docker" || driverName == "nerdctl" || driverName == "kube") && hasDockerOption(inj.DockerArgs, "--gpus") {
			value = compactDeviceIndexes(len(indexes))
		}
		env[key] = value
	}
	updated.Env = env

	args := append([]string(nil), ws.DockerArgs...)
	if cleanup := inj.ManagedSelection; cleanup != nil {
		args = removeDockerDevicePaths(args, cleanup.DevicePaths)
		for _, option := range cleanup.DockerOptions {
			args = removeDockerOption(args, option)
		}
	}
	updated.DockerArgs = mergeDeviceDockerArgs(args, inj.DockerArgs)
	if len(inj.Mounts) > 0 {
		mounts := append([]runtime.Mount(nil), ws.Mounts...)
		for _, mount := range inj.Mounts {
			mounts = append(mounts, runtime.Mount{Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly})
		}
		updated.Mounts = mounts
	}
	*ws = updated
}

// removeDockerDevicePaths removes only --device mappings whose host source
// names a prober-managed device path. Other devices and malformed options are
// preserved; device backends replace their accelerator nodes without dropping
// unrelated access such as /dev/infiniband.
func removeDockerDevicePaths(args, paths []string) []string {
	managed := make(map[string]bool, len(paths))
	for _, path := range paths {
		managed[filepath.Clean(path)] = true
	}
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--device" {
			if index+1 == len(args) {
				result = append(result, arg)
				continue
			}
			value := args[index+1]
			if managedDockerDevice(value, managed) {
				index++
				continue
			}
			result = append(result, arg, value)
			index++
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--device="); ok && managedDockerDevice(value, managed) {
			continue
		}
		result = append(result, arg)
	}
	return result
}

func managedDockerDevice(value string, managed map[string]bool) bool {
	// DRM by-path aliases contain the colon in their PCI BDF, so split-at-first
	// colon parsing does not recover the source path. Match declared full source
	// paths first, then retain the existing parser for ordinary device paths.
	for path := range managed {
		if value == path || strings.HasPrefix(value, path+":") {
			return true
		}
	}
	return managed[filepath.Clean(dockerDeviceSource(value))]
}

func dockerDeviceSource(value string) string {
	return strings.SplitN(value, ":", 2)[0]
}

func removeDockerOption(args []string, option string) []string {
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, option+"=") {
			continue
		}
		if arg == option {
			if index+1 < len(args) {
				index++
			}
			continue
		}
		result = append(result, arg)
	}
	return result
}

func hasDockerOption(args []string, option string) bool {
	for _, arg := range args {
		if arg == option || strings.HasPrefix(arg, option+"=") {
			return true
		}
	}
	return false
}

func compactDeviceIndexes(count int) string {
	indexes := make([]string, count)
	for index := range indexes {
		indexes[index] = strconv.Itoa(index)
	}
	return strings.Join(indexes, ",")
}
