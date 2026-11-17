// Pure NVIDIA text parsers: turning nvidia-smi CSV/dmon/listing output into
// Metric/proto.GPUInfo values. Nothing here shells out or touches a clock —
// see nvidia.go for the SMIProber that calls these against real command
// output (or a fake runner in tests).
package gpu

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func parseNVIDIAMetrics(out string) []Metric {
	var metrics []Metric
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := splitCSV(line)
		if len(f) < 7 {
			continue
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		m := Metric{Index: idx, UUID: f[1], Vendor: accelerator.VendorNVIDIA, Values: map[string]float64{}}
		add := func(metric string, field int, scale float64) {
			if v, err := strconv.ParseFloat(f[field], 64); err == nil {
				m.Values[metric] = v * scale
			}
		}
		add("gpuworks_gpu_utilization", 2, 1)
		add("gpuworks_gpu_memory_used_bytes", 3, 1024*1024)
		add("gpuworks_gpu_memory_total_bytes", 4, 1024*1024)
		add("gpuworks_gpu_temperature_celsius", 5, 1)
		add("gpuworks_gpu_power_watts", 6, 1)
		if len(f) > 7 {
			add("gpuworks_gpu_fan_speed_percent", 7, 1)
		}
		// The four plan §4.1 fields are always queried together (whichever of
		// metricsQueryEventReasons/metricsQueryThrottleReasons won) and always
		// fall back together, so a short row simply has none of them rather
		// than some. Field 11 holds either spelling's value; both are the same
		// hex bitmask shape, so parsing does not need to know which tier won.
		if len(f) > 11 {
			add("gpuworks_gpu_memory_bandwidth_utilization", 8, 1)
			add("gpuworks_gpu_sm_clock_mhz", 9, 1)
			add("gpuworks_gpu_sm_clock_max_mhz", 10, 1)
			if reasons, ok := parseThrottleReasonsHex(f[11]); ok {
				m.Values["gpuworks_gpu_throttle_reasons"] = reasons
			}
		}
		if len(m.Values) > 0 {
			metrics = append(metrics, m)
		}
	}
	return metrics
}

// parseThrottleReasonsHex parses nvidia-smi's clocks_event_reasons.active (or
// the deprecated clocks_throttle_reasons.active) field, a hex bitmask like
// "0x0000000000000004" (nvmlClocksEventReasons, formerly
// nvmlClocksThrottleReasons). The agent forwards the raw value; bit
// interpretation (HW_SLOWDOWN, SW_THERMAL, ...) lives server-side. A GPU that
// does not support the field reports "[Not Supported]" or "[N/A]", which is
// not hex and correctly yields ok=false rather than a bogus zero.
func parseThrottleReasonsHex(field string) (float64, bool) {
	field = strings.TrimSpace(field)
	hex, ok := strings.CutPrefix(strings.ToLower(field), "0x")
	if !ok || hex == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(hex, 16, 64)
	if err != nil {
		return 0, false
	}
	return float64(v), true
}

type pcieThroughput struct {
	RXBytesPerSec float64
	TXBytesPerSec float64
}

// parseNVIDIADmon parses a single `nvidia-smi dmon -c 1 -s t` sample. NVIDIA
// reports rxpci/txpci in MiB/s; unsupported values are rendered as dashes.
func parseNVIDIADmon(out string) map[int]pcieThroughput {
	result := map[int]pcieThroughput{}
	rxColumn, txColumn := -1, -1
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			header := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "#")))
			for i, field := range header {
				switch strings.ToLower(field) {
				case "rxpci":
					rxColumn = i
				case "txpci":
					txColumn = i
				}
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		idx, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		if rxColumn < 0 {
			// Minimal fixtures may provide only `gpu rxpci txpci` rows.
			rxColumn, txColumn = 1, 2
		}
		if rxColumn >= len(fields) {
			rxColumn = -1
		}
		if txColumn >= len(fields) {
			txColumn = -1
		}
		var rx, tx float64
		var rxErr, txErr error
		if rxColumn >= 0 {
			rx, rxErr = strconv.ParseFloat(fields[rxColumn], 64)
		} else {
			rxErr = strconv.ErrSyntax
		}
		if txColumn >= 0 {
			tx, txErr = strconv.ParseFloat(fields[txColumn], 64)
		} else {
			txErr = strconv.ErrSyntax
		}
		if rxErr != nil && txErr != nil {
			continue
		}
		var sample pcieThroughput
		if rxErr == nil {
			sample.RXBytesPerSec = rx * 1024 * 1024
		}
		if txErr == nil {
			sample.TXBytesPerSec = tx * 1024 * 1024
		}
		result[idx] = sample
	}
	return result
}

func mergeNVIDIAPCIeMetrics(metrics []Metric, samples map[int]pcieThroughput) {
	for i := range metrics {
		sample, ok := samples[metrics[i].Index]
		if !ok {
			continue
		}
		metrics[i].Values["gpuworks_gpu_pcie_rx_bytes_per_sec"] = sample.RXBytesPerSec
		metrics[i].Values["gpuworks_gpu_pcie_tx_bytes_per_sec"] = sample.TXBytesPerSec
	}
}

var (
	nvidiaGPUListingLinePattern = regexp.MustCompile(`^GPU\s+(\d+):`)
	nvidiaMIGListingLinePattern = regexp.MustCompile(`^\s*MIG\s+(.+?)\s+Device\s+\d+:\s+\(UUID:\s*MIG-[^)]+\)\s*$`)
	nvidiaMIGMemoryPattern      = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)gb\b`)
)

// enrichSingleVisibleNVIDIAMIG handles provider containers that expose one MIG
// slice while nvidia-smi --query-gpu still reports the physical parent and an
// unavailable memory.total value. A single visible slice is one schedulable
// device, so its profile memory is authoritative. Multiple visible slices need
// a first-class per-MIG inventory model and are deliberately left untouched.
func enrichSingleVisibleNVIDIAMIG(gpus []proto.GPUInfo, listing string) {
	profiles := make(map[int][]string)
	physicalIndex := -1
	for _, line := range strings.Split(listing, "\n") {
		if match := nvidiaGPUListingLinePattern.FindStringSubmatch(strings.TrimSpace(line)); len(match) == 2 {
			physicalIndex, _ = strconv.Atoi(match[1])
			continue
		}
		if physicalIndex < 0 {
			continue
		}
		if match := nvidiaMIGListingLinePattern.FindStringSubmatch(line); len(match) == 2 {
			profiles[physicalIndex] = append(profiles[physicalIndex], strings.TrimSpace(match[1]))
		}
	}
	for i := range gpus {
		visible := profiles[gpus[i].Index]
		if len(visible) != 1 {
			continue
		}
		match := nvidiaMIGMemoryPattern.FindStringSubmatch(visible[0])
		if len(match) != 2 {
			continue
		}
		memoryGiB, err := strconv.ParseFloat(match[1], 64)
		if err != nil || memoryGiB <= 0 {
			continue
		}
		gpus[i].MemoryTotalBytes = int64(memoryGiB * (1 << 30))
		gpus[i].MIGEnabled = true
	}
}

func parseNVIDIAInfo(out string) []proto.GPUInfo {
	var gpus []proto.GPUInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := splitCSV(line)
		if len(f) < 7 {
			continue
		}
		idx, _ := strconv.Atoi(f[0])
		memMiB, _ := strconv.ParseInt(f[3], 10, 64)
		cc := f[4]
		g := proto.GPUInfo{
			Index:             idx,
			UUID:              f[1],
			Name:              f[2],
			MemoryTotalBytes:  memMiB * 1024 * 1024,
			ComputeCapability: cc,
			Vendor:            accelerator.VendorNVIDIA,
			ArchID:            nvidiaArchID(cc),
			PCIeBDF:           f[5],
			NUMANode:          numaForBDF(f[5]),
			DriverVersion:     f[6],
			Healthy:           true,
			HealthStatus:      "OK",
		}
		if len(f) > 7 {
			g.PowerLimitWatts, _ = strconv.ParseFloat(f[7], 64)
		}
		if len(f) > 8 {
			g.Healthy, g.HealthStatus, g.HealthCode, g.HealthMessage = nvidiaHealth(f[8:])
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// nvidiaHealth decides a card's level from the health fields that follow
// power.limit, in the order of accelerator.NVIDIAHealthMemoryFields (an older driver
// answers only the first two). Volatile uncorrectable errors and a failed row
// replacement stop scheduling; a replacement waiting for a reset keeps the
// card schedulable at a lower preference; the aggregate counter is lifetime
// history a reset never clears, so it leaves the card healthy.
func nvidiaHealth(values []string) (bool, string, string, string) {
	value := func(i int) string {
		if i < len(values) {
			return values[i]
		}
		return ""
	}
	count := func(i int) uint64 {
		n, _ := strconv.ParseUint(value(i), 10, 64)
		return n
	}
	yes := func(i int) bool { return strings.EqualFold(value(i), "yes") }
	var codes []string
	var evidence []accelerator.HealthField
	for i, check := range []struct {
		code string
		hit  func(int) bool
	}{
		{accelerator.HealthMemoryUncorrectable, func(i int) bool { return count(i) > 0 }},
		{accelerator.HealthMemoryErrorHistory, func(i int) bool { return count(i) > 0 }},
		{accelerator.HealthMemoryRepairPending, yes},
		{accelerator.HealthMemoryRepairPending, yes},
		{accelerator.HealthMemoryRemapFailed, yes},
	} {
		if check.hit(i) {
			// Only fields that contributed are evidence; the full state is
			// one recheck command away.
			codes = append(codes, check.code)
			evidence = append(evidence, accelerator.HealthField{Name: accelerator.NVIDIAHealthMemoryFields[i], Value: value(i)})
		}
	}
	// The recovery the driver asks for follows the memory fields.
	if recovery := nvidiaRecoveryCode(value(len(accelerator.NVIDIAHealthMemoryFields))); recovery != "" {
		codes = append(codes, recovery)
		evidence = append(evidence, accelerator.HealthField{Name: accelerator.NVIDIARecoveryField, Value: value(len(accelerator.NVIDIAHealthMemoryFields))})
	}
	code := accelerator.MostSevereHealth(codes...)
	if code == "" {
		return true, accelerator.HealthOK, "", ""
	}
	status := accelerator.HealthLevel(code)
	return accelerator.HealthSchedulable(status), status, code, accelerator.FormatHealthEvidence(evidence)
}

// nvidiaRecoveryCode maps the driver's gpu_recovery_action to a finding:
// "None" and "[N/A]" (or an empty field) are none; a reboot is a reboot; any
// form of reset ("Reset", "Drain and Reset", "Drain P2P" which is followed by
// one) is a reset. A value this agent does not know is still the driver
// asking for something, so it reads as a reset rather than nothing.
func nvidiaRecoveryCode(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	switch {
	case v == "", v == "none", strings.HasPrefix(v, "[n/a"), v == "n/a", strings.HasPrefix(v, "[unknown"):
		return ""
	case strings.Contains(v, "reboot"):
		return accelerator.HealthRebootRequired
	default:
		return accelerator.HealthResetRequired
	}
}

func splitCSV(line string) []string {
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// applyUnifiedMemoryFallback fills MemoryTotalBytes for unified-memory parts
// from host RAM: the GPU addresses the same coherent pool the CPU does, so
// MemTotal is the schedulable ceiling. nvidia-smi answers [N/A] for their
// memory.total, so parseNVIDIAInfo records 0 and this fills it in. A discrete
// GPU whose memory.total merely failed to parse keeps its honest 0 — only the
// models accelerator.UnifiedMemory names get a value, and that list is shared
// with the server, which sizes the host's share of such a card from it.
// hostBytes is the host's memory size, 0 when unknown.
func applyUnifiedMemoryFallback(gpus []proto.GPUInfo, hostBytes func() int64) {
	total := hostBytes()
	if total <= 0 {
		return
	}
	for i := range gpus {
		if gpus[i].MemoryTotalBytes == 0 && accelerator.UnifiedMemory(gpus[i].Name) {
			gpus[i].MemoryTotalBytes = total
		}
	}
}

// hostMemoryBytes reports MemTotal from proc/meminfo under hostRoot (empty
// means "/"); 0 when unavailable (non-Linux hosts have no fallback and keep
// the parsed 0).
func hostMemoryBytes(hostRoot string) int64 {
	data, err := os.ReadFile(filepath.Join("/", hostRoot, "proc/meminfo"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb <= 0 {
			return 0
		}
		return kb * 1024
	}
	return 0
}

func nvidiaArchID(cc string) string {
	cc = strings.TrimSpace(cc)
	if cc == "" {
		return ""
	}
	parts := strings.SplitN(cc, ".", 2)
	major := strings.TrimSpace(parts[0])
	minor := "0"
	if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		minor = strings.TrimSpace(parts[1])
	}
	if major == "" {
		return ""
	}
	return "sm_" + major + minor
}

// numaForBDF reads the NUMA node for a PCI device from sysfs; -1 if unknown.
func numaForBDF(bdf string) int {
	// nvidia-smi prints e.g. 00000000:65:00.0 — sysfs wants 0000:65:00.0.
	bdf = accelerator.NormalizeBDF(bdf)
	if bdf == "" {
		return -1
	}
	return readIntFile("/sys/bus/pci/devices/" + bdf + "/numa_node")
}
