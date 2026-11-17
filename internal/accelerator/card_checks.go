package accelerator

import (
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Card checks: each thing this platform checks on every card and how often
// each one runs. The agent reports one result per card and check (a check
// status and a code, proto.CardCheck); the server works out freshness from the
// period; the client renders the names in its own locale. Both sides read the
// catalog from here so neither keeps a second copy of the periods.

// Check ids, in the order they are shown.
const (
	// CheckVendorHealth is the vendor tool's verdict on the card: ECC and
	// row remapping (NVIDIA, PPU), npu-smi's health (Ascend), RAS counters
	// and bad pages (Hygon).
	CheckVendorHealth = "vendor_health"
	// CheckVendorCLI is whether the vendor tool answered at all.
	CheckVendorCLI = "vendor_cli"
	// CheckOccupancy is whether processes outside the platform use the card.
	CheckOccupancy = "occupancy"
	// CheckPCIeLink is the width and speed of the links between the card and
	// its root port.
	CheckPCIeLink = "pcie_link"
	// CheckPCIeErrors is the AER error counters on that path.
	CheckPCIeErrors = "pcie_errors"
	// CheckXid is the NVIDIA driver's Xid events for the card.
	CheckXid = "xid"
	// CheckNVLink is whether the card's NVLinks are all up.
	CheckNVLink = "nvlink"
	// CheckRuntime is whether the vendor runtime can create a context on the
	// card and allocate on it.
	CheckRuntime = "runtime"
	// CheckPeerMemory is whether the host's RDMA NICs can register the card's
	// memory (GPUDirect RDMA): the vendor's peer-memory kernel module is
	// loaded. Only hosts with RDMA NICs are asked; PD modes that move the KV
	// over RDMA need it, nothing else does, so a missing module warns and
	// leaves the card schedulable.
	CheckPeerMemory = "peer_memory"
)

// PeerMemoryModule is the kernel module through which a vendor's driver lets
// RDMA NICs register its cards' memory, or "" for a vendor without one this
// platform checks.
func PeerMemoryModule(vendor string) string {
	switch NormalizeVendor(vendor) {
	case VendorPPU:
		return "alixpu_peermem"
	case VendorNVIDIA:
		return "nvidia_peermem"
	}
	return ""
}

// PeerMemoryFix is the command that loads a vendor's peer-memory module now
// and on every boot, or "".
func PeerMemoryFix(vendor string) string {
	module := PeerMemoryModule(vendor)
	if module == "" {
		return ""
	}
	return "modprobe " + module + " && echo " + module + " > /etc/modules-load.d/" + module + ".conf"
}

// CardCheckSpec is one check in the catalog.
type CardCheckSpec struct {
	ID string
	// Period is how often the check produces a new answer when nothing holds
	// it back. Checks that run with every status report share ReportPeriod.
	Period time.Duration
	// Vendors are the vendors the check exists for; empty means every vendor
	// except Apple's, whose unified GPU has none of these interfaces.
	Vendors []string
}

// ReportPeriod is how often the agent reports a node's status, and with it
// every check that runs on each report.
const ReportPeriod = 60 * time.Second

// RuntimeCheckPeriod and NVLinkCheckPeriod are the two checks that run on a
// schedule of their own.
const (
	RuntimeCheckPeriod = 15 * time.Minute
	NVLinkCheckPeriod  = 5 * time.Minute
)

var cardCheckCatalog = []CardCheckSpec{
	{ID: CheckVendorHealth, Period: ReportPeriod},
	{ID: CheckVendorCLI, Period: ReportPeriod},
	{ID: CheckOccupancy, Period: ReportPeriod},
	{ID: CheckPCIeLink, Period: ReportPeriod},
	{ID: CheckPCIeErrors, Period: ReportPeriod},
	{ID: CheckXid, Period: ReportPeriod, Vendors: []string{VendorNVIDIA}},
	{ID: CheckNVLink, Period: NVLinkCheckPeriod, Vendors: []string{VendorNVIDIA}},
	{ID: CheckRuntime, Period: RuntimeCheckPeriod, Vendors: []string{VendorNVIDIA, VendorAscend, VendorHygon, VendorPPU}},
	{ID: CheckPeerMemory, Period: ReportPeriod, Vendors: []string{VendorNVIDIA, VendorPPU}},
}

// CardChecks are the checks that exist for a vendor's cards, in display order.
func CardChecks(vendor string) []CardCheckSpec {
	vendor = NormalizeVendor(vendor)
	if vendor == VendorApple || vendor == "" {
		return nil
	}
	out := make([]CardCheckSpec, 0, len(cardCheckCatalog))
	for _, spec := range cardCheckCatalog {
		if len(spec.Vendors) == 0 || containsVendor(spec.Vendors, vendor) {
			out = append(out, spec)
		}
	}
	return out
}

// CardCheckIDs are every check id in display order.
func CardCheckIDs() []string {
	ids := make([]string, len(cardCheckCatalog))
	for i, spec := range cardCheckCatalog {
		ids[i] = spec.ID
	}
	return ids
}

// CardCheckFor returns the catalog entry for an id.
func CardCheckFor(id string) (CardCheckSpec, bool) {
	for _, spec := range cardCheckCatalog {
		if spec.ID == id {
			return spec, true
		}
	}
	return CardCheckSpec{}, false
}

func containsVendor(vendors []string, vendor string) bool {
	for _, v := range vendors {
		if v == vendor {
			return true
		}
	}
	return false
}

// CheckStatusFor is the check status a finding code puts a check in: a code
// that leaves the card schedulable warns, any other fails, none passes.
func CheckStatusFor(code string) string {
	switch HealthLevel(code) {
	case HealthOK:
		return proto.CheckPass
	case HealthWarning:
		return proto.CheckWarn
	}
	return proto.CheckFail
}

// CheckProblem reports whether a check status is one a person has to act on.
func CheckProblem(status string) bool { return status == proto.CheckWarn || status == proto.CheckFail }

// CardCheckProbe is the command that shows what a check looks at on a card,
// whatever it found: what a person runs to see it for themselves. It is ""
// when there is no command for it (the check reads a stream, or the card
// cannot be addressed).
func CardCheckProbe(check string, card HealthCard) string {
	target := HealthTarget(card.BDF, card.Index)
	bdf := NormalizeBDF(strings.TrimSpace(card.BDF))
	switch check {
	case CheckVendorHealth:
		switch card.Vendor {
		case VendorNVIDIA:
			if target != "" {
				return "nvidia-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i " + target
			}
		case VendorPPU:
			if target != "" {
				return "ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i " + target
			}
		case VendorAscend:
			return "npu-smi info"
		case VendorHygon:
			if bdf != "" {
				ras := "/sys/bus/pci/devices/" + bdf + "/ras/"
				return "grep . " + ras + "*_err_count; cat " + ras + "gpu_vram_bad_pages"
			}
		}
	case CheckVendorCLI, CheckOccupancy:
		return ToolCommand(card.Vendor)
	case CheckPCIeLink:
		if bdf != "" {
			return "lspci -vv -s " + bdf + " | grep -E 'LnkCap:|LnkSta:'"
		}
	case CheckPCIeErrors:
		if bdf != "" {
			return "grep -H TOTAL /sys/bus/pci/devices/" + bdf + "/aer_dev_*"
		}
	case CheckXid:
		return "dmesg | grep -i xid"
	case CheckPeerMemory:
		if module := PeerMemoryModule(card.Vendor); module != "" {
			return "lsmod | grep " + module
		}
	case CheckNVLink:
		if target != "" {
			return "nvidia-smi nvlink -s -e -i " + target
		}
	case CheckRuntime:
		card.Code = HealthRuntimeUnusable
		if guide, ok := runtimeUnusableGuide(card); ok {
			return guide.Probe
		}
	}
	return ""
}
