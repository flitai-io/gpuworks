package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	dockerruntime "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/agent/sysinfo"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Doctor writes a human-readable self-check to w. It is the standardized
// environment probe covering host facts, GPU detection, and docker
// availability.
func Doctor(ctx context.Context, w io.Writer, drivers ...runtime.Driver) {
	doctorWithProber(ctx, w, gpu.New(), drivers...)
}

func doctorWithProber(ctx context.Context, w io.Writer, p gpu.Prober, drivers ...runtime.Driver) {
	report := buildDoctorReport(ctx, p, nil, drivers...)
	_, _ = io.WriteString(w, report.Report)
}

// doctorBudget bounds a doctor run the server asked for, below the server's
// wait for it (nodeDoctorTimeout in internal/server/api): a check still
// running at the deadline is cut short and the report goes back with what it
// found, rather than the server giving up on the whole report.
const doctorBudget = 100 * time.Second

// diskLowPercent is the free-space share below which a path the node depends
// on is reported.
const diskLowPercent = 10

// buildDoctorReport runs every check and returns the structured report with
// its English text rendering. disks are the paths beyond the root filesystem
// the node depends on (models root, container data, storages); the CLI has
// none to offer. It offers doctorNVIDIAChecks no state directory (""):
// callers that have a running node's own use buildDoctorReportForNode.
func buildDoctorReport(ctx context.Context, p gpu.Prober, disks []proto.PathUsage, drivers ...runtime.Driver) proto.DoctorReport {
	return buildDoctorReportWith(ctx, p, disks, nil, "", drivers...)
}

// buildDoctorReportForNode is buildDoctorReport with the running node's own
// state directory, so the NCCL check's script file lands under it (the same
// path on the host and in the agent's own container) rather than
// os.TempDir() when CLIs run on the host.
func buildDoctorReportForNode(ctx context.Context, p gpu.Prober, disks []proto.PathUsage, stateDir string, drivers ...runtime.Driver) proto.DoctorReport {
	return buildDoctorReportWith(ctx, p, disks, nil, stateDir, drivers...)
}

// buildDoctorReportWith is buildDoctorReport with an explicit source for the
// state-directory access check, for callers that have no running driver (the
// CLI): nil means the selected driver is asked.
func buildDoctorReportWith(ctx context.Context, p gpu.Prober, disks []proto.PathUsage, access runtime.StateAccessReporter, stateDir string, drivers ...runtime.Driver) proto.DoctorReport {
	var r proto.DoctorReport
	var driver runtime.Driver
	if len(drivers) > 0 {
		driver = drivers[0]
	}
	runtimeName := ""
	if driver != nil {
		runtimeName = driver.Name()
	}
	doctorHost(&r, disks, runtimeName)
	doctorRuntimes(ctx, &r, driver)
	doctorProvision(&r, driver)
	if access == nil {
		access, _ = driver.(runtime.StateAccessReporter)
	}
	doctorStateAccess(&r, access)
	if p == nil {
		p = gpu.New()
	}
	gpus := doctorAccelerators(ctx, &r, p)
	// How containers get the accelerators means nothing on a Mac, whose
	// engines are host processes with the GPU to hand.
	if !doctorOnMac {
		doctorDeviceFacts(ctx, &r, p, driver, gpus)
	}
	if r.Accelerators != nil && strings.EqualFold(p.Vendor(), "nvidia") {
		// The whole-node checks open every card at once.
		idents := make([]string, 0, len(gpus))
		for _, g := range gpus {
			idents = append(idents, accelerator.CardIdent(g.UUID, g.PCIeBDF))
		}
		end := doctorBeginAttempt(ctx, idents...)
		doctorNVIDIAChecks(ctx, &r, len(gpus), stateDir)
		end()
	}
	// Dies that exist only in a fixture say nothing about the host's CANN.
	if r.Accelerators != nil && !gpu.IsSynthetic(p) {
		doctorCardRuntimeChecks(ctx, &r, gpus)
	}
	if r.Accelerators != nil && !gpu.IsSynthetic(p) {
		doctorPeerMemory(&r, p.Vendor())
	}
	if doctorOnMac {
		doctorMacChecks(ctx, &r, gpus)
	}
	sort.SliceStable(r.Findings, func(i, j int) bool {
		return r.Findings[i].Level == proto.DoctorLevelError && r.Findings[j].Level != proto.DoctorLevelError
	})
	r.Report = renderDoctorText(r)
	return r
}

// doctorOnMac selects the macOS checks; a report test sets it.
var doctorOnMac = goruntime.GOOS == "darwin"

// doctorCollectSysinfo lets report tests supply host capacity independently of
// the filesystem on which the test runs.
var doctorCollectSysinfo = sysinfo.Collect

func doctorHost(r *proto.DoctorReport, extra []proto.PathUsage, runtimeName string) {
	sys := doctorCollectSysinfo()
	r.Host = &proto.DoctorHost{
		Hostname: sys.Hostname, OS: sys.OS, Arch: sys.Arch, Kernel: sys.Kernel,
		CPUModel: sys.CPUModel, CPUCores: sys.CPUCores,
		MemTotalBytes: sys.MemTotalKB * 1024, MemAvailBytes: sys.MemAvailKB * 1024,
		Disks: []proto.DoctorDisk{},
	}
	root := proto.PathUsage{Kind: "root", Path: "/", TotalBytes: sys.DiskTotalKB * 1024, AvailableBytes: sys.DiskAvailKB * 1024}
	listed := map[string]bool{}
	for _, usage := range append([]proto.PathUsage{root}, extra...) {
		if usage.TotalBytes > 0 && !listed[usage.Path] {
			listed[usage.Path] = true
			r.Host.Disks = append(r.Host.Disks, proto.DoctorDisk{Kind: usage.Kind, Path: usage.Path, TotalBytes: usage.TotalBytes, AvailBytes: usage.AvailableBytes})
		}
	}
	// Paths on one filesystem share its free space: report the filesystem
	// once and name every path it holds.
	var order []string
	groups := map[string][]proto.DoctorDisk{}
	for _, disk := range r.Host.Disks {
		if disk.AvailBytes*100 >= disk.TotalBytes*diskLowPercent {
			continue
		}
		key := "path:" + disk.Path
		if dev, ok := doctorFilesystemOf(disk.Path); ok {
			key = "dev:" + strconv.FormatUint(dev, 10)
		}
		if groups[key] == nil {
			order = append(order, key)
		}
		groups[key] = append(groups[key], disk)
	}
	for _, key := range order {
		disks := groups[key]
		disk := disks[0]
		paths := make([]string, len(disks))
		for i, d := range disks {
			paths[i] = d.Path
		}
		finding := proto.DoctorFinding{
			Code: proto.DoctorDiskLow, Level: proto.DoctorLevelWarning, Disk: &disk,
			Probe: "statfs(" + strings.Join(paths, ", ") + ")", Recheck: "df -h " + strings.Join(paths, " "),
		}
		if len(paths) > 1 {
			finding.Evidence = []proto.DoctorField{{Name: "paths", Value: strings.Join(paths, ", ")}}
		}
		r.Findings = append(r.Findings, finding)
	}
	doctorFilesystems(r, runtimeName)
}

// doctorFilesystemOf identifies the filesystem holding a path (its device
// number), walking up to the nearest existing ancestor for a directory the
// agent has not created yet. It is a package var so a test can place paths on
// filesystems of its choosing.
var doctorFilesystemOf = func(path string) (uint64, bool) {
	for {
		if info, err := os.Stat(path); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				return uint64(st.Dev), true
			}
			return 0, false
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, false
		}
		path = parent
	}
}

// doctorRuntimes reports the container runtimes and tools, and flags the
// selected container runtime when it no longer answers.
func doctorRuntimes(ctx context.Context, r *proto.DoctorReport, driver runtime.Driver) {
	selected := ""
	if driver != nil {
		selected = driver.Name()
	}
	checks := []struct{ name, recheck string }{{"docker", "docker version"}, {"nerdctl", "nerdctl --version"}, {"python3", ""}, {"dcgmi", ""}}
	if doctorOnMac {
		// A Mac's engines run from environment packages that carry their own
		// Python; docker and DCGM play no part.
		checks = nil
	}
	args := map[string][]string{
		"docker":  {"version", "--format", "{{.Server.Version}}"},
		"nerdctl": {"--version"},
		"python3": {"--version"},
		// dcgmi's documented flag is --version; -v is not a recognized dcgmi flag.
		"dcgmi": {"--version"},
	}
	listed := false
	for _, check := range checks {
		result := checkCommand(ctx, check.name, args[check.name]...)
		rt := proto.DoctorRuntime{Name: check.name, Status: result.status, Version: result.version,
			Selected: check.name == selected, Command: check.name + " " + strings.Join(args[check.name], " ")}
		r.Runtimes = append(r.Runtimes, rt)
		listed = listed || rt.Selected
		if rt.Selected && result.status != proto.DoctorRuntimeOK {
			r.Findings = append(r.Findings, proto.DoctorFinding{
				Code: proto.DoctorRuntimeUnavailable, Level: proto.DoctorLevelError,
				Probe: rt.Command, Evidence: []proto.DoctorField{{Name: "result", Value: result.detail}},
				Recheck: check.recheck,
			})
		}
	}
	if selected != "" && !listed {
		r.Runtimes = append(r.Runtimes, proto.DoctorRuntime{Name: selected, Status: proto.DoctorRuntimeOK, Selected: true})
	}
	if diagnostic, ok := driver.(interface{ Diagnostics() string }); ok {
		r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactRuntimeDetails, Value: diagnostic.Diagnostics()})
	}
}

// doctorUnrecognizedFunctions reports the PCI functions of a supported
// accelerator vendor that no rule recognized. Dropping them is how a new
// product line at an unannounced class would read as a card that is simply
// not in the machine; naming it here gives whoever runs doctor the exact
// ids to send us.
func doctorUnrecognizedFunctions(r *proto.DoctorReport) {
	unknown := gpu.UnrecognizedAccelerators("")
	if len(unknown) == 0 {
		return
	}
	evidence := make([]proto.DoctorField, 0, len(unknown))
	for _, fn := range unknown {
		evidence = append(evidence, proto.DoctorField{Name: fn.BDF, Value: fn.Label()})
	}
	r.Findings = append(r.Findings, proto.DoctorFinding{
		Code: proto.DoctorAcceleratorUnrecognized, Level: proto.DoctorLevelWarning,
		Probe: "read /sys/bus/pci/devices", Evidence: evidence,
		Recheck: "lspci -nn -k",
	})
}

// doctorAccelerators records the inventory and turns each card's health
// finding into a finding grouped by code, one target per card.
func doctorAccelerators(ctx context.Context, r *proto.DoctorReport, p gpu.Prober) []proto.GPUInfo {
	// Not gated on p.Available(), which asks whether the vendor CLI can
	// answer. A prober now describes the cards on the host's PCI bus
	// whether or not its CLI answers, and a host whose CLI is wedged is
	// the one somebody is running doctor on: on 198.51.100.31, where
	// npu-smi cannot initialize, the gate turned eight 910B4 into "no
	// accelerator tool found (CPU-only node)".
	vendor := p.Vendor()
	gpus, err := p.Probe(ctx)
	if held, ok := ctx.Value(doctorHeldFindingsKey{}).(func([]proto.GPUInfo)); ok && held != nil {
		held(gpus)
	}
	doctorUnrecognizedFunctions(r)
	if vendor == "" && len(gpus) == 0 && err == nil {
		// Nothing here claims to be an accelerator host.
		return nil
	}
	r.Accelerators = &proto.DoctorAccelerators{Vendor: vendor, Driver: p.DriverVersion(ctx), GPUs: []proto.DoctorGPU{}}
	if err != nil && !gpu.CLIHasNothingToAnswer(gpus) {
		// The tool failing is a finding, not a reason to drop the cards it
		// failed to describe. A probe that could not ask still knows which
		// cards are in the machine, and on the host somebody is running
		// doctor on that list is the point: returning here showed eight
		// 910B4 as none (198.51.100.31, 2026-10-15).
		r.Findings = append(r.Findings, proto.DoctorFinding{
			Code: proto.DoctorAcceleratorProbeFailed, Level: proto.DoctorLevelError,
			Probe: accelerator.ToolCommand(vendor), Evidence: []proto.DoctorField{{Name: "error", Value: err.Error()}},
			Recheck: accelerator.ToolCommand(vendor),
		})
	}
	byCode := map[string]*proto.DoctorFinding{}
	var order []string
	for _, g := range gpus {
		status := g.HealthStatus
		if status == "" {
			status = accelerator.HealthOK
		}
		r.Accelerators.GPUs = append(r.Accelerators.GPUs, proto.DoctorGPU{
			Index: g.Index, Name: g.Name, MemoryBytes: g.MemoryTotalBytes, PCIeBDF: g.PCIeBDF, Ownership: g.Ownership,
			NUMANode: g.NUMANode, HealthStatus: status, HealthCode: g.HealthCode,
		})
		if status == accelerator.HealthOK {
			continue
		}
		finding := byCode[g.HealthCode]
		if finding == nil {
			level := proto.DoctorLevelWarning
			if status == accelerator.HealthError {
				level = proto.DoctorLevelError
			}
			finding = &proto.DoctorFinding{Code: g.HealthCode, Level: level}
			byCode[g.HealthCode] = finding
			order = append(order, g.HealthCode)
		}
		evidence := accelerator.ParseHealthEvidence(g.HealthMessage)
		target := proto.DoctorTarget{Index: g.Index, PCIeBDF: g.PCIeBDF}
		for _, f := range evidence {
			target.Evidence = append(target.Evidence, proto.DoctorField{Name: f.Name, Value: f.Value})
		}
		card := accelerator.HealthCard{Vendor: vendor, Code: g.HealthCode, BDF: g.PCIeBDF, Index: g.Index, UUID: g.UUID}
		if guide, ok := accelerator.HealthGuideFor(card, evidence); ok {
			target.Probe, target.Recheck, target.Actions = guide.Probe, guide.Recheck, guide.Actions
		}
		finding.Targets = append(finding.Targets, target)
	}
	attachProcesses(ctx, p, byCode)
	for _, code := range order {
		r.Findings = append(r.Findings, *byCode[code])
	}
	return gpus
}

// attachProcesses names the compute processes on every card that has a
// recovery action, so the reader knows what must stop first. A failed listing
// leaves the targets without processes rather than failing the report.
func attachProcesses(ctx context.Context, p gpu.Prober, findings map[string]*proto.DoctorFinding) {
	lister, ok := p.(gpu.ComputeProcessLister)
	if !ok {
		return
	}
	needed := false
	for _, f := range findings {
		for _, t := range f.Targets {
			needed = needed || len(t.Actions) > 0
		}
	}
	if !needed {
		return
	}
	procs, err := lister.ComputeProcesses(ctx)
	if err != nil {
		return
	}
	for _, f := range findings {
		for i := range f.Targets {
			t := &f.Targets[i]
			if len(t.Actions) == 0 {
				continue
			}
			for _, proc := range procs {
				if gpu.SameBDF(proc.PCIeBDF, t.PCIeBDF) {
					t.Processes = append(t.Processes, proto.DoctorProcess{PID: proc.PID, Name: proc.Name})
				}
			}
		}
	}
}

// doctorDeviceFacts states how workloads get the accelerators: the vendor
// runtime plugin, the selected runtime's device access, and the injection.
func doctorDeviceFacts(ctx context.Context, r *proto.DoctorReport, p gpu.Prober, driver runtime.Driver, gpus []proto.GPUInfo) {
	if r.Accelerators == nil {
		return
	}
	driverName := ""
	if driver != nil {
		driverName = driver.Name()
	} else {
		driverName = doctorDetectedRuntime(ctx)
	}
	if driverName != "" {
		plugin := runtime.DetectPlugin(ctx, p, driverName)
		value := "none"
		switch {
		case plugin.Name == "":
		case !plugin.Installed:
			value = plugin.Name + " (not installed)"
		case plugin.Version == "":
			value = plugin.Name + " (version unknown)"
		default:
			value = plugin.Name + " " + plugin.Version
		}
		r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactRuntimePlugin, Value: value})
	}
	if diagnostic, ok := driver.(runtime.DeviceAccessDiagnoser); ok {
		value := ""
		if spec, err := doctorWorkloadSpec(ctx, p, driverName, gpus); err != nil {
			value = "device access: unavailable (" + err.Error() + ")"
		} else {
			value = diagnostic.DeviceAccessDiagnostics(spec)
		}
		r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactDeviceAccess, Value: value})
	}
	for _, g := range gpus {
		if !g.Healthy {
			continue
		}
		value := "explicit"
		// The same question a launch asks, through the same seam: a backend
		// whose injection depends on the container runtime must be asked
		// about this host's runtime, or the report says a deployment will
		// work where the server will refuse it.
		inj := gpu.ResolveInjection(ctx, p, gpu.InjectionRequest{Indexes: []int{g.Index}, DriverName: driverName, NNodes: 1})
		if inj.UnsupportedReason != "" {
			value = "unavailable: " + inj.UnsupportedReason
		}
		r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactDeviceInjection, Value: value})
		break
	}
}

// doctorDetectedRuntime is deliberately read-only and lightweight. The CLI
// doctor has no NodeRuntime instance to borrow, so it discovers only an
// already-running Docker/nerdctl backend for plugin reporting.
func doctorDetectedRuntime(ctx context.Context) string {
	if ok, _ := dockerruntime.New().Detect(ctx); ok {
		return "docker"
	}
	if _, err := exec.LookPath("nerdctl"); err == nil {
		if result := checkCmd(ctx, "nerdctl", "-n", "gpuworks", "ps"); result != "present (query failed)" && result != "not found" {
			return "nerdctl"
		}
	}
	return ""
}

func doctorWorkloadSpec(ctx context.Context, p gpu.Prober, driverName string, gpus []proto.GPUInfo) (runtime.WorkloadSpec, error) {
	indexes := make([]int, 0, len(gpus))
	for _, g := range gpus {
		if g.Healthy {
			indexes = append(indexes, g.Index)
		}
	}
	if len(indexes) == 0 {
		return runtime.WorkloadSpec{}, fmt.Errorf("no healthy accelerator is available")
	}
	inj := gpu.ResolveInjection(ctx, p, gpu.InjectionRequest{Indexes: indexes, DriverName: driverName, NNodes: 1})
	if inj.UnsupportedReason != "" {
		return runtime.WorkloadSpec{}, fmt.Errorf("%s", inj.UnsupportedReason)
	}
	mounts := make([]runtime.Mount, 0, len(inj.Mounts))
	for _, mount := range inj.Mounts {
		mounts = append(mounts, runtime.Mount{Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly})
	}
	return runtime.WorkloadSpec{GPUIndexes: indexes, Env: inj.Env, DockerArgs: inj.DockerArgs, Mounts: mounts}, nil
}

// GrantDeviceAccess gives a non-root runtime user access to the device nodes
// selected by the same prober/runtime rules used for workloads.
func GrantDeviceAccess(ctx context.Context, p gpu.Prober, driver runtime.DeviceAccessGranter, driverName, username string) (string, error) {
	if p == nil {
		p = gpu.New()
	}
	if !p.Available() {
		return "", fmt.Errorf("gpu prober is unavailable")
	}
	gpus, err := p.Probe(ctx)
	if err != nil {
		return "", fmt.Errorf("probe accelerators: %w", err)
	}
	spec, err := doctorWorkloadSpec(ctx, p, driverName, gpus)
	if err != nil {
		return "", err
	}
	return driver.GrantDeviceAccess(ctx, spec, username)
}

func gib(kb int64) float64 { return float64(kb) / (1024 * 1024) }

// checkDoctorCmdLookPath and checkDoctorCmdRun are package vars (not direct
// exec calls in checkCmd's body) so a test can simulate a present/absent
// binary and its output deterministically — for docker, python3 or dcgmi —
// without depending on what happens to be installed on the machine running
// the test.
var (
	checkDoctorCmdLookPath = exec.LookPath
	checkDoctorCmdRun      = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}
)

// commandCheck is a tool's presence and version: version is the version
// number found in the output, detail is what to show when the query failed.
type commandCheck struct {
	status, version, detail string
}

var versionRE = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)+(?:[-+][0-9A-Za-z.]+)?`)

func checkCommand(ctx context.Context, name string, args ...string) commandCheck {
	if _, err := checkDoctorCmdLookPath(name); err != nil {
		return commandCheck{status: proto.DoctorRuntimeNotFound, detail: "not found"}
	}
	out, err := checkDoctorCmdRun(ctx, name, args...)
	if err != nil {
		detail := err.Error()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(strings.TrimSpace(string(exitErr.Stderr))) > 0 {
			detail = strings.TrimSpace(string(exitErr.Stderr)) + " (" + detail + ")"
		}
		return commandCheck{status: proto.DoctorRuntimeQueryFailed, detail: detail}
	}
	text := strings.TrimSpace(string(out))
	version := versionRE.FindString(text)
	if version == "" {
		version = strings.Join(strings.Fields(text), " ")
	}
	return commandCheck{status: proto.DoctorRuntimeOK, version: version}
}

func checkCmd(ctx context.Context, name string, args ...string) string {
	if _, err := checkDoctorCmdLookPath(name); err != nil {
		return "not found"
	}
	out, err := checkDoctorCmdRun(ctx, name, args...)
	if err != nil {
		return "present (query failed)"
	}
	return trim(string(out))
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
