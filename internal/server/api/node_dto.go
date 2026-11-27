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

// nodeView is n as the API shows it, with the registered scheduling
// holders' answer for it.
func (a *apiHandlers) nodeView(n *store.Node) dto.Node {
	view := a.nodeConnectionView(*n)
	return a.withCardHolds(n, nodeDTO(&view, false, a.nodeHold(n)))
}

func (a *apiHandlers) nodeViewWithToken(n *store.Node) dto.Node {
	view := a.nodeConnectionView(*n)
	return a.withCardHolds(n, nodeDTO(&view, true, a.nodeHold(n)))
}

// withCardHolds adds to d, n's view, why the card holders hold each card
// its row says they hold (store.Store.CardHoldOf).
func (a *apiHandlers) withCardHolds(n *store.Node, d dto.Node) dto.Node {
	for i := range d.GPUs {
		if i < len(n.GPUs) {
			d.GPUs[i].SchedulingHold = a.cardHoldMessage(n, n.GPUs[i])
		}
	}
	return d
}

// cardHoldMessage is why a card holder keeps g out of scheduling; nil when
// none does.
func (a *apiHandlers) cardHoldMessage(n *store.Node, g store.GPU) *dto.ProductMessage {
	if a.d.Store == nil || !g.HeldByHolder() {
		return nil
	}
	pe := a.d.Store.CardHoldOf(n, g)
	if pe == nil {
		return nil
	}
	return &dto.ProductMessage{Code: pe.Code(), Params: pe.Params(), Message: pe.Error()}
}

func (a *apiHandlers) nodeHold(n *store.Node) store.NodeHold {
	if a.d.Store == nil {
		return store.NodeHold{}
	}
	return a.d.Store.HoldOf(n)
}

func nodeDTO(n *store.Node, includeToken bool, hold store.NodeHold) dto.Node {
	d := dto.Node{
		UUID:                  n.UUID,
		Scheduling:            nodeSchedulingName(n.Scheduling),
		SchedulingBy:          string(n.SchedulingBy),
		CordonReason:          n.CordonReason(),
		Uncordon:              nodeUncordon(n, hold),
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
		LastRecoveryCode:      n.LastRecoveryCode,
		HasSSHCredential:      n.HasSSHCredential,
		CreatedAt:             utcTime(n.CreatedAt),
		GPUs:                  make([]dto.GPU, 0, len(n.GPUs)),
	}
	if hold.Held() {
		pe := hold.ProductError()
		d.CordonMessage = &dto.ProductMessage{Code: pe.Code(), Params: pe.Params(), Message: pe.Error()}
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
		d.RuntimeFallbackSocket = runtimeSocketDenialDTO(n.RuntimeSocketDenial())
	}
	d.SlurmNode, d.SlurmUnavailable, d.SlurmReason = n.SlurmNode, n.SlurmUnavailable, n.SlurmReason
	d.SlurmFixes = slurmFixes(n)
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
			SchedulingHeldBy:      string(g.SchedulingHeldBy),
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

// Uncordon values (dto.Node.Uncordon).
const (
	nodeUncordonAllowed = "allowed"
	nodeUncordonConfirm = "confirm"
	nodeUncordonRefused = "refused"
)

// nodeUncordon is what turning n's scheduling back on does: refused while
// held (store.SetNodeScheduling answers ErrNodeHeld); confirm while the
// platform has a reason to cordon it, which it does again on the next
// report; otherwise allowed.
func nodeUncordon(n *store.Node, hold store.NodeHold) string {
	if hold.Held() {
		return nodeUncordonRefused
	}
	cards := make([]proto.GPUInfo, 0, len(n.GPUs))
	for _, g := range n.GPUs {
		cards = append(cards, proto.GPUInfo{Ownership: g.Ownership, HealthCode: g.HealthCode})
	}
	if store.PlatformCordonReason(n.RuntimeGPUBlocked, n.KubeUnschedulable, n.SlurmUnavailable, cards) != "" {
		return nodeUncordonConfirm
	}
	return nodeUncordonAllowed
}
