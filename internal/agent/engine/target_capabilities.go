package engine

import (
	"fmt"
	goruntime "runtime"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// WithHostRuntimeFacts injects host OS and architecture facts for tests. The
// active container driver is always read from the Engine at validation time.
func WithHostRuntimeFacts(read func() accelerator.HostRuntime) EngineOption {
	return func(engine *Engine) {
		engine.hostRuntimeFacts = read
	}
}

// validateTargetDesired admits a new container after runtime inspection. Existing
// containers are adopted before this check. Old wire launches have no snapshot;
// only their explicit structural facts can be checked, never their argv.
func (e *Engine) validateTargetDesired(spec proto.InstanceDesiredState) error {
	driverName := ""
	if driver := e.Driver(); driver != nil {
		driverName = driver.Name()
	}
	return e.validateTargetDesiredForDriver(spec, driverName)
}

func (e *Engine) validateTargetDesiredForDriver(spec proto.InstanceDesiredState, driverName string) error {
	if e.prober == nil {
		return nil
	}
	vendor := accelerator.NormalizeVendor(e.prober.Vendor())
	structuralSleep := spec.Sleepable || spec.DesiredSleep != 0
	structuralMulti := spec.NNodes > 1 || spec.NodeRank != 0 || spec.Headless || spec.DistGroup != "" || spec.DistAttempt != 0 || (spec.DistRole != "" && spec.DistRole != "leader" && spec.DistRole != "standalone")
	structuralKV := false
	for _, mount := range spec.Mounts {
		if mount.Purpose == proto.MountPurposeKVDisk {
			structuralKV = true
		}
	}
	snapshot := spec.RequestedFeatures
	if snapshot == nil {
		// Legacy servers expose only structural facts; their argv is never parsed.
		snapshot = &proto.RequestedFeatures{EngineSleep: structuralSleep, MultiNode: structuralMulti, KVDiskTier: structuralKV}
	} else if structuralSleep && !snapshot.EngineSleep || structuralMulti && !snapshot.MultiNode || structuralKV && !snapshot.KVDiskTier {
		return fmt.Errorf("launch feature snapshot contradicts desired topology or resources")
	}

	var requested []accelerator.Feature
	for _, name := range snapshot.Names() {
		requested = append(requested, accelerator.Feature(name))
	}

	if missing := accelerator.Unsupported(vendor, requested...); len(missing) > 0 {
		return fmt.Errorf("%s does not support requested accelerator features: %s", accelerator.VendorBrand(vendor), strings.Join(featureNames(missing), ", "))
	}
	if spec.RequestedFeatures != nil && !snapshot.CommandKnown && accelerator.RequiresKnownLaunchFeatures(vendor) {
		return fmt.Errorf("launch_features_unknown: %s needs a recognizable executable entrypoint", accelerator.VendorBrand(vendor))
	}
	host := accelerator.HostRuntime{OS: goruntime.GOOS, Arch: goruntime.GOARCH}
	if e.hostRuntimeFacts != nil {
		host = e.hostRuntimeFacts()
	}
	host.Driver = driverName
	if err := gpu.RuntimeRequirementError(vendor, host); err != nil {
		return err
	}

	if accelerator.RequiresDeviceSelection(vendor) && len(spec.GPUIndexes) == 0 {
		return gpu.DeviceSelectionError(vendor)
	}
	if accelerator.RequiresDeviceSelection(vendor) {
		// A vendor that requires explicit device selection may still have a
		// memory-only logical partition backend of its own (AMD's
		// amd-dgpu/libvrocm.so): a quota is accepted only when it names
		// exactly that backend, never a whole-device contract borrowed from
		// another vendor and never a compute/SM limit, which no such backend
		// currently exposes.
		if spec.SMLimitPct != 0 && spec.SMLimitPct != 100 {
			return fmt.Errorf("%s does not support a compute/SM limit", accelerator.VendorBrand(vendor))
		}
		if spec.PartitionBackend != "" || spec.VRAMLimitBytes != 0 {
			backend, ok := accelerator.Lookup(spec.PartitionBackend)
			if !ok || !backend.Matches(vendor, "") {
				return fmt.Errorf("%s requires a whole-device partition contract", accelerator.VendorBrand(vendor))
			}
		}
	}
	return nil
}

// featureNames formats the stable feature identifiers in a target refusal.
func featureNames(features []accelerator.Feature) []string {
	names := make([]string, len(features))
	for i, feature := range features {
		names[i] = string(feature)
	}
	return names
}
