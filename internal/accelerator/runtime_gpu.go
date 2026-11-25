package accelerator

import "strings"

// Whether a container runtime needs a vendor plugin to give a container
// accelerators, which is vendor knowledge and so belongs here.
//
// It differs per vendor and per runtime, and getting it wrong in either
// direction is costly: demanding a plugin that is not needed takes a
// working node out of service, and not demanding one that is leaves every
// deployment on the node failing at container start with an error from the
// runtime that names no cause.

// Whether a vendor's runtime can be asked on the host whether it can
// actually use a card is not stated here: that depends on which library and
// CLI the agent would have to call, which is agent mechanism rather than
// vendor identity. The agent's gpu.HostRuntimeCheckFor is the one place that
// knows it.

// Why a runtime cannot give a container this node's accelerators. Codes
// rather than sentences, so the text an operator reads is in their own
// language (the plugin itself is named by RuntimeGPUPlugin).
//
// They are two codes because the remedy differs: one is "install the
// vendor's plugin", the other "configure the daemon you already have".
const (
	// RuntimeGPUBlockedPluginMissing: the vendor's container plugin is not
	// installed. This is what can be established about a runtime that has
	// no daemon to ask — nerdctl consumes the toolkit directly.
	RuntimeGPUBlockedPluginMissing = "gpu_plugin_missing"
	// RuntimeGPUBlockedRuntimeUnconfigured: the container daemon is
	// running and answered, and has no GPU support configured. The
	// daemon's own answer, never the agent's PATH: a toolkit installed
	// beside the agent says nothing about what dockerd was configured
	// with, and treating it as readiness is what let unconfigured nodes
	// take deployments that then failed at container start.
	RuntimeGPUBlockedRuntimeUnconfigured = "gpu_runtime_unconfigured"
)

// The vendor container plugins, by the names their vendors give them. The
// agent's probers report a detected plugin under these names and the server
// names the one a node is missing with them, so both say the same thing.
const (
	PluginNVIDIAContainerToolkit = "NVIDIA Container Toolkit"
	PluginAscendDockerRuntime    = "Ascend Docker Runtime"
)

// RuntimeGPUPlugin names the vendor container plugin this runtime driver
// needs to expose accelerators, or "" when it needs none.
//
//   - NVIDIA: docker and nerdctl both go through the Container Toolkit.
//   - Ascend: docker needs the Ascend Docker Runtime; nerdctl is given the
//     device nodes and the driver mounts directly (see the agent's
//     DriverAwareInjector), so it needs no plugin.
//   - udocker binds the device nodes itself, whatever the vendor.
//   - Hygon and PPU expose their cards by device passthrough, which the
//     agent arranges itself.
//   - A node with no accelerator vendor needs nothing.
func RuntimeGPUPlugin(vendor, runtimeDriver string) string {
	spec, _ := vendorSpecOf(vendor)
	return spec.gpuPlugins[strings.ToLower(strings.TrimSpace(runtimeDriver))]
}

// RuntimeNeedsGPUPlugin reports whether this runtime driver needs the
// vendor's container plugin installed to expose accelerators.
func RuntimeNeedsGPUPlugin(vendor, runtimeDriver string) bool {
	return RuntimeGPUPlugin(vendor, runtimeDriver) != ""
}

// RuntimeGPUReady is the code a node reports for a runtime it passed over
// (RuntimeGPUFallback) once that runtime can give containers the node's
// accelerators after all: the agent stays on runveil while instances run
// there and switches back once none do.
const RuntimeGPUReady = "gpu_ready"

// RuntimeGPUFallback reports whether runtime auto-detection may pass over
// a runtime that cannot give containers this vendor's accelerators and
// take runveil instead, which binds the device nodes itself.
//
//   - NVIDIA: runveil binds the device nodes and the host's driver
//     libraries, so a node without the Container Toolkit still serves.
//   - Ascend: not taken. runveil (PRoot) fails CANN on arm64 910B hosts
//     (docs/reference/ascend-support-matrix.md), so the node keeps the
//     runtime it has and says which plugin it lacks.
//   - The others need no plugin under any runtime; nothing is passed over.
func RuntimeGPUFallback(vendor string) bool {
	spec, _ := vendorSpecOf(vendor)
	return spec.gpuFallback
}
