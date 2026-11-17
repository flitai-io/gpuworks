package app

import (
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// peerMemoryRoot is where the peer-memory check reads the host's sysfs; a
// test points it at a directory of its own.
var peerMemoryRoot = "/sys"

// peerMemoryState is whether the host's RDMA NICs can register the cards of
// vendor: proto.PeerMemoryLoaded or proto.PeerMemoryMissing, or "" when the
// host has no RDMA NIC or the vendor has no peer-memory module to check.
func peerMemoryState(root, vendor string) string {
	module := accelerator.PeerMemoryModule(vendor)
	if module == "" {
		return ""
	}
	nics, err := os.ReadDir(filepath.Join(root, "class", "infiniband"))
	if err != nil || len(nics) == 0 {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, "module", module)); err == nil {
		return proto.PeerMemoryLoaded
	}
	return proto.PeerMemoryMissing
}

// PeerMemory is the node's peer-memory state, reported with its status.
func (n *NodeRuntime) PeerMemory() string {
	return peerMemoryState(peerMemoryRoot, n.vendor())
}

// peerMemoryChecks puts the peer-memory check on every card of a vendor that
// has a module for it, on a host with RDMA NICs: pass when the module is
// loaded, warn with the command that loads it otherwise. The card stays
// schedulable either way; only PD modes over RDMA need the module.
func (n *NodeRuntime) peerMemoryChecks(gpus []proto.GPUInfo, now time.Time, round checkRound) {
	for i := range gpus {
		card := &gpus[i]
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if ident == "" || !accelerator.CardUsable(card.Ownership) {
			continue
		}
		vendor := accelerator.NormalizeVendor(card.Vendor, n.vendor())
		switch peerMemoryState(peerMemoryRoot, vendor) {
		case proto.PeerMemoryLoaded:
			round.put(ident, accelerator.CheckPeerMemory, checkResult{status: proto.CheckPass, at: now})
		case proto.PeerMemoryMissing:
			round.put(ident, accelerator.CheckPeerMemory, checkResult{
				status: proto.CheckWarn, code: proto.DoctorPeerMemoryMissing, at: now,
				evidence: []accelerator.HealthField{
					{Name: "module", Value: accelerator.PeerMemoryModule(vendor)},
					{Name: "fix", Value: accelerator.PeerMemoryFix(vendor)},
				},
			})
		}
	}
}

// doctorPeerMemory reports the same condition the peer-memory check warns
// about, so a diagnosis never reads OK where the health checks do not.
func doctorPeerMemory(r *proto.DoctorReport, vendor string) {
	vendor = accelerator.NormalizeVendor(vendor, "")
	if peerMemoryState(peerMemoryRoot, vendor) != proto.PeerMemoryMissing {
		return
	}
	module := accelerator.PeerMemoryModule(vendor)
	r.Findings = append(r.Findings, proto.DoctorFinding{
		ID: proto.DoctorCheckPeerMemory, Status: proto.CheckWarn, Code: proto.DoctorPeerMemoryMissing,
		Probe: "ls /sys/module/" + module,
		Evidence: []proto.DoctorField{
			{Name: "module", Value: module},
			{Name: "fix", Value: accelerator.PeerMemoryFix(vendor)},
		},
		Recheck: "lsmod | grep " + module,
	})
}
