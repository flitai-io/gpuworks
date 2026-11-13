package accelerator

import (
	"slices"
	"strconv"
	"strings"
)

// Evidence field names, spelled as the vendor tool's query interface spells
// them. The agent queries these fields and records them; the probe command a
// person is shown queries exactly the same fields.
var (
	PPUHealthMemoryFields = []string{
		"ecc.errors.uncorrected.volatile.dram",
		"ecc.errors.uncorrected.aggregate.dram",
		"retired_pages.double_bit.count",
		"retired_pages.pending",
		"remapped_rows.uncorrectable",
		"remapped_rows.pending",
		"remapped_rows.failure",
	}
	PPUHealthXIDFields = []string{
		"xid.correctable.ppu_reset",
		"xid.correctable.os_reboot",
		"xid.correctable.cold_reboot",
	}
	NVIDIAHealthMemoryFields = []string{
		"ecc.errors.uncorrected.volatile.total",
		"ecc.errors.uncorrected.aggregate.total",
		"retired_pages.pending",
		"remapped_rows.pending",
		"remapped_rows.failure",
	}
	// NVIDIARecoveryField is the recovery the driver itself asks for after an
	// error ("None", or a reset, a reboot, draining P2P first); drivers from
	// R550 answer it. It is the state behind most Xid events: the driver
	// clears it once the recovery has been done.
	NVIDIARecoveryField = "gpu_recovery_action"
	// HygonRASBlocks are the driver's RAS blocks whose error counts the agent
	// reads (/sys/bus/pci/devices/<bdf>/ras/<block>_err_count): memory first,
	// then the blocks a K100 driver enables RAS for. A block whose file is
	// missing is skipped.
	HygonRASBlocks = []string{"umc", "gfx", "sdma", "mmhub", "hdp", "mp0", "mp1", "pcie_bif", "xgmi_wafl"}
	// AscendHealthFieldNames are the labels npu-smi prints for the chip address and
	// in its health view ("npu-smi info -t health").
	AscendHealthFieldNames = []string{"NPU", "Chip", "Health", "Health Status", "Error Code", "Error Information"}
)

// HealthGuide is what a person runs for a finding: Probe reproduces the evidence
// fields exactly as the agent read them, Recheck is the vendor tool's readable
// view of the same state, Actions are the recovery commands the vendor tool
// itself names for the finding. A finding whose recovery is not a command
// (reboot the host, contact the hardware vendor) has no Actions; the client
// states it in prose.
type HealthGuide struct {
	Probe   string   `json:"probe"`
	Recheck string   `json:"recheck,omitempty"`
	Actions []string `json:"actions,omitempty"`
}

// HealthCard is the card a guide is wanted for. Three ways of naming it,
// because the vendors' interfaces want different ones: the vendor tools take
// the PCI address (or the index, when the address is unknown), while the
// runtime check names the card the way its own runtime does — a UUID for
// CUDA, the die index for Ascend.
type HealthCard struct {
	Vendor string
	Code   string
	BDF    string
	Index  int
	UUID   string
}

// HealthGuideFor returns the commands for a card's finding. ok is false when
// there is nothing reliable to show (no finding, an unknown code, or a vendor
// without a health interface).
func HealthGuideFor(card HealthCard, evidence []HealthField) (HealthGuide, bool) {
	if card.Code == "" {
		return HealthGuide{}, false
	}
	// A card the runtime could not use is every vendor's finding, and the
	// check behind it is this platform's own rather than the vendor tool's,
	// so it is answered here instead of in each vendor's rules.
	if card.Code == HealthRuntimeUnusable {
		return runtimeUnusableGuide(card)
	}
	// PCIe findings are read from sysfs the same way on every vendor.
	if guide, ok := pcieHealthGuide(card.Code, evidence); ok {
		return guide, true
	}
	target := HealthTarget(card.BDF, card.Index)
	if target == "" {
		return HealthGuide{}, false
	}
	switch card.Vendor {
	case VendorKunlunxin:
		if card.Code == HealthMemoryUncorrectable {
			return HealthGuide{Probe: "xpu-smi -q -x", Recheck: "xpu-smi -q -x"}, true
		}
	case VendorPPU:
		return ppuHealthGuide(card.Code, target, evidence)
	case VendorNVIDIA:
		return nvidiaHealthGuide(card.Code, target, evidence)
	case VendorAscend:
		return ascendHealthGuide(card.Code, evidence)
	case VendorHygon:
		return hygonHealthGuide(card.Code, card.BDF)
	}
	return HealthGuide{}, false
}

// runtimeUnusableGuide is what to run for a card the host runtime could not
// use: the check itself, which is the only thing that shows the failure, and
// the vendor tool's view of the same card, which on such a card typically
// reports it as healthy — that disagreement is the finding.
//
// Only the vendors whose runtime can be asked on the host have one
// (gpu.HostRuntimeCheckFor). For the rest the verdict came from their CLI,
// and their own code's guide covers it.
func runtimeUnusableGuide(card HealthCard) (HealthGuide, bool) {
	switch card.Vendor {
	case VendorNVIDIA:
		if card.UUID == "" {
			// Without the UUID the command could only name an index, which
			// addresses a different card once the driver drops one — the
			// mistake the check itself avoids by using UUIDs.
			return HealthGuide{}, false
		}
		guide := HealthGuide{Probe: CUDARuntimeSmokeCommand(card.UUID)}
		if target := HealthTarget(card.BDF, card.Index); target != "" {
			guide.Recheck = "nvidia-smi -q -i " + target
		}
		return guide, true
	case VendorAscend:
		if card.Index < 0 {
			return HealthGuide{}, false
		}
		return HealthGuide{
			Probe: AscendRuntimeSmokeCommand(strconv.Itoa(card.Index)),
			// npu-smi addresses the chip, not the die index; the die's own
			// row is what `npu-smi info` prints for it.
			Recheck: "npu-smi info -m; npu-smi info",
		}, true
	case VendorHygon:
		if strings.TrimSpace(card.BDF) == "" {
			return HealthGuide{}, false
		}
		return HealthGuide{Probe: HygonRuntimeSmokeCommand(card.BDF), Recheck: "hy-smi --healthcheck"}, true
	}
	return HealthGuide{}, false
}

// HealthTarget picks the address the vendor tools accept for a card, and ""
// for one that cannot be addressed. A card the vendor tool never enumerated
// carries no index of its own (it is reported as -1), and naming it "-1"
// would hand the reader a command for a card that does not exist.
func HealthTarget(bdf string, index int) string {
	if bdf = strings.TrimSpace(bdf); bdf != "" {
		return bdf
	}
	if index < 0 {
		return ""
	}
	return strconv.Itoa(index)
}

// probedFields are the evidence fields the vendor query can reproduce: the
// fields the agent recorded when they are all among the known ones, otherwise
// the whole group (a row from an older agent recorded a sentence).
func probedFields(evidence []HealthField, known []string) []string {
	var names []string
	for _, f := range evidence {
		if !slices.Contains(known, f.Name) {
			return known
		}
		names = append(names, f.Name)
	}
	if len(names) == 0 {
		return known
	}
	return names
}

func ppuHealthGuide(code, target string, evidence []HealthField) (HealthGuide, bool) {
	query := func(fields []string) string {
		return "ppu-smi --query-ppu=pci.bus_id," + strings.Join(probedFields(evidence, fields), ",") + " --format=csv -i " + target
	}
	memory := HealthGuide{
		Probe:   query(PPUHealthMemoryFields),
		Recheck: "ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i " + target,
	}
	reset := "ppu-smi -r -i " + target
	switch code {
	case HealthMemoryRepairPending, HealthMemoryUncorrectable:
		memory.Actions = []string{reset}
		return memory, true
	case HealthMemoryRemapFailed, HealthMemoryErrorHistory:
		return memory, true
	case HealthResetRequired:
		return HealthGuide{Probe: query(PPUHealthXIDFields), Actions: []string{reset}}, true
	case HealthRebootRequired, HealthPowerCycleRequired:
		return HealthGuide{Probe: query(PPUHealthXIDFields)}, true
	}
	return HealthGuide{}, false
}

func nvidiaHealthGuide(code, target string, evidence []HealthField) (HealthGuide, bool) {
	memory := HealthGuide{
		Probe:   "nvidia-smi --query-gpu=pci.bus_id," + strings.Join(probedFields(evidence, NVIDIAHealthMemoryFields), ",") + " --format=csv -i " + target,
		Recheck: "nvidia-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i " + target,
	}
	switch code {
	case HealthMemoryRepairPending, HealthMemoryUncorrectable:
		memory.Actions = []string{"nvidia-smi -r -i " + target}
		return memory, true
	case HealthMemoryRemapFailed, HealthMemoryErrorHistory:
		return memory, true
	case HealthNVLinkDown:
		return HealthGuide{
			Probe:   "nvidia-smi nvlink -s -e -i " + target,
			Recheck: "nvidia-smi topo -m; dmesg | grep -i xid",
		}, true
	case HealthResetRequired, HealthRebootRequired, HealthPowerCycleRequired:
		// The driver's own recovery request; only a reset is a command.
		recovery := HealthGuide{
			Probe:   "nvidia-smi --query-gpu=pci.bus_id," + NVIDIARecoveryField + " --format=csv -i " + target,
			Recheck: "nvidia-smi -q -i " + target + "; dmesg | grep -i xid",
		}
		if code == HealthResetRequired {
			recovery.Actions = []string{"nvidia-smi -r -i " + target}
		}
		return recovery, true
	}
	return HealthGuide{}, false
}

// PCIeLinkField and PCIeAERField name the evidence of PCIe findings: one field
// per link or device, named by its PCI address.
const (
	PCIeLinkField = "PCIe Link "
	PCIeAERField  = "PCIe AER "
)

// pcieHealthGuide shows the link or the error counters of the first device
// the evidence names; lspci prints both ends of the link it is asked about.
func pcieHealthGuide(code string, evidence []HealthField) (HealthGuide, bool) {
	prefix := ""
	switch code {
	case HealthPCIeLinkDegraded:
		prefix = PCIeLinkField
	case HealthPCIeErrors, HealthPCIeFatal:
		prefix = PCIeAERField
	default:
		return HealthGuide{}, false
	}
	for _, f := range evidence {
		bdf, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || bdf == "" {
			continue
		}
		if prefix == PCIeLinkField {
			return HealthGuide{
				Probe:   "cat /sys/bus/pci/devices/" + bdf + "/{current,max}_link_{speed,width}",
				Recheck: "lspci -vv -s " + bdf + " | grep -E 'LnkCap:|LnkSta:'",
			}, true
		}
		return HealthGuide{
			Probe:   "grep -H TOTAL /sys/bus/pci/devices/" + bdf + "/aer_dev_*",
			Recheck: "dmesg | grep -i -E 'AER|pcieport'",
		}, true
	}
	return HealthGuide{}, false
}

// hygonHealthGuide shows the RAS files the agent read and hy-smi's readable
// view of them. hy-smi names no recovery command for these, so there are no
// Actions.
func hygonHealthGuide(code, bdf string) (HealthGuide, bool) {
	if bdf == "" {
		return HealthGuide{}, false
	}
	ras := "/sys/bus/pci/devices/" + NormalizeBDF(bdf) + "/ras/"
	switch code {
	case HealthMemoryUncorrectable, HealthVendorFault:
		return HealthGuide{Probe: "grep . " + ras + "*_err_count", Recheck: "hy-smi --showrasinfo all"}, true
	case HealthMemoryRepairPending, HealthMemoryRemapFailed, HealthMemoryErrorHistory:
		return HealthGuide{Probe: "cat " + ras + "gpu_vram_bad_pages", Recheck: "hy-smi --showpagesinfo"}, true
	}
	return HealthGuide{}, false
}

func ascendHealthGuide(code string, evidence []HealthField) (HealthGuide, bool) {
	if code != HealthVendorWarning && code != HealthVendorFault {
		return HealthGuide{}, false
	}
	guide := HealthGuide{Probe: "npu-smi info"}
	npu, chip := HealthFieldValue(evidence, "NPU"), HealthFieldValue(evidence, "Chip")
	if npu != "" && chip != "" {
		guide.Recheck = "npu-smi info -t health -i " + npu + " -c " + chip
	}
	return guide, true
}

// ToolCommand is the vendor tool's own overview command: what a person runs
// first when the tool itself fails or a card is missing.
func ToolCommand(vendor string) string {
	switch vendor {
	case VendorNVIDIA:
		return "nvidia-smi"
	case VendorKunlunxin:
		return "xpu-smi -q -x"
	case VendorPPU:
		return "ppu-smi"
	case VendorAscend:
		return "npu-smi info"
	case VendorHygon:
		return "hy-smi"
	case VendorAMD:
		return "rocminfo"
	}
	return ""
}
