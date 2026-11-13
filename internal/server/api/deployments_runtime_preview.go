package api

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func imagePartitionCompatibilityWarnings(image *store.Image, node *store.Node) []messages.Warning {
	if image == nil || node == nil {
		return nil
	}
	var warnings []messages.Warning
	// Every backend whose build profiles serve the image framework contributes
	// its own unbuilt/unverified warning; backends without profiles (NVIDIA)
	// and frameworks a backend does not serve stay silent.
	for _, backend := range accelerator.All() {
		if !accelerator.SupportsFramework(backend, image.Framework) {
			continue
		}
		matchedGPU := false
		for _, gpu := range node.GPUs {
			gpuVendor := gpu.Vendor
			if gpuVendor == "" {
				gpuVendor = node.GPUVendor
			}
			if backend.Matches(gpuVendor, gpu.ArchID) {
				matchedGPU = true
				break
			}
		}
		if !matchedGPU {
			continue
		}
		profile, warning := accelerator.ProfileCompatibility(backend.ID(), image.Framework, image.FrameworkVersion, node.Arch)
		if warning != "" {
			warnings = append(warnings, messages.NewWarning(messages.WarningImagePartitionProfileUnbuilt, map[string]any{
				"framework_version": image.FrameworkVersion,
				"arch":              accelerator.NormalizeArch(node.Arch),
			}))
		} else if profile.Backend != "" && !profile.Verified {
			warnings = append(warnings, messages.NewWarning(messages.WarningImagePartitionProfileUnverified, map[string]any{
				"framework_version": image.FrameworkVersion,
				"arch":              accelerator.NormalizeArch(node.Arch),
			}))
		}
	}
	return warnings
}
