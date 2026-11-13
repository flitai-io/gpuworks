// Accelerator health: this file is the single vocabulary for it — the three
// levels, the stable finding codes an agent reports, the evidence it records
// for a finding and the vendor commands a person runs to recheck or act on it.
//
// The agent decides a card's level from vendor tool output and records the
// fields it decided on as evidence; the server derives the commands from the
// vendor, the code and the card's PCI address, and the client renders the
// human-readable explanation for the code in its own locale. Nothing here is
// prose meant for a person except the commands themselves.

package accelerator

import "strings"

// Levels. A card at HealthWarning stays schedulable but is used only after the
// cards at HealthOK; a card at HealthError is not scheduled.
const (
	HealthOK      = "OK"
	HealthWarning = "Warning"
	HealthError   = "Error"
)

// Finding codes. A card carries at most one code, the most severe finding;
// the evidence carries every field that contributed.
const (
	// HealthMemoryRepairPending: a memory row or page is marked for replacement
	// and the replacement takes effect at the next reset of the card.
	HealthMemoryRepairPending = "memory_repair_pending"
	// HealthMemoryUncorrectable: uncorrectable memory errors since the driver
	// last loaded or the card was last reset.
	HealthMemoryUncorrectable = "memory_uncorrectable_errors"
	// HealthMemoryRemapFailed: a memory row could not be replaced.
	HealthMemoryRemapFailed = "memory_remap_failed"
	// HealthResetRequired, HealthRebootRequired and HealthPowerCycleRequired: the
	// driver reported an event whose stated recovery is a card reset, an
	// operating system reboot or a cold reboot respectively.
	HealthResetRequired      = "reset_required"
	HealthRebootRequired     = "reboot_required"
	HealthPowerCycleRequired = "power_cycle_required"
	// HealthVendorWarning and HealthVendorFault: the vendor tool's own health
	// verdict, used where the tool states a level rather than raw counters.
	HealthVendorWarning = "vendor_health_warning"
	HealthVendorFault   = "vendor_health_fault"
	// HealthMemoryErrorHistory is informational: lifetime counters show errors
	// that were already handled (pages retired, rows replaced). The card stays
	// at HealthOK.
	HealthMemoryErrorHistory = "memory_error_history"
	// HealthRuntimeUnusable: the card is listed and looks well, but the
	// vendor runtime cannot use it: NVIDIA could not create a CUDA context,
	// or Ascend could not create an ACL context. One card failing to
	// initialize can affect every card of a host (measured on zstone-164:
	// three bad cards, and cuInit returned 101 for all of them), so the
	// finding is recorded per card from trying each one rather than inferred
	// from any other.
	HealthRuntimeUnusable = "runtime_unusable"
	// HealthDriverInitFailed: the card is present and bound to its vendor
	// driver, but the driver could not bring it up, so the vendor CLI does
	// not list it and nothing can use it. The driver's log says why (for
	// NVIDIA: "RmInitAdapter failed", often with "GPU may need to be
	// reset"). Nothing about the card is known beyond its PCI identity.
	HealthDriverInitFailed = "driver_init_failed"
	// HealthCLIUnanswered: the card is present and bound to its vendor
	// driver, but the vendor CLI did not answer at all this round (it
	// timed out, or it is not installed), so nothing is known about the
	// card beyond its PCI identity. The driver may be perfectly well —
	// this says only that we could not ask. The node carries the probe
	// error that explains why.
	HealthCLIUnanswered        = "vendor_cli_unanswered"
	HealthInventoryIncomplete  = "inventory_incomplete"
	HealthInventoryInvalid     = "inventory_identity_invalid"
	HealthTelemetryUnavailable = "telemetry_unavailable"
	// HealthPCIeLinkDegraded: a PCIe link between the card and its root
	// port runs at fewer lanes, or (while the card is busy) a lower speed,
	// than both of its ends support. The card works, slower to load and to
	// move data, so it stays schedulable after the healthy ones.
	HealthPCIeLinkDegraded = "pcie_link_degraded"
	// HealthPCIeErrors: the PCIe devices between the card and its root port
	// reported new non-fatal (recovered) errors.
	HealthPCIeErrors = "pcie_errors"
	// HealthPCIeFatal: they reported new fatal errors, after which the link
	// has to be reset; the card cannot be relied on until it has been, and a
	// runtime check has passed on it since.
	HealthPCIeFatal = "pcie_fatal_errors"
	// HealthNVLinkDown: fewer of the card's NVLinks are active than it has
	// had. The card works; work spread over it and its peers is slower, and
	// the topology the scheduler reads no longer groups it with them.
	HealthNVLinkDown = "nvlink_down"
)

// HealthLevel is the level a code puts a card at. An unknown code is treated as an
// error so that a newer agent's finding is never silently scheduled.
func HealthLevel(code string) string {
	switch code {
	case "", HealthMemoryErrorHistory:
		return HealthOK
	case HealthMemoryRepairPending, HealthVendorWarning, HealthPCIeLinkDegraded, HealthPCIeErrors, HealthNVLinkDown, HealthTelemetryUnavailable, HealthInventoryIncomplete:
		return HealthWarning
	default:
		return HealthError
	}
}

// HealthSchedulable reports whether a card at this level may be scheduled.
func HealthSchedulable(status string) bool { return status != HealthError }

// HealthDegraded reports whether a schedulable card should be used only after the
// cards that are fully healthy.
func HealthDegraded(status string) bool { return status == HealthWarning }

// healthSeverity orders codes so a card reports its most severe finding. A
// code missing from here sorts below every other, so a new one can never
// win — TestEveryHealthCodeHasASeverity holds this.
//
// The order is "which one does an operator most need to read", not a scale
// of badness. So a finding that names a cause outranks one that names a
// symptom of it, and anything that makes a card unusable outranks every
// finding that leaves it schedulable.
var healthSeverity = map[string]int{
	// Leaves the card schedulable.
	HealthInventoryIncomplete:  1,
	HealthMemoryErrorHistory:   2,
	HealthPCIeLinkDegraded:     3,
	HealthPCIeErrors:           4,
	HealthNVLinkDown:           5,
	HealthTelemetryUnavailable: 6,
	HealthVendorWarning:        7,
	HealthMemoryRepairPending:  8,
	// The runtime cannot use the card. It has to outrank every finding
	// above, which would otherwise leave the card looking schedulable, and
	// to yield to every finding below, which names why rather than that.
	HealthRuntimeUnusable: 9,
	// Causes the vendor tool names, each more specific than the symptom.
	HealthPCIeFatal:           10,
	HealthMemoryUncorrectable: 11,
	HealthMemoryRemapFailed:   12,
	HealthVendorFault:         13,
	HealthInventoryInvalid:    14,
	HealthResetRequired:       15,
	HealthRebootRequired:      16,
	HealthPowerCycleRequired:  17,
	// Nothing is known about the card: the tool did not answer, so it has
	// no finding to give. It yields to the one below, which names a cause.
	HealthCLIUnanswered: 18,
	// The vendor tool does not list the card at all, so none of its
	// findings about it can exist.
	HealthDriverInitFailed: 19,
}

// MostSevereHealth returns the most severe of the codes, or "" when there are none.
func MostSevereHealth(codes ...string) string {
	best := ""
	for _, code := range codes {
		if healthSeverity[code] > healthSeverity[best] {
			best = code
		}
	}
	return best
}

// HealthField is one value the agent read from the vendor tool, named exactly as the
// tool names it so a person can find it in the tool's own output.
type HealthField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// FormatHealthEvidence renders evidence as the text stored with the card: one
// "name=value" line per field. HealthField names never contain "=" or a newline;
// values never contain a newline (FormatHealthEvidence folds any into a space).
func FormatHealthEvidence(fields []HealthField) string {
	lines := make([]string, 0, len(fields))
	for _, f := range fields {
		value := strings.Join(strings.Fields(f.Value), " ")
		lines = append(lines, f.Name+"="+value)
	}
	return strings.Join(lines, "\n")
}

// ParseHealthEvidence reads text written by FormatHealthEvidence. Text from an agent that
// predates the evidence format is a free-form sentence (which may itself
// contain "=", e.g. "uncorrected volatile DRAM ECC=1; remapped rows pending");
// it comes back whole as a single field with no name so it can still be shown
// verbatim.
func ParseHealthEvidence(text string) []HealthField {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	fields := make([]HealthField, 0, len(lines))
	for _, line := range lines {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !isHealthFieldName(name) {
			return []HealthField{{Value: text}}
		}
		fields = append(fields, HealthField{Name: name, Value: value})
	}
	return fields
}

// isHealthFieldName accepts the names FormatHealthEvidence writes: a vendor tool's
// query field ("remapped_rows.pending") or one of the labels npu-smi prints.
func isHealthFieldName(name string) bool {
	if name == "" {
		return false
	}
	if !strings.ContainsAny(name, " ;,") {
		return true
	}
	for _, known := range AscendHealthFieldNames {
		if name == known {
			return true
		}
	}
	return false
}

// HealthFieldValue returns the value of the named evidence field.
func HealthFieldValue(fields []HealthField, name string) string {
	for _, f := range fields {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}
