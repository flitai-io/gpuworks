package gpu

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// PPUProber implements the T-Head/Alibaba PPU backend via ppu-smi v1.18.
// Enumeration indexes are deliberately treated as ephemeral: a PPU assigned
// to VFIO disappears from the host and all following indexes can be renumbered.
type PPUProber struct {
	mu       sync.RWMutex
	lastSeen map[int]ppuIdentity

	// hostRoot is where the host's /sys is read for the PCI roster; empty
	// means "/". A test sets it so the roster describes its fixture
	// rather than whatever is in the machine running the test.
	hostRoot string
}

type ppuIdentity struct {
	uuid string
	bdf  string
}

var _ DriverAwareInjector = (*PPUProber)(nil)

func (p *PPUProber) Vendor() string { return accelerator.VendorPPU }

func (p *PPUProber) Available() bool {
	if _, err := lookCLI("ppu-smi"); err != nil {
		return false
	}
	_, err := queryPPU(context.Background(), "--query-ppu=index", "--format=csv,noheader,nounits")
	return err == nil
}

const ppuProbeFields = "index,name,architecture,uuid,minor_number,pci.bus_id,memory.total,memory.used,utilization.ppu,temperature.ppu,power.draw,driver_version,ecc.errors.uncorrected.volatile.dram,ecc.errors.uncorrected.aggregate.dram,retired_pages.double_bit.count,retired_pages.pending,remapped_rows.uncorrectable,remapped_rows.pending,remapped_rows.failure,xid.correctable.ppu_reset,xid.correctable.os_reboot,xid.correctable.cold_reboot"

func (p *PPUProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	if _, err := lookCLI("ppu-smi"); err != nil {
		return p.roster(nil, false), nil //nolint:nilerr // Absence of this vendor CLI is an unavailable probe, not a node failure.
	}
	out, err := queryPPU(ctx, "--query-ppu="+ppuProbeFields, "--format=csv,noheader,nounits")
	if err != nil {
		return p.roster(nil, false), err
	}
	gpus := parsePPUInfo(out)
	p.mu.Lock()
	if p.lastSeen == nil {
		p.lastSeen = make(map[int]ppuIdentity)
	}
	for _, g := range gpus {
		p.lastSeen[g.Index] = ppuIdentity{uuid: g.UUID, bdf: g.PCIeBDF}
	}
	p.mu.Unlock()
	return p.roster(gpus, true), nil
}

// roster places what ppu-smi listed on the roster of what the host has.
func (p *PPUProber) roster(listed []proto.GPUInfo, answered bool) []proto.GPUInfo {
	return Roster(accelerator.VendorPPU, p.hostRoot, listed, answered)
}

func (p *PPUProber) DriverVersion(ctx context.Context) string {
	out, err := queryPPU(ctx, "--query-ppu=driver_version", "--format=csv,noheader,nounits")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			return v
		}
	}
	return ""
}

func (p *PPUProber) Framework(ctx context.Context) (string, string) {
	// Card enumeration only proves that the kernel driver and HGML management
	// library are present. The VERSION view reports whether a host HGGC runtime
	// is installed, but N/A is only an observation: inference images may carry
	// HGGC themselves, and readiness is decided by the container probe.
	out, err := queryPPU(ctx, "-q", "-d", "VERSION")
	if err != nil {
		return "hggc", ""
	}
	return "hggc", parsePPUFrameworkVersion(out)
}

// PartitionProbeLibraries supplies the management-library closure omitted
// from the intentionally tiny probe image. This does not make host HGGC a
// prerequisite: libhgml/libuki are driver-side libraries already used by
// ppu-smi, while the inference image remains responsible for its HGGC runtime.
func (p *PPUProber) PartitionProbeLibraries(ctx context.Context) ([]ProbeLibrary, error) {
	names := []string{"libhgml.so", "libuki.so"}
	libraries := make([]ProbeLibrary, 0, len(names))
	for _, name := range names {
		path, err := sharedLibraryPath(ctx, name)
		if err != nil {
			return nil, err
		}
		libraries = append(libraries, ProbeLibrary{Source: path, Name: name})
	}
	return libraries, nil
}

func sharedLibraryPath(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if filepath.Base(name) != name || name == "." || name == "" {
		return "", fmt.Errorf("invalid shared library name %q", name)
	}
	var candidates []string
	if path, err := lookCLI("ldconfig"); err == nil {
		if out, runErr := cliCommand(ctx, path, "-p").Output(); runErr == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if candidate := parseLdconfigLibraryPath(line, name); candidate != "" {
					candidates = append(candidates, candidate)
				}
			}
		}
	}
	for _, dir := range []string{"/usr/lib64", "/lib64", "/usr/lib", "/lib"} {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		resolved, info, err := resolveLibraryCandidate(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("resolve probe dependency %s at %s: %w", name, candidate, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("probe dependency %s at %s is not a regular file", name, resolved)
		}
		return resolved, nil
	}
	return "", fmt.Errorf("probe dependency %s was not found on the host: %w", name, os.ErrNotExist)
}

// resolveLibraryCandidate resolves a shared-library candidate's symlinks
// and stats it, on the host when CLIs run there (through hostRoot, so an
// absolute symlink target is resolved against the host's own root, not the
// agent's /proc/1/root view of it) and locally otherwise. The returned path
// is always the host's own, never one prefixed with hostRoot, because it
// becomes a mount source for an instance container.
func resolveLibraryCandidate(candidate string) (string, os.FileInfo, error) {
	if cliOnHost.Load() {
		return resolveOnHost(candidate)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(resolved)
	return resolved, info, err
}

func parseLdconfigLibraryPath(output, name string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != name {
			continue
		}
		for i := 1; i+1 < len(fields); i++ {
			if fields[i] == "=>" && filepath.IsAbs(fields[i+1]) {
				return fields[i+1]
			}
		}
	}
	return ""
}

func parsePPUFrameworkVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "HGGC Runtime Version") {
			continue
		}
		version := strings.TrimSpace(value)
		if version == "" || strings.EqualFold(version, "N/A") {
			return ""
		}
		return strings.TrimPrefix(strings.TrimPrefix(version, "v"), "V")
	}
	return ""
}

func (p *PPUProber) Topology(ctx context.Context) string {
	out, err := queryPPU(ctx, "topo", "-m")
	if err != nil {
		return ""
	}
	return out
}

func (p *PPUProber) Metrics(ctx context.Context) ([]Metric, error) {
	const fields = "index,uuid,memory.used,memory.total,utilization.ppu,temperature.ppu,power.draw"
	out, err := queryPPU(ctx, "--query-ppu="+fields, "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return parsePPUMetrics(out), nil
}

func (p *PPUProber) RuntimeInjection(indexes []int) RuntimeInjection {
	return p.RuntimeInjectionForDriver(indexes, "")
}

func (p *PPUProber) RuntimeInjectionForDriver(indexes []int, driverName string) RuntimeInjection {
	if len(indexes) == 0 {
		return RuntimeInjection{}
	}
	rows, err := runPPU(context.Background(), "--query-ppu=index,uuid", "--format=csv,noheader,nounits")
	if err != nil {
		return RuntimeInjection{UnsupportedReason: fmt.Sprintf("cannot verify PPU device inventory: ppu-smi: %v", err)}
	}
	return p.runtimeInjection(indexes, rows, resolvePPUDevice, driverName)
}

func (p *PPUProber) runtimeInjection(indexes []int, rows string, resolve func(int, string) (string, error), driverName string) RuntimeInjection {
	args := []string{
		"--device", "/dev/alixpu:/dev/alixpu:rwm",
		"--device", "/dev/alixpu_ctl:/dev/alixpu_ctl:rwm",
	}
	byIndex := make(map[int]string)
	for _, row := range parsePPUCSV(rows) {
		if len(row) < 2 {
			continue
		}
		index, indexErr := strconv.Atoi(strings.TrimSpace(row[0]))
		if indexErr == nil && strings.TrimSpace(row[1]) != "" {
			byIndex[index] = strings.TrimSpace(row[1])
		}
	}
	deviceUUIDs := make([]string, 0, len(indexes))
	selected := make(map[string]bool)
	for _, index := range indexes {
		uuid := byIndex[index]
		if uuid == "" {
			return RuntimeInjection{UnsupportedReason: fmt.Sprintf("PPU device %d is not present in ppu-smi inventory", index)}
		}
		if !validPPUUUID(uuid) || selected[uuid] {
			return RuntimeInjection{UnsupportedReason: fmt.Sprintf("PPU device %d has an invalid or duplicate UUID %q", index, uuid)}
		}
		selected[uuid] = true
		p.mu.RLock()
		expected, hasExpected := p.lastSeen[index]
		p.mu.RUnlock()
		if hasExpected && expected.uuid != uuid {
			return RuntimeInjection{UnsupportedReason: fmt.Sprintf("PPU index %d changed from UUID %s to %s since probe", index, expected.uuid, uuid)}
		}
		path, err := resolve(index, uuid)
		if err != nil {
			return RuntimeInjection{UnsupportedReason: err.Error()}
		}
		args = append(args, "--device", path+":"+path+":rwm")
		deviceUUIDs = append(deviceUUIDs, uuid)
	}
	// Driver-independent diagnostics keep the existing UUID contract. Actual
	// workloads select their numeric view through DriverAwareInjector.
	visible := strings.Join(deviceUUIDs, ",")
	if driverName != "" {
		var err error
		visible, err = ppuVisibleDeviceIndexes(indexes, driverName)
		if err != nil {
			return RuntimeInjection{UnsupportedReason: err.Error()}
		}
	}
	return RuntimeInjection{
		Env: map[string]string{
			accelerator.EnvNVIDIAVisibleDevices: "void",
			accelerator.EnvCUDAVisibleDevices:   visible,
			accelerator.PPUEnvDeviceUUIDs:       strings.Join(deviceUUIDs, ","),
		},
		DockerArgs: args,
	}
}

// OCI exposes only the selected devices, retaining their driver enumeration
// order; the kube runtime's device plugin hands the pod the same OCI devices.
// PRoot exposes the host inventory. Keep the requested compute order in
// either view, using numeric selectors understood by HGGC and inference engines.
func ppuVisibleDeviceIndexes(indexes []int, driverName string) (string, error) {
	visibleIndexes := append([]int(nil), indexes...)
	switch driverName {
	case "docker", "nerdctl", "kube":
		inventory := append([]int(nil), indexes...)
		sort.Ints(inventory)
		for i, index := range indexes {
			visibleIndexes[i] = sort.SearchInts(inventory, index)
		}
	case "udocker":
	default:
		return "", fmt.Errorf("PPU device visibility is not supported for runtime %q", driverName)
	}
	visible := make([]string, len(visibleIndexes))
	for i, index := range visibleIndexes {
		visible[i] = strconv.Itoa(index)
	}
	return strings.Join(visible, ","), nil
}

var ppuUUIDPattern = regexp.MustCompile(`^GPU-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validPPUUUID(value string) bool { return ppuUUIDPattern.MatchString(value) }

// verifyPPUDevice protects against the observed VFIO/index churn. ppu-smi's
// minor_number is the character-device minor (reported by stat in decimal),
// while the node suffix is the enumeration ordinal. They are independent.
func resolvePPUDevice(index int, uuid string) (string, error) {
	if uuid == "" {
		return "", fmt.Errorf("PPU device %d is not present in ppu-smi inventory", index)
	}
	files, err := filepath.Glob("/proc/driver/alixpu/ppus/*/information")
	if err != nil {
		return "", fmt.Errorf("cannot enumerate PPU device mappings: %w", err)
	}
	for _, file := range files {
		data, readErr := os.ReadFile(file)
		if readErr != nil {
			continue
		}
		fields := parsePPUProcInfo(string(data))
		if fields["uuid"] != uuid {
			continue
		}
		minor, parseErr := strconv.Atoi(fields["minor"])
		if parseErr != nil || minor < 1 {
			return "", fmt.Errorf("PPU device %d has invalid driver minor for %s", index, uuid)
		}
		// The alixpu character nodes are numbered minor-1; this is independent
		// from ppu-smi's enumeration index and survives VFIO renumbering.
		path := ppuDevicePath(minor)
		if err := verifyPPUDeviceNode(index, path, minor); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("PPU device %d UUID %s has no /proc/driver/alixpu mapping", index, uuid)
}

func ppuDevicePath(minor int) string {
	return fmt.Sprintf("/dev/alixpu_ppu%d", minor-1)
}

func parsePPUProcInfo(out string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		switch key {
		case "PPU UUID":
			fields["uuid"] = value
		case "Device Minor":
			fields["minor"] = value
		}
	}
	return fields
}

// verifyPPUDeviceNode checks the host's live /dev when CLIs run there: a
// privileged container's own /dev is a copy made at container start, so a
// device node created afterward (the driver finishing its init) would
// otherwise be reported missing.
func verifyPPUDeviceNode(index int, path string, expected int) error {
	info, err := statTool(path)
	if err != nil {
		return fmt.Errorf("PPU device %d is unavailable: %s: %w", index, path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot inspect PPU device %d character-device minor", index)
	}
	actual := int(unixMinor(uint64(stat.Rdev)))
	if actual != expected {
		return fmt.Errorf("PPU device %d mapping mismatch: %s minor=%d, driver minor=%d", index, path, actual, expected)
	}
	return nil
}

func unixMinor(dev uint64) uint64 {
	// Linux's new-style dev_t stores the low eight minor bits at 0..7 and
	// the remaining minor bits at 12..31. This is also the representation
	// returned by stat(2) for the character devices used by alixpu.
	return (dev & 0xff) | ((dev >> 12) & 0xffffff00)
}

// queryPPU runs a read-only ppu-smi query, which a slow driver may answer
// from the last result (see QueryVendorCLI).
func queryPPU(ctx context.Context, args ...string) (string, error) {
	return queryLogged(ctx, accelerator.VendorPPU, "ppu-smi", args...)
}

// runPPU runs ppu-smi for an answer that must be fresh.
func runPPU(ctx context.Context, args ...string) (string, error) {
	slog.Debug("gpu command", "vendor", accelerator.VendorPPU, "args", args)
	out, err := RunVendorCLI(ctx, nil, "ppu-smi", args...)
	if err != nil {
		slog.Warn("gpu command failed", "vendor", accelerator.VendorPPU, "args", args, "err", err)
	}
	return out, err
}

func parsePPUInfo(out string) []proto.GPUInfo {
	rows := parsePPUCSV(out)
	gpus := make([]proto.GPUInfo, 0, len(rows))
	for _, f := range rows {
		if len(f) < 22 {
			continue
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		memMiB, _ := strconv.ParseInt(f[6], 10, 64)
		gpu := proto.GPUInfo{
			Index: idx, UUID: f[3], Name: f[1], MemoryTotalBytes: memMiB * 1024 * 1024,
			Vendor: accelerator.VendorPPU, ArchID: ppuArchID(f[1], f[2]), PCIeBDF: accelerator.NormalizeBDF(f[5]),
			NUMANode: numaForBDF(f[5]), DriverVersion: f[11],
		}
		code, evidence := ppuHealth(f)
		SetCardFinding(&gpu, code, evidence)
		gpus = append(gpus, gpu)
	}
	return gpus
}

func ppuArchID(name, architecture string) string {
	name = strings.TrimSpace(name)
	upper := strings.ToUpper(name)
	if i := strings.Index(upper, "ZW"); i >= 0 {
		return name[i:]
	}
	return strings.TrimSpace(architecture)
}

func parsePPUMetrics(out string) []Metric {
	rows := parsePPUCSV(out)
	metrics := make([]Metric, 0, len(rows))
	for _, f := range rows {
		if len(f) < 7 {
			continue
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		m := Metric{Index: idx, UUID: f[1], Vendor: accelerator.VendorPPU, Values: map[string]float64{}}
		addPPUMetric(m.Values, "gpuworks_gpu_memory_used_bytes", f[2], 1024*1024)
		addPPUMetric(m.Values, "gpuworks_gpu_memory_total_bytes", f[3], 1024*1024)
		addPPUMetric(m.Values, "gpuworks_gpu_utilization", f[4], 1)
		addPPUMetric(m.Values, "gpuworks_gpu_temperature_celsius", f[5], 1)
		addPPUMetric(m.Values, "gpuworks_gpu_power_watts", f[6], 1)
		metrics = append(metrics, m)
	}
	return metrics
}

func parsePPUCSV(out string) [][]string {
	r := csv.NewReader(strings.NewReader(out))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil
	}
	for i := range rows {
		for j := range rows[i] {
			rows[i][j] = strings.TrimSpace(rows[i][j])
		}
	}
	return rows
}

// ppuHealth decides a card's level from the health fields of ppuProbeFields
// (indexes 12..21, in the order of accelerator.PPUHealthMemoryFields then
// accelerator.PPUHealthXIDFields). Lifetime counters of errors that were already
// handled (aggregate ECC, retired pages, replaced rows) are history and leave
// the card healthy; errors since the last reset, a failed replacement and the
// driver's recovery events stop scheduling; a replacement waiting for a reset
// keeps the card schedulable at a lower preference.
// ppuHealth is the finding ppu-smi's row states: the most severe code and
// the fields that contributed. What that code means for the card — its
// level, its Healthy flag — is SetCardFinding's to decide, so this reads
// the vendor's table and nothing else.
func ppuHealth(f []string) (string, []accelerator.HealthField) {
	memory, xid := f[12:19], f[19:22]
	names := accelerator.PPUHealthMemoryFields
	var codes []string
	var evidence []accelerator.HealthField
	// flag records a field that contributed to a finding; only those are
	// evidence; the full state is one recheck command away.
	flag := func(code, name, value string) {
		codes = append(codes, code)
		evidence = append(evidence, accelerator.HealthField{Name: name, Value: value})
	}
	for i, check := range []struct {
		code string
		hit  func(string) bool
	}{
		{accelerator.HealthMemoryUncorrectable, func(v string) bool { return ppuCount(v) > 0 }},
		{accelerator.HealthMemoryErrorHistory, func(v string) bool { return ppuCount(v) > 0 }},
		{accelerator.HealthMemoryErrorHistory, func(v string) bool { return ppuCount(v) > 0 }},
		{accelerator.HealthMemoryRepairPending, ppuYes},
		{accelerator.HealthMemoryErrorHistory, func(v string) bool { return ppuCount(v) > 0 }},
		{accelerator.HealthMemoryRepairPending, ppuYes},
		{accelerator.HealthMemoryRemapFailed, ppuYes},
	} {
		if check.hit(memory[i]) {
			flag(check.code, names[i], memory[i])
		}
	}
	for i, code := range []string{accelerator.HealthResetRequired, accelerator.HealthRebootRequired, accelerator.HealthPowerCycleRequired} {
		if ppuSignalPresent(xid[i]) {
			flag(code, accelerator.PPUHealthXIDFields[i], xid[i])
		}
	}
	return accelerator.MostSevereHealth(codes...), evidence
}

func ppuCount(v string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func ppuYes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1", "failure", "failed":
		return true
	default:
		return false
	}
}

func ppuSignalPresent(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v != "" && v != "0" && v != "no" && v != "n/a" && v != "[n/a]"
}

func addPPUMetric(dst map[string]float64, name, value string, scale float64) {
	if v, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
		dst[name] = v * scale
	}
}
