package api

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// Fix codes of dto.NodeGPUIsolationFix; the console has the text for each.
const (
	gpuIsolationFixOperatorDevicePlugin = "gpu_operator_device_plugin"
	gpuIsolationFixOperatorLegacyAppend = "gpu_operator_toolkit_legacy_append"
	gpuIsolationFixOperatorLegacyCreate = "gpu_operator_toolkit_legacy_create"
	gpuIsolationFixHostLegacy           = "host_toolkit_legacy"
)

// clusterPolicyPatch patches the cluster's one ClusterPolicy (the GPU
// Operator's configuration), whatever it is named. Never offer NVIDIA's
// device plugin next to another device plugin: both would advertise the
// same cards, so the first fix is only for a cluster without one.
const clusterPolicyPatch = `kubectl patch clusterpolicy "$(kubectl get clusterpolicy -o jsonpath='{.items[0].metadata.name}')"`

var (
	// The Operator's own device plugin writes the per-card CDI names.
	gpuIsolationOperatorDevicePlugin = []string{
		clusterPolicyPatch + ` --type=merge -p '{"spec":{"devicePlugin":{"enabled":true}}}'`,
	}
	// Set the toolkit back to legacy mode, which takes card indexes. The
	// append form fails where spec.toolkit.env does not exist yet, and the
	// create form would replace the entries of one that does.
	gpuIsolationOperatorLegacyAppend = []string{
		clusterPolicyPatch + ` --type=json -p '[{"op":"add","path":"/spec/toolkit/env/-","value":{"name":"NVIDIA_CONTAINER_RUNTIME_MODE","value":"legacy"}}]'`,
	}
	gpuIsolationOperatorLegacyCreate = []string{
		clusterPolicyPatch + ` --type=json -p '[{"op":"add","path":"/spec/toolkit/env","value":[{"name":"NVIDIA_CONTAINER_RUNTIME_MODE","value":"legacy"}]}]'`,
	}
	// A toolkit installed on the host: nvidia-ctk edits its configuration
	// (/etc/nvidia-container-runtime/config.toml by default) in place, and
	// containerd reads it again after a restart.
	gpuIsolationHostLegacy = []string{
		"sudo nvidia-ctk config --in-place --set nvidia-container-runtime.mode=legacy",
		"sudo systemctl restart containerd",
	}
)

// nodeGPUIsolationDTO is a node's soft-isolation notice with the commands
// that end it, chosen by whose toolkit the agent found: the GPU Operator's,
// one on the host, or (when it could not tell) both.
func nodeGPUIsolationDTO(reported *proto.NVIDIAIsolation) *dto.NodeGPUIsolation {
	if reported == nil {
		return nil
	}
	out := &dto.NodeGPUIsolation{Level: reported.Level, Reason: reported.Reason, Toolkit: reported.Toolkit, Cards: reported.Cards, Fixes: []dto.NodeGPUIsolationFix{}}
	if reported.Toolkit != proto.NVIDIAToolkitHost {
		out.Fixes = append(out.Fixes,
			dto.NodeGPUIsolationFix{Code: gpuIsolationFixOperatorDevicePlugin, Commands: gpuIsolationOperatorDevicePlugin},
			dto.NodeGPUIsolationFix{Code: gpuIsolationFixOperatorLegacyAppend, Commands: gpuIsolationOperatorLegacyAppend},
			dto.NodeGPUIsolationFix{Code: gpuIsolationFixOperatorLegacyCreate, Commands: gpuIsolationOperatorLegacyCreate})
	}
	if reported.Toolkit != proto.NVIDIAToolkitGPUOperator {
		out.Fixes = append(out.Fixes, dto.NodeGPUIsolationFix{Code: gpuIsolationFixHostLegacy, Commands: gpuIsolationHostLegacy})
	}
	return out
}

// fillNodeGPUIsolation sets the soft-isolation notice each online node last
// reported.
func (a *apiHandlers) fillNodeGPUIsolation(out []dto.Node) {
	if a.d.NodeRuntime == nil {
		return
	}
	for i := range out {
		out[i].GPUIsolation = nodeGPUIsolationDTO(a.d.NodeRuntime.NVIDIAIsolation(out[i].UUID))
	}
}
