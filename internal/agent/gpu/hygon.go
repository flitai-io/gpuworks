package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

type hygonCommandRunner func(context.Context, ...string) (string, error)

type hygonIdentity struct {
	uuid string
	bdf  string
}

// HygonProber detects Hygon DCUs through hy-smi and resolves per-card device
// nodes through sysfs. It deliberately avoids ROCm/DTK shared libraries so the
// agent remains a CGO-free static binary.
type HygonProber struct {
	command  hygonCommandRunner
	lookPath func(string) (string, error)
	readFile func(string) ([]byte, error)
	glob     func(string) ([]string, error)
	stat     func(string) (os.FileInfo, error)

	mu       sync.RWMutex
	lastSeen map[int]hygonIdentity

	// hostRoot is where the host's /sys is read for the PCI roster; empty
	// means "/". A test sets it so the roster describes its fixture
	// rather than whatever is in the machine running the test.
	hostRoot string
}

func (p *HygonProber) Vendor() string { return accelerator.VendorHygon }

func (p *HygonProber) Available() bool {
	if _, err := p.lookup("hy-smi"); err != nil {
		return false
	}
	out, err := p.run(context.Background(), hygonInventoryArgs()...)
	return err == nil && len(parseHygonInventory(out, nil)) > 0
}

func (p *HygonProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	if _, err := p.lookup("hy-smi"); err != nil {
		return p.roster(nil, false), nil //nolint:nilerr // Absence of this vendor CLI is an unavailable probe, not a node failure.
	}
	out, err := p.run(ctx, hygonInventoryArgs()...)
	if err != nil {
		// hy-smi answers "No device available" on a host whose only DCU is
		// passed through to a virtual machine (measured on 192.0.2.133).
		// The card is there; it is simply not this host's to use.
		return p.roster(nil, false), err
	}
	driver := p.DriverVersion(ctx)
	gpus := parseHygonInventory(out, func(path string) ([]byte, error) {
		return p.read(path)
	})
	for i := range gpus {
		gpus[i].DriverVersion = driver
		code, evidence := hygonHealth(gpus[i].PCIeBDF, p.read)
		SetCardFinding(&gpus[i], code, evidence)
	}

	p.mu.Lock()
	p.lastSeen = make(map[int]hygonIdentity, len(gpus))
	for _, gpu := range gpus {
		p.lastSeen[gpu.Index] = hygonIdentity{uuid: gpu.UUID, bdf: gpu.PCIeBDF}
	}
	p.mu.Unlock()
	return p.roster(gpus, true), nil
}

// roster places what hy-smi listed on the roster of what the host has.
func (p *HygonProber) roster(listed []proto.GPUInfo, answered bool) []proto.GPUInfo {
	return Roster(accelerator.VendorHygon, p.hostRoot, listed, answered)
}

func (p *HygonProber) DriverVersion(ctx context.Context) string {
	for _, path := range []string{"/sys/module/hycu/version", "/sys/module/hydcu/version"} {
		if raw, err := p.read(path); err == nil {
			if version := strings.TrimSpace(string(raw)); version != "" {
				return version
			}
		}
	}
	out, err := p.run(ctx, "--showdriverversion")
	if err != nil {
		return ""
	}
	return parseHygonVersion(out, []string{"driver version"})
}

func (p *HygonProber) Framework(ctx context.Context) (string, string) {
	out, err := p.run(ctx, "--showversion")
	if err != nil {
		return "dtk", ""
	}
	return "dtk", parseHygonVersion(out, []string{"dtk version", "rocm runtime version", "rocm version"})
}

func (p *HygonProber) Topology(ctx context.Context) string {
	if out, err := p.run(ctx, "--showtopo"); err == nil && strings.TrimSpace(out) != "" {
		return out
	}
	out, err := p.run(ctx, hygonInventoryArgs()...)
	if err != nil {
		return ""
	}
	gpus := parseHygonInventory(out, func(path string) ([]byte, error) {
		return p.read(path)
	})
	if len(gpus) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("HCU\tBDF\tNUMA\n")
	for _, gpu := range gpus {
		fmt.Fprintf(&b, "%d\t%s\t%d\n", gpu.Index, gpu.PCIeBDF, gpu.NUMANode)
	}
	return b.String()
}

func (p *HygonProber) Metrics(ctx context.Context) ([]Metric, error) {
	out, err := p.run(ctx, "--showuse", "--showmeminfo", "vram", "--showtemp", "--showpower")
	if err == nil {
		return p.parseMetrics(out, ""), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	// K100_AI hy-smi releases reject the combined query, so utilization,
	// temperature and power would otherwise never be sampled. Memory fields
	// stay on the proven --showmeminfo vram query; the rest come from the
	// bare overview table every hy-smi prints.
	memOut, memErr := p.run(ctx, "--showmeminfo", "vram")
	overview, overviewErr := p.run(ctx)
	if memErr != nil && overviewErr != nil {
		return nil, errors.Join(err, memErr, overviewErr)
	}
	return p.parseMetrics(memOut, overview), nil
}

func (p *HygonProber) parseMetrics(fieldsOut, overview string) []Metric {
	records := parseHygonFields(fieldsOut)
	overviewRows := parseHygonOverview(overview)
	indexes := sortedHygonIndexes(records)
	for index := range overviewRows {
		if _, ok := records[index]; !ok {
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)
	metrics := make([]Metric, 0, len(indexes))
	for _, index := range indexes {
		fields := records[index]
		bdf := hygonPCIBDF(fields["pci bus"])
		uuid := p.hygonUUID(index, bdf, fields)
		p.mu.RLock()
		if identity, ok := p.lastSeen[index]; ok && identity.uuid != "" {
			uuid = identity.uuid
		}
		p.mu.RUnlock()
		m := Metric{Index: index, UUID: uuid, Vendor: accelerator.VendorHygon, Values: map[string]float64{}}
		for key, value := range fields {
			switch {
			case strings.Contains(key, "vram total used memory"):
				addHygonMetric(m.Values, "gpuworks_gpu_memory_used_bytes", value, 1024*1024)
			case strings.Contains(key, "vram total memory"):
				addHygonMetric(m.Values, "gpuworks_gpu_memory_total_bytes", value, 1024*1024)
			case strings.Contains(key, "gpu use") || strings.Contains(key, "hcu use") || strings.Contains(key, "gpu utilization"):
				addHygonMetric(m.Values, "gpuworks_gpu_utilization", value, 1)
			case strings.Contains(key, "temperature"):
				addHygonMetric(m.Values, "gpuworks_gpu_temperature_celsius", value, 1)
			case strings.Contains(key, "average graphics package power") || key == "power":
				addHygonMetric(m.Values, "gpuworks_gpu_power_watts", value, 1)
			}
		}
		// The detailed query's fields win; the overview only fills what the
		// meminfo-only fallback cannot answer. Cells hy-smi reported as N/A
		// never reach this map, so unsupported telemetry stays unknown
		// instead of reading as zero.
		for name, value := range overviewRows[index] {
			if _, ok := m.Values[name]; !ok {
				m.Values[name] = value
			}
		}
		if len(m.Values) > 0 {
			metrics = append(metrics, m)
		}
	}
	return metrics
}

func (p *HygonProber) drmNodes(bdf string) ([]string, error) {
	pattern := filepath.Join("/sys/bus/pci/devices", bdf, "drm", "*")
	paths, err := p.match(pattern)
	if err != nil {
		return nil, fmt.Errorf("cannot enumerate DRM nodes for Hygon PCI %s: %w", bdf, err)
	}
	var cards, renders []string
	for _, sysfsPath := range paths {
		name := filepath.Base(sysfsPath)
		var dst *[]string
		switch {
		case strings.HasPrefix(name, "card"):
			dst = &cards
		case strings.HasPrefix(name, "renderD"):
			dst = &renders
		default:
			continue
		}
		path := filepath.Join("/dev/dri", name)
		if err := p.requireDevice(path); err != nil {
			return nil, fmt.Errorf("Hygon PCI %s maps to unavailable device %s: %w", bdf, path, err)
		}
		*dst = append(*dst, path)
	}
	sort.Strings(cards)
	sort.Strings(renders)
	if len(cards) == 0 || len(renders) == 0 {
		return nil, fmt.Errorf("Hygon PCI %s requires both card and render DRM nodes", bdf)
	}
	return append(cards, renders...), nil
}

func (p *HygonProber) hygonUUID(index int, bdf string, fields map[string]string) string {
	if value := strings.TrimSpace(fields["unique id"]); value != "" {
		return normalizeHygonUUID(value)
	}
	if bdf != "" {
		if raw, err := p.read(filepath.Join("/sys/bus/pci/devices", bdf, "unique_id")); err == nil {
			if value := strings.TrimSpace(string(raw)); value != "" {
				return normalizeHygonUUID(value)
			}
		}
		return "hygon-" + bdf
	}
	return "hygon-" + strconv.Itoa(index)
}

func parseHygonInventory(out string, readFile func(string) ([]byte, error)) []proto.GPUInfo {
	records := parseHygonFields(out)
	indexes := sortedHygonIndexes(records)
	gpus := make([]proto.GPUInfo, 0, len(indexes))
	for _, index := range indexes {
		fields := records[index]
		name := strings.TrimSpace(fields["card series"])
		bdf := hygonPCIBDF(fields["pci bus"])
		memMiB := parseFirstHygonFloat(fields["vram total memory (mib)"])
		if name == "" && bdf == "" && memMiB == 0 {
			continue
		}
		canonical := normalizeHygonSKU(name)
		uuid := "hygon-" + strconv.Itoa(index)
		numa := -1
		if bdf != "" {
			uuid = "hygon-" + bdf
			if readFile != nil {
				if raw, err := readFile(filepath.Join("/sys/bus/pci/devices", bdf, "unique_id")); err == nil {
					if value := strings.TrimSpace(string(raw)); value != "" {
						uuid = normalizeHygonUUID(value)
					}
				}
				if raw, err := readFile(filepath.Join("/sys/bus/pci/devices", bdf, "numa_node")); err == nil {
					if parsed, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil {
						numa = parsed
					}
				}
			}
		}
		displayName := name
		if displayName == "" {
			displayName = "Hygon DCU"
		}
		// ArchID is the most specific identity the card gives: its SKU, or
		// the series when it names only the series (a bare "K100" is one of
		// several K100 SKUs). A card that names no known SKU has none.
		archID := canonical
		switch canonical {
		case "UNKNOWN_HYGON_SKU":
			archID = ""
		case "K100_FAMILY_AMBIGUOUS":
			archID = "K100"
		}
		gpus = append(gpus, proto.GPUInfo{
			Index:            index,
			UUID:             uuid,
			Name:             displayName,
			MemoryTotalBytes: int64(memMiB * 1024 * 1024),
			Vendor:           accelerator.VendorHygon,
			ArchID:           archID,
			PCIeBDF:          bdf,
			NUMANode:         numa,
			Healthy:          true,
			HealthStatus:     "OK",
		})
	}
	return gpus
}

var hygonFieldRE = regexp.MustCompile(`(?i)^(?:HCU|DCU)\[([0-9]+)]\s*:\s*([^:]+):\s*(.*?)\s*$`)
var hygonNumberRE = regexp.MustCompile(`[-+]?[0-9]+(?:\.[0-9]+)?`)

// Newer hy-smi releases append "--> SN: ..." after the PCI address.
func hygonPCIBDF(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ""
	}
	return accelerator.NormalizeBDF(fields[0])
}

func parseHygonFields(out string) map[int]map[string]string {
	records := make(map[int]map[string]string)
	for _, line := range strings.Split(out, "\n") {
		match := hygonFieldRE.FindStringSubmatch(strings.TrimSpace(line))
		if len(match) != 4 {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if records[index] == nil {
			records[index] = make(map[string]string)
		}
		key := strings.ToLower(strings.Join(strings.Fields(match[2]), " "))
		records[index][key] = strings.TrimSpace(match[3])
	}
	return records
}

func normalizeHygonSKU(raw string) string {
	value := strings.ToUpper(strings.TrimSpace(raw))
	compact := strings.NewReplacer("_", "", "-", "", " ", "").Replace(value)
	switch compact {
	case "BW1100":
		return "BW1100"
	case "BW1000":
		return "BW1000"
	case "K100AI":
		return "K100AI"
	case "Z100":
		return "Z100"
	case "BW":
		return "BW"
	case "BW1101":
		return "BW1101"
	case "BW150":
		return "BW150"
	case "K100":
		return "K100_FAMILY_AMBIGUOUS"
	default:
		return "UNKNOWN_HYGON_SKU"
	}
}

func normalizeHygonUUID(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(strings.TrimPrefix(value, "0x"), "0X")
	if strings.HasPrefix(strings.ToUpper(value), "GPU-") {
		return "GPU-" + strings.TrimPrefix(strings.TrimPrefix(value, "GPU-"), "gpu-")
	}
	return "GPU-" + value
}

func parseHygonVersion(out string, labels []string) string {
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(strings.Trim(parts[0], "|")))
		for _, label := range labels {
			if strings.Contains(key, label) {
				return strings.TrimSpace(strings.Trim(parts[1], "|"))
			}
		}
	}
	return ""
}

func addHygonMetric(dst map[string]float64, name, raw string, scale float64) {
	if value := parseFirstHygonFloat(raw); value != 0 || hygonNumberRE.MatchString(raw) {
		dst[name] = value * scale
	}
}

func parseFirstHygonFloat(raw string) float64 {
	value := hygonNumberRE.FindString(raw)
	parsed, _ := strconv.ParseFloat(value, 64)
	return parsed
}

func sortedHygonIndexes(records map[int]map[string]string) []int {
	indexes := make([]int, 0, len(records))
	for index := range records {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}

func hygonInventoryArgs() []string {
	return []string{"--showbus", "--showproductname", "--showmeminfo", "vram"}
}

func (p *HygonProber) run(ctx context.Context, args ...string) (string, error) {
	if p.command != nil {
		return p.command(ctx, args...)
	}
	// Keep the normal CLI name for shared query health tracking. Some DTK
	// installations expose hy-smi only under their installation directory.
	command := "hy-smi"
	if _, err := lookCLI(command); err != nil {
		var lookupErr error
		command, lookupErr = p.lookup(command)
		if lookupErr != nil {
			return "", lookupErr
		}
	}
	return queryLogged(ctx, accelerator.VendorHygon, command, args...)
}

func (p *HygonProber) lookup(name string) (string, error) {
	if p.lookPath != nil {
		return p.lookPath(name)
	}
	command, err := lookCLI(name)
	if err == nil || name != "hy-smi" {
		return command, err
	}
	for _, candidate := range []string{"/opt/dtk/bin/hy-smi", "/opt/hyhal/bin/hy-smi"} {
		if command, err := lookCLI(candidate); err == nil {
			return command, nil
		}
	}
	return "", err
}

func (p *HygonProber) read(path string) ([]byte, error) {
	if p.readFile != nil {
		return p.readFile(path)
	}
	return os.ReadFile(path)
}

func (p *HygonProber) match(pattern string) ([]string, error) {
	if p.glob != nil {
		return p.glob(pattern)
	}
	return filepath.Glob(pattern)
}

// inspect stats path where CLIs run (statTool): a privileged container's
// own /dev and /opt/hyhal are copies made at container start, so a device
// node or driver directory that appears afterward is found only on the
// host's live file system.
func (p *HygonProber) inspect(path string) (os.FileInfo, error) {
	if p.stat != nil {
		return p.stat(path)
	}
	return statTool(path)
}

func (p *HygonProber) requireDevice(path string) error {
	info, err := p.inspect(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeDevice == 0 {
		return fmt.Errorf("not a device node")
	}
	return nil
}
