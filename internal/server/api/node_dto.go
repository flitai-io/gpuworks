package api

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const bytesPerGiB = 1024 * 1024 * 1024

func toNodeDTO(n *store.Node) dto.Node {
	return nodeDTO(n, false)
}

func toNodeDTOWithToken(n *store.Node) dto.Node {
	return nodeDTO(n, true)
}

func nodeDTO(n *store.Node, includeToken bool) dto.Node {
	d := dto.Node{
		UUID:                  n.UUID,
		Scheduling:            nodeSchedulingName(n.Scheduling),
		SchedulingBy:          string(n.SchedulingBy),
		CordonReason:          n.CordonReason(),
		Name:                  n.Name,
		State:                 n.ConnectionState(),
		Hostname:              n.Hostname,
		OS:                    n.OS,
		Kernel:                n.Kernel,
		Arch:                  n.Arch,
		CPUModel:              n.CPUModel,
		CPUCores:              n.CPUCores,
		MemTotalGiB:           float64(n.MemTotalKB) * 1024 / bytesPerGiB,
		DiskTotalGiB:          float64(n.DiskTotalKB) * 1024 / bytesPerGiB,
		GPUCount:              n.GPUCount,
		GPUVendor:             nodeDTOGPUVendor(n),
		GPUDriver:             nodeDTOGPUDriver(n),
		NvidiaDriver:          n.NvidiaDriver,
		Framework:             n.Framework,
		FrameworkVersion:      n.FrameworkVersion,
		RuntimeDriver:         n.RuntimeDriver,
		RuntimePreference:     n.RuntimePreference,
		RuntimeChoices:        nodeDTORuntimeChoices(n),
		RuntimePlugin:         n.RuntimePlugin,
		Runtimes:              nodeDTORuntimes(n),
		PartitionCapabilities: partitioncap.ForNode(n),
		ManagementAddress:     managementIP(*n),
		ServerURL:             n.ServerURL,
		ConnectMode:           n.ConnectMode,
		ServerDialAddress:     n.ServerDialAddress,
		ServerDialListen:      n.ServerDialListen,
		AgentVersion:          n.AgentVersion,
		LastRemoteAddr:        n.LastRemoteAddr,
		LastRejectReason:      n.LastRejectReason,
		LastRejectAt:          utcTimePtr(n.LastRejectAt),
		LastSeenAt:            utcTimePtr(n.LastSeenAt),
		RecoveryPhase:         n.RecoveryPhase,
		RecoveryAttempts:      n.RecoveryAttempts,
		LastProbeAt:           utcTimePtr(n.LastProbeAt),
		LastRecoveryError:     n.LastRecoveryError,
		HasSSHCredential:      n.HasSSHCredential,
		CreatedAt:             utcTime(n.CreatedAt),
		GPUs:                  make([]dto.GPU, 0, len(n.GPUs)),
	}
	d.Labels, _ = store.NodeLabels(n.Labels)
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	if includeToken {
		// The command carries the token, so it goes out only where the
		// token itself does.
		d.Token = n.Token
		d.InstallCommand = nodeInstallCommandFor(n)
	}
	if n.PathUsage != "" {
		var usage any
		if json.Unmarshal([]byte(n.PathUsage), &usage) == nil {
			d.PathUsage = usage
		}
	}
	d.RuntimeGPUBlocked = n.RuntimeGPUBlocked
	d.RuntimeGPUPlugin = accelerator.RuntimeGPUPlugin(n.GPUVendor, n.RuntimeDriver)
	if n.RuntimeFallbackFrom != "" {
		d.RuntimeFallbackFrom = n.RuntimeFallbackFrom
		d.RuntimeFallbackReason = n.RuntimeFallbackReason
		d.RuntimeFallbackPlugin = accelerator.RuntimeGPUPlugin(n.GPUVendor, n.RuntimeFallbackFrom)
	}
	d.AcceleratorPreference = n.AcceleratorPreference
	d.AcceleratorVendors = acceleratorChoices(n)
	d.GPUProbeError = n.GPUProbeError
	d.GPUProbeFailedAt = utcTimePtr(n.GPUProbeFailedAt)
	if n.TopoJSON != "" {
		d.Topology = dto.ParseNodeTopology(n.TopoJSON)
	}
	partitionReports := partitioncap.Decode(n)
	for i := range n.GPUs {
		g := &n.GPUs[i]
		arch := n.ArchOf(*g)
		evidence, guide := gpuHealthDetail(g)
		d.GPUs = append(d.GPUs, dto.GPU{
			Index:                 g.Index,
			UUID:                  g.UUID,
			Name:                  g.Name,
			ComputeCapability:     g.ComputeCapability,
			ArchFamily:            arch.Family,
			Vendor:                g.Vendor,
			ArchID:                arch.ID,
			MemoryTotalBytes:      g.MemoryTotal,
			MemoryCapacityKnown:   gpuMemoryCapacityKnown(g.MemoryTotal),
			PowerLimitWatts:       g.PowerLimitWatts,
			PCIeBDF:               g.PCIeBDF,
			NUMANode:              g.NUMANode,
			PartitionCapabilities: partitionReports.ForGPU(g),
			MIGEnabled:            g.MIGEnabled,
			Ownership:             g.Ownership,
			ForeignProcesses:      g.ForeignUse(),
			SchedulingStopped:     g.SchedulingStopped,
			SchedulingAllowed:     g.SchedulingAllowedAt != nil,
			SchedulingActions:     g.SchedulingActions(),
			Schedulable:           g.CanTakeWork(),
			HealthStatus:          g.HealthStatus,
			HealthCode:            g.HealthCode,
			HealthMessage:         g.HealthMessage,
			HealthEvidence:        evidence,
			HealthGuide:           guide,
		})
	}
	return d
}

func nodeDTOGPUVendor(n *store.Node) string {
	if n.GPUVendor != "" {
		return n.GPUVendor
	}
	if n.NvidiaDriver != "" {
		return "nvidia"
	}
	for _, gpu := range append(n.OwnedGPUs(), n.GPUs...) {
		if gpu.Vendor != "" {
			return gpu.Vendor
		}
	}
	return ""
}

func nodeDTOGPUDriver(n *store.Node) string {
	vendor := nodeDTOGPUVendor(n)
	if n.GPUDriver != "" {
		return accelerator.NormalizeDriverVersion(vendor, n.GPUDriver)
	}
	return accelerator.NormalizeDriverVersion(vendor, n.NvidiaDriver)
}

// nodeDTORuntimeChoices is what the runtime selector offers: nothing for a
// Kubernetes node, whose instances are pods; only automatic on a Mac, where
// a container runtime would run in a VM without the Metal GPU and the host
// driver is the way; every driver otherwise.
func nodeDTORuntimeChoices(n *store.Node) []string {
	switch {
	case n.KubeClusterID != 0:
		return nil
	case n.OS == "darwin":
		return []string{proto.RuntimePreferenceAuto}
	}
	return []string{proto.RuntimePreferenceAuto, proto.RuntimePreferenceDocker, proto.RuntimePreferenceNerdctl, proto.RuntimePreferenceUdocker}
}

func nodeDTORuntimes(n *store.Node) []string {
	if n.Runtimes != "" {
		var out []string
		if json.Unmarshal([]byte(n.Runtimes), &out) == nil && len(out) > 0 {
			return out
		}
	}
	if n.RuntimeDriver != "" {
		return []string{n.RuntimeDriver}
	}
	return nil
}
