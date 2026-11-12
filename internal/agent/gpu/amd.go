package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// AMDProber reads the amdgpu kernel ABI without requiring a ROCm installation
// or cgo. PCI vendor verification prevents ROCm-compatible Hygon devices from
// being reported as AMD. Index is the DRM card number, never a HIP ordinal.
// See https://www.kernel.org/doc/html/latest/gpu/amdgpu/thermal.html.
type AMDProber struct {
	mu              sync.RWMutex
	collectRead     sysfsReadGate
	metricsRead     sysfsReadGate
	injectRead      sysfsReadGate
	telemetry       *amdMetricInventory
	clockNow        func() time.Time
	lastSeen        map[int]proto.GPUInfo
	lastSeenDevices map[int]string
	lastSeenMem     map[int]amdMemoryFacts
	readFile        func(string) ([]byte, error)
	glob            func(string) ([]string, error)
	stat            func(string) (os.FileInfo, error)
	hostRoot        string
	hostRuntime     func() accelerator.HostRuntime
}

func (p *AMDProber) Vendor() string { return accelerator.VendorAMD }
func (p *AMDProber) Available() bool {
	// A display-only AMD PCI function is not a usable compute backend.
	// Explicit vendor selection still reports its inventory and failure.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if p.boundedKFDDeviceError(ctx) != nil {
		return false
	}
	gs, err := p.inventory(ctx)
	if err != nil {
		return false
	}
	for _, g := range gs {
		if g.Healthy {
			return true
		}
	}
	return false
}

func (p *AMDProber) runtimeFacts() accelerator.HostRuntime {
	if p.hostRuntime != nil {
		return p.hostRuntime()
	}
	return accelerator.HostRuntime{OS: goruntime.GOOS, Arch: goruntime.GOARCH}
}

// hostPath roots real OS access in a fixture tree when requested. Injected
// functions continue to receive stable host paths so tests can model exact
// sysfs failures without translating every path.
func (p *AMDProber) hostPath(path string) string {
	if p.hostRoot == "" {
		return path
	}
	root := filepath.Clean(p.hostRoot)
	clean := filepath.Clean(path)
	if rel, err := filepath.Rel(root, clean); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return clean
	}
	return filepath.Join(root, strings.TrimLeft(clean, string(filepath.Separator)))
}
func (p *AMDProber) read(path string) ([]byte, error) {
	if p.readFile != nil {
		return p.readFile(path)
	}
	return os.ReadFile(p.hostPath(path))
}
func (p *AMDProber) match(path string) ([]string, error) {
	if p.glob != nil {
		return p.glob(path)
	}
	return filepath.Glob(p.hostPath(path))
}
func (p *AMDProber) inspect(path string) (os.FileInfo, error) {
	if p.stat != nil {
		return p.stat(path)
	}
	if p.hostRoot == "" {
		// A privileged pod's /dev is a copy made at container start;
		// lstatTool checks the host's live /dev when CLIs run on the host.
		return lstatTool(path)
	}
	return os.Lstat(p.hostPath(path))
}

// matchDev globs /dev, where a privileged pod's own copy of the host's
// device nodes is stale: it goes through the host when CLIs run there.
func (p *AMDProber) matchDev(path string) ([]string, error) {
	if p.glob != nil || p.hostRoot != "" {
		return p.match(path)
	}
	return globTool(path), nil
}

// readHostFile reads a host file that is not in sysfs (a ROCm install's
// version marker): through the host's root when CLIs run on the host.
func (p *AMDProber) readHostFile(path string) ([]byte, error) {
	if p.readFile != nil || p.hostRoot != "" {
		return p.read(path)
	}
	return readTool(path)
}
func (p *AMDProber) text(path string) string {
	b, _ := p.read(path)
	return strings.TrimSpace(string(b))
}
func (p *AMDProber) number(path string) (int64, error) {
	b, e := p.read(path)
	if e != nil {
		return 0, e
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// Probe waits for at most one sysfs collection. Kernel sysfs reads have no
// cancellation API, so the caller can leave on cancellation while the one
// in-flight read finishes; later calls cannot spawn unbounded readers.
func (p *AMDProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	return boundedSysfsRead(ctx, &p.collectRead, func() ([]proto.GPUInfo, error) { return p.collect(ctx) })
}

func (p *AMDProber) collect(ctx context.Context) ([]proto.GPUInfo, error) {
	gs, err := p.inventoryRaw(ctx)
	p.markKFDUnavailable(gs, p.kfdDeviceError())
	gs = Roster(accelerator.VendorAMD, p.hostRoot, gs, err == nil)
	conflicts := markAMDDuplicateIdentities(gs)
	// Sysfs access can block; never hold the cache lock while reading it.
	seen := make(map[int]proto.GPUInfo, len(gs))
	memory := make(map[int]amdMemoryFacts, len(gs))
	for _, g := range gs {
		if ctx.Err() != nil {
			return gs, ctx.Err()
		}
		if g.Index >= 0 && g.PCIeBDF != "" && g.UUID != "" && !conflicts[g.Index] && g.Healthy {
			seen[g.Index] = g
			path := fmt.Sprintf("/sys/class/drm/card%d/device", g.Index)
			vram, readErr := p.number(filepath.Join(path, "mem_info_vram_total"))
			if readErr == nil {
				memory[g.Index] = p.memoryFacts(path, g.PCIeBDF, g.ArchID, vram)
			}
		}
	}
	snapshot := p.metricInventoryFrom(gs)
	p.mu.Lock()
	p.telemetry = snapshot
	p.lastSeen = seen
	p.lastSeenDevices = snapshot.deviceIDs
	p.lastSeenMem = memory
	p.mu.Unlock()
	return gs, err
}

// Only published inventory advances the identity baseline. Runtime checks and
// telemetry must not accept a changed card merely by observing it once.
func (p *AMDProber) inventory(ctx context.Context) ([]proto.GPUInfo, error) {
	return boundedSysfsRead(ctx, &p.collectRead, func() ([]proto.GPUInfo, error) { return p.inventoryRaw(ctx) })
}

func (p *AMDProber) inventoryRaw(ctx context.Context) ([]proto.GPUInfo, error) {
	paths, err := p.match("/sys/class/drm/card[0-9]*/device")
	if err != nil {
		return nil, err
	}
	var gs []proto.GPUInfo
	var firstDeviceErr error
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return gs, err
		}
		index, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(filepath.Dir(path)), "card"))
		if err != nil {
			continue
		}
		vendor, err := p.read(filepath.Join(path, "vendor"))
		if err != nil {
			// A card whose vendor attribute cannot be read is recovered from
			// the independent PCI roster; it must not erase readable siblings.
			if firstDeviceErr == nil {
				firstDeviceErr = err
			}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(string(vendor)), accelerator.AMDPCIVendorHex) {
			continue
		}
		bdf := p.deviceBDF(path)
		name := p.text(filepath.Join(path, "product_name"))
		archID := p.archID(path)
		if name == "" {
			name = amdInstinctName(p.text(filepath.Join(path, "device")))
		}
		if name == "" {
			name = "AMD GPU " + p.text(filepath.Join(path, "device"))
			if archID != "" {
				name += " (" + archID + ")"
			}
		}
		uuid := ""
		if bdf != "" {
			uuid = "amd-" + bdf
			if id := normalizeAMDUniqueID(p.text(filepath.Join(path, "unique_id"))); id != "" {
				// The physical ASIC serial is stable across DRM renumbering and PCI slots.
				uuid = "amd-" + id
			}
		}
		numa := -1
		if n, e := p.number(filepath.Join(path, "numa_node")); e == nil {
			numa = int(n)
		}
		gpu := proto.GPUInfo{Index: index, UUID: uuid, Name: name, Vendor: accelerator.VendorAMD, ArchID: archID, PCIeBDF: bdf, NUMANode: numa, DriverVersion: p.DriverVersion(ctx)}
		var findings []accelerator.HealthField
		var capacityFacts *amdMemoryFacts
		if bdf == "" {
			findings = append(findings, accelerator.HealthField{Name: "PCI Address", Value: "unavailable"})
		}
		if archID == "" {
			findings = append(findings, accelerator.HealthField{Name: "KFD compute identity", Value: "PCI/render/architecture mapping unavailable or ambiguous"})
		}
		if renderErr := p.renderDeviceError(path); renderErr != nil {
			findings = append(findings, accelerator.HealthField{Name: "DRM render device", Value: renderErr.Error()})
		}
		total, memErr := p.number(filepath.Join(path, "mem_info_vram_total"))
		if memErr != nil || total <= 0 {
			findings = append(findings, accelerator.HealthField{Name: "VRAM inventory", Value: "unavailable"})
			if memErr != nil && firstDeviceErr == nil {
				firstDeviceErr = memErr
			}
		} else {
			facts := p.memoryFacts(path, bdf, archID, total)
			capacityFacts = &facts
			gpu.MemoryTotalBytes = facts.totalBytes
			if p.text(filepath.Join(path, "device")) == accelerator.AMDStrixHaloDeviceHex && archID == accelerator.AMDStrixHaloArch && facts.source != amdMemoryUMAPool {
				gpu.MemoryTotalBytes = 0
				findings = append(findings, accelerator.HealthField{Name: "UMA capacity", Value: "KFD/GTT public pool unverified: " + facts.diagnostic})
			}
		}
		if len(findings) != 0 {
			if capacityFacts != nil {
				findings = append(findings, amdMemoryHealthFields(*capacityFacts)...)
			}
			SetCardFinding(&gpu, accelerator.HealthDriverInitFailed, findings)
		} else {
			gpu.Healthy = true
			gpu.HealthStatus = accelerator.HealthOK
		}
		gs = append(gs, gpu)
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].Index < gs[j].Index })
	markAMDDuplicateIdentities(gs)
	if len(gs) == 0 && firstDeviceErr != nil {
		return gs, firstDeviceErr
	}
	return gs, nil
}

func (p *AMDProber) deviceBDF(devicePath string) string {
	for _, line := range strings.Split(p.text(filepath.Join(devicePath, "uevent")), "\n") {
		if strings.HasPrefix(line, "PCI_SLOT_NAME=") {
			return accelerator.NormalizeBDF(strings.TrimPrefix(line, "PCI_SLOT_NAME="))
		}
	}
	return ""
}

// markAMDDuplicateIdentities degrades every row that shares a PCI address,
// published UUID, or DRM index. Keeping the first would make array order an
// identity policy and could authorize injection against an ambiguous card.
func markAMDDuplicateIdentities(gpus []proto.GPUInfo) map[int]bool {
	indexes, bdfs, uuids := map[int]int{}, map[string]int{}, map[string]int{}
	for _, gpu := range gpus {
		if gpu.Index >= 0 {
			indexes[gpu.Index]++
		}
		if gpu.PCIeBDF != "" {
			bdfs[gpu.PCIeBDF]++
		}
		if gpu.UUID != "" {
			uuids[gpu.UUID]++
		}
	}
	conflicts := map[int]bool{}
	for i := range gpus {
		gpu := &gpus[i]
		var fields []accelerator.HealthField
		if gpu.Index >= 0 && indexes[gpu.Index] > 1 {
			fields = append(fields, accelerator.HealthField{Name: "DRM index", Value: strconv.Itoa(gpu.Index)})
		}
		if gpu.PCIeBDF != "" && bdfs[gpu.PCIeBDF] > 1 {
			fields = append(fields, accelerator.HealthField{Name: "PCI Address", Value: gpu.PCIeBDF})
		}
		if gpu.UUID != "" && uuids[gpu.UUID] > 1 {
			fields = append(fields, accelerator.HealthField{Name: "GPU UUID", Value: gpu.UUID})
		}
		if len(fields) > 0 {
			SetCardFinding(gpu, accelerator.HealthRuntimeUnusable, fields)
			conflicts[gpu.Index] = true
		}
	}
	return conflicts
}

func (p *AMDProber) markKFDUnavailable(gpus []proto.GPUInfo, reason error) {
	if reason == nil {
		return
	}
	for i := range gpus {
		if !gpus[i].Healthy {
			continue // Preserve the more specific inventory finding.
		}
		evidence := []accelerator.HealthField{
			{Name: "KFD device", Value: reason.Error()},
		}
		SetCardFinding(&gpus[i], accelerator.HealthRuntimeUnusable, evidence)
	}
}

func amdMemoryHealthFields(facts amdMemoryFacts) []accelerator.HealthField {
	fields := []accelerator.HealthField{{Name: "Memory Capacity Source", Value: string(facts.source)}}
	if facts.diagnostic != "" {
		fields = append(fields, accelerator.HealthField{Name: "Memory Capacity Evidence", Value: facts.diagnostic})
	}
	return fields
}

func normalizeAMDUniqueID(id string) string {
	id = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), "0x")
	if id == "" {
		return ""
	}
	for _, digit := range id {
		if !(digit >= '0' && digit <= '9') && !(digit >= 'a' && digit <= 'f') {
			return ""
		}
	}
	value, err := strconv.ParseUint(id, 16, 64)
	if err != nil || value == 0 {
		return ""
	}
	return id
}
func (p *AMDProber) DriverVersion(context.Context) string {
	return p.text("/sys/module/amdgpu/version")
}
func (p *AMDProber) Framework(context.Context) (string, string) {
	for _, path := range []string{"/opt/rocm/.info/version", "/opt/rocm/.info/version-dev"} {
		if raw, err := p.readHostFile(path); err == nil {
			if v := strings.TrimSpace(string(raw)); v != "" {
				return "rocm", v
			}
		}
	}
	return "rocm", ""
}
func (p *AMDProber) Topology(ctx context.Context) string {
	gs, e := p.inventory(ctx)
	if e != nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("DRM\tBDF\tNUMA\n")
	for _, g := range gs {
		fmt.Fprintf(&b, "%d\t%s\t%d\n", g.Index, g.PCIeBDF, g.NUMANode)
	}
	return b.String()
}
func (p *AMDProber) Metrics(ctx context.Context) ([]Metric, error) {
	return boundedSysfsRead(ctx, &p.metricsRead, func() ([]Metric, error) { return p.metricsRaw(ctx) })
}

func (p *AMDProber) metricsRaw(ctx context.Context) ([]Metric, error) {
	snapshot, e := p.metricInventory(ctx)
	if e != nil {
		return nil, e
	}
	gs := p.liveMetricIdentities(snapshot)
	conflicts := markAMDDuplicateIdentities(gs)
	p.mu.RLock()
	baseline := make(map[int]proto.GPUInfo, len(p.lastSeen))
	for index, gpu := range p.lastSeen {
		baseline[index] = gpu
	}
	baselineDevices := p.lastSeenDevices
	baselineMemory := make(map[int]amdMemoryFacts, len(p.lastSeenMem))
	for index, facts := range p.lastSeenMem {
		baselineMemory[index] = facts
	}
	p.mu.RUnlock()
	ms := make([]Metric, 0, len(gs))
	for _, g := range gs {
		if g.UUID == "" || g.PCIeBDF == "" || conflicts[g.Index] || !sameAMDMetricIdentity(snapshot.cards[g.Index], g) {
			continue
		}
		if published, ok := baseline[g.Index]; ok && (!sameAMDMetricIdentity(published, g) || baselineDevices[g.Index] != snapshot.deviceIDs[g.Index]) {
			continue
		}
		path := fmt.Sprintf("/sys/class/drm/card%d/device", g.Index)
		values := map[string]float64{}
		facts := snapshot.memory[g.Index]
		capacityMatches := p.metricCapacityMatches(path, snapshot, g.Index)
		if capacityMatches && g.MemoryTotalBytes > 0 {
			values["gpuworks_gpu_memory_total_bytes"] = float64(g.MemoryTotalBytes)
		}
		// Do not combine used bytes from a different memory pool or a changed
		// capacity with the last published inventory baseline.
		published, hasPublished := baseline[g.Index]
		publishedMemory, hasPublishedMemory := baselineMemory[g.Index]
		if capacityMatches && hasPublished && hasPublishedMemory && published.UUID == g.UUID && published.PCIeBDF == g.PCIeBDF &&
			published.MemoryTotalBytes == g.MemoryTotalBytes && publishedMemory.source == facts.source &&
			publishedMemory.totalBytes == facts.totalBytes && facts.totalBytes == g.MemoryTotalBytes && facts.totalBytes > 0 {
			if used, err := p.number(filepath.Join(path, facts.usedPath)); err == nil && used >= 0 {
				values["gpuworks_gpu_memory_used_bytes"] = float64(used)
			}
		}
		for file, key := range map[string]string{"gpu_busy_percent": "gpuworks_gpu_utilization"} {
			if v, e := p.number(filepath.Join(path, file)); e == nil && v >= 0 {
				values[key] = float64(v)
			}
		}
		hwmons, e := p.match(filepath.Join(path, "hwmon/hwmon*"))
		if e != nil {
			// hwmon is optional per-card telemetry; one broken sysfs subtree
			// must not discard readings already collected for healthy peers.
			hwmons = nil
		}
		for _, hw := range hwmons {
			for file, spec := range map[string]struct {
				key   string
				scale float64
			}{
				"temp1_input":    {"gpuworks_gpu_temperature_celsius", 1000},
				"power1_average": {"gpuworks_gpu_power_watts", 1000000},
				"freq1_input":    {"gpuworks_gpu_sm_clock_mhz", 1000000},
			} {
				if v, e := p.number(filepath.Join(hw, file)); e == nil && v >= 0 {
					values[spec.key] = float64(v) / spec.scale
				}
			}
		}
		// A hot removal/replacement during telemetry reads must not attach the
		// replacement's values to the prior card's published UUID.
		after, ok := p.liveMetricCard(snapshot, g.Index)
		if !ok || !sameAMDMetricIdentity(g, after) {
			continue
		}
		ms = append(ms, Metric{Index: g.Index, UUID: g.UUID, Vendor: accelerator.VendorAMD, Values: values})
	}
	return ms, nil
}
