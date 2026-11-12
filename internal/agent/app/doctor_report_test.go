package app

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// doctorPPUProber is a PPU host as ppu-smi describes it: card 5 waits for a
// row replacement and runs a process, card 6 waits idle, card 7 failed a
// replacement, card 0 is healthy.
type doctorPPUProber struct {
	*gpu.FixedProber
	procs    []gpu.ComputeProcess
	procsErr error
	probeErr error
	listed   int
}

func (p *doctorPPUProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	if p.probeErr != nil {
		return nil, p.probeErr
	}
	return p.FixedProber.Probe(ctx)
}

func (p *doctorPPUProber) ComputeProcesses(context.Context) ([]gpu.ComputeProcess, error) {
	p.listed++
	return p.procs, p.procsErr
}

const pendingEvidence = "remapped_rows.pending=Yes\nremapped_rows.uncorrectable=0\nremapped_rows.failure=No"

func newDoctorPPUProber() *doctorPPUProber {
	return &doctorPPUProber{
		FixedProber: &gpu.FixedProber{VendorName: "ppu", GPUs: []proto.GPUInfo{
			{Index: 0, Name: "PPU-ZW810E", Vendor: "ppu", PCIeBDF: "0001:c8:00.0", Healthy: true, HealthStatus: "OK"},
			{Index: 5, Name: "PPU-ZW810E", Vendor: "ppu", PCIeBDF: "0000:7e:00.0", NUMANode: 0, Healthy: true,
				HealthStatus: "Warning", HealthCode: "memory_repair_pending", HealthMessage: pendingEvidence},
			{Index: 6, Name: "PPU-ZW810E", Vendor: "ppu", PCIeBDF: "0001:0b:00.0", Healthy: true,
				HealthStatus: "Warning", HealthCode: "memory_repair_pending", HealthMessage: pendingEvidence},
			{Index: 7, Name: "PPU-ZW810E", Vendor: "ppu", PCIeBDF: "0000:c6:00.0", Healthy: false,
				HealthStatus: "Error", HealthCode: "memory_remap_failed", HealthMessage: "remapped_rows.failure=Yes"},
		}},
		procs: []gpu.ComputeProcess{
			{PID: 135707, Name: "sglang::scheduler", PCIeBDF: "00000000:7E:00.0"},
			{PID: 1741701, Name: "VLLM::EngineCore", PCIeBDF: "00000001:C8:00.0"},
		},
	}
}

func withDoctorRootDisk(t *testing.T, total, available int64) {
	t.Helper()
	original := doctorCollectSysinfo
	t.Cleanup(func() { doctorCollectSysinfo = original })
	doctorCollectSysinfo = func() proto.SysInfo {
		return proto.SysInfo{DiskTotalKB: total / 1024, DiskAvailKB: available / 1024}
	}
}

func withNoTools(t *testing.T) {
	withDoctorRootDisk(t, 200<<30, 100<<30)
	withDoctorExecSeams(t, func(string) (string, error) { return "", exec.ErrNotFound },
		func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("must not run") })
}

func TestDoctorReportGroupsAcceleratorFindingsWithEvidenceAndCommands(t *testing.T) {
	withNoTools(t)
	p := newDoctorPPUProber()
	r := buildDoctorReport(context.Background(), p, nil)

	if r.Accelerators == nil || r.Accelerators.Vendor != "ppu" || len(r.Accelerators.GPUs) != 4 {
		t.Fatalf("accelerators = %+v", r.Accelerators)
	}
	if g := r.Accelerators.GPUs[1]; g.PCIeBDF != "0000:7e:00.0" || g.HealthStatus != "Warning" || g.HealthCode != "memory_repair_pending" {
		t.Fatalf("gpu 5 = %+v", g)
	}
	var codes []string
	for _, f := range r.Findings {
		codes = append(codes, f.Level+"/"+f.Code)
	}
	// Errors come first; cards sharing a finding are one finding.
	if !reflect.DeepEqual(codes, []string{"error/memory_remap_failed", "warning/memory_repair_pending"}) {
		t.Fatalf("findings = %v", codes)
	}
	failed, pending := r.Findings[0], r.Findings[1]
	if len(pending.Targets) != 2 || pending.Targets[0].Index != 5 || pending.Targets[1].Index != 6 {
		t.Fatalf("pending targets = %+v", pending.Targets)
	}
	card5 := pending.Targets[0]
	if card5.Recheck != "ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0" ||
		!reflect.DeepEqual(card5.Actions, []string{"ppu-smi -r -i 0000:7e:00.0"}) ||
		!strings.HasPrefix(card5.Probe, "ppu-smi --query-ppu=pci.bus_id,") {
		t.Fatalf("card 5 commands = %+v", card5)
	}
	wantEvidence := []proto.DoctorField{{Name: "remapped_rows.pending", Value: "Yes"}, {Name: "remapped_rows.uncorrectable", Value: "0"}, {Name: "remapped_rows.failure", Value: "No"}}
	if !reflect.DeepEqual(card5.Evidence, wantEvidence) {
		t.Fatalf("card 5 evidence = %+v", card5.Evidence)
	}
	// Only the process on the card itself is named, matched across the
	// tool's 8-digit domain and the stored 4-digit one.
	if !reflect.DeepEqual(card5.Processes, []proto.DoctorProcess{{PID: 135707, Name: "sglang::scheduler"}}) || len(pending.Targets[1].Processes) != 0 {
		t.Fatalf("processes = %+v / %+v", card5.Processes, pending.Targets[1].Processes)
	}
	// A failed replacement has no command to run, so no processes are listed for it.
	if len(failed.Targets) != 1 || len(failed.Targets[0].Actions) != 0 || len(failed.Targets[0].Processes) != 0 {
		t.Fatalf("failed = %+v", failed)
	}
	if p.listed != 1 {
		t.Fatalf("process listing ran %d times", p.listed)
	}

	for _, want := range []string{
		"5 0000:7e:00.0", "1 OK, 2 Warning, 1 Error",
		"[warning] memory_repair_pending: accelerator 5, 6",
		"recheck   ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0",
		"action    ppu-smi -r -i 0000:7e:00.0",
		"running   pid 135707 sglang::scheduler",
		"remapped_rows.pending = Yes",
	} {
		if !strings.Contains(strings.Join(strings.Fields(r.Report), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("text report lacks %q:\n%s", want, r.Report)
		}
	}

	// The RPC result survives the wire.
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back proto.DoctorReport
	if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, r) {
		t.Fatalf("round trip differs: %v\n%s", err, raw)
	}
}

func TestDoctorReportKeepsFindingsWhenProcessListingFails(t *testing.T) {
	withNoTools(t)
	p := newDoctorPPUProber()
	p.procsErr = errors.New("ppu-smi timed out")
	r := buildDoctorReport(context.Background(), p, nil)
	if len(r.Findings) != 2 || len(r.Findings[1].Targets[0].Actions) != 1 || len(r.Findings[1].Targets[0].Processes) != 0 {
		t.Fatalf("findings = %+v", r.Findings)
	}
}

func TestDoctorReportSkipsProcessListingWithoutActions(t *testing.T) {
	withNoTools(t)
	p := newDoctorPPUProber()
	p.GPUs = p.GPUs[:1]
	buildDoctorReport(context.Background(), p, nil)
	if p.listed != 0 {
		t.Fatal("processes listed for a node with nothing to recover")
	}
}

func TestDoctorReportProbeFailure(t *testing.T) {
	withNoTools(t)
	p := newDoctorPPUProber()
	p.probeErr = errors.New("ppu-smi: exit status 9")
	r := buildDoctorReport(context.Background(), p, nil)
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %+v", r.Findings)
	}
	f := r.Findings[0]
	if f.Code != proto.DoctorAcceleratorProbeFailed || f.Level != proto.DoctorLevelError || f.Recheck != "ppu-smi" ||
		!reflect.DeepEqual(f.Evidence, []proto.DoctorField{{Name: "error", Value: "ppu-smi: exit status 9"}}) {
		t.Fatalf("finding = %+v", f)
	}
}

func TestDoctorReportFlagsLowDisk(t *testing.T) {
	withNoTools(t)
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), []proto.PathUsage{
		{Kind: "models", Path: "/data/models", TotalBytes: 200 << 30, AvailableBytes: 14 << 30},
		{Kind: "docker", Path: "/var/lib/docker", TotalBytes: 200 << 30, AvailableBytes: 100 << 30},
		{Kind: "udocker", Path: "/var/lib/gpuworks/udocker"}, // not measured: left out
	})
	var disk *proto.DoctorFinding
	for i := range r.Findings {
		if r.Findings[i].Code == proto.DoctorDiskLow && r.Findings[i].Disk.Path == "/data/models" {
			disk = &r.Findings[i]
		}
	}
	if disk == nil || disk.Level != proto.DoctorLevelWarning || disk.Recheck != "df -h /data/models" || disk.Probe != "statfs(/data/models)" {
		t.Fatalf("findings = %+v", r.Findings)
	}
	for _, f := range r.Findings {
		if f.Code == proto.DoctorDiskLow && f.Disk.Path == "/var/lib/docker" {
			t.Fatalf("half-free disk flagged: %+v", f)
		}
	}
	paths := map[string]bool{}
	for _, d := range r.Host.Disks {
		paths[d.Path] = true
	}
	if !paths["/"] || !paths["/data/models"] || !paths["/var/lib/docker"] || paths["/var/lib/gpuworks/udocker"] || r.Host.Disks[0].TotalBytes != 200<<30 {
		t.Fatalf("disks = %+v", r.Host.Disks)
	}
}

func TestDoctorReportFlagsSelectedRuntimeThatDoesNotAnswer(t *testing.T) {
	withDoctorRootDisk(t, 200<<30, 100<<30)
	withDoctorExecSeams(t,
		func(name string) (string, error) {
			if name == "docker" {
				return "/usr/bin/docker", nil
			}
			return "", exec.ErrNotFound
		},
		func(context.Context, string, ...string) ([]byte, error) {
			return nil, &exec.ExitError{Stderr: []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock.\n")}
		})
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), nil, &doctorDockerDriver{})
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %+v", r.Findings)
	}
	f := r.Findings[0]
	if f.Code != proto.DoctorRuntimeUnavailable || f.Level != proto.DoctorLevelError || f.Recheck != "docker version" ||
		f.Probe != "docker version --format {{.Server.Version}}" || !strings.HasPrefix(f.Evidence[0].Value, "Cannot connect to the Docker daemon") {
		t.Fatalf("finding = %+v", f)
	}
	if !r.Runtimes[0].Selected || r.Runtimes[0].Status != proto.DoctorRuntimeQueryFailed {
		t.Fatalf("runtimes = %+v", r.Runtimes)
	}
}

// dcgmi prints an empty line before "dcgmi  version: 3.0.25" (seen on the
// PPU host); the version is what the report shows.
func TestDoctorRuntimeVersionIsTheVersionNumber(t *testing.T) {
	withDoctorRootDisk(t, 200<<30, 100<<30)
	withDoctorExecSeams(t, func(string) (string, error) { return "/usr/bin/x", nil },
		func(_ context.Context, name string, _ ...string) ([]byte, error) {
			return map[string][]byte{"docker": []byte("26.1.3\n"), "nerdctl": []byte("nerdctl version 2.1.3\n"), "python3": []byte("Python 3.7.9\n"), "dcgmi": []byte("\ndcgmi  version: 3.0.25\n")}[name], nil
		})
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), nil)
	got := map[string]string{}
	for _, rt := range r.Runtimes {
		got[rt.Name] = rt.Version
	}
	if !reflect.DeepEqual(got, map[string]string{"docker": "26.1.3", "nerdctl": "2.1.3", "python3": "3.7.9", "dcgmi": "3.0.25"}) {
		t.Fatalf("versions = %v", got)
	}
}

// Paths on one filesystem are one finding naming all of them (seen on the PPU
// host: /, the models root, the runveil tree and a storage all on one 195 GiB
// disk), and a path listed twice is listed once.
func TestDoctorReportGroupsLowDiskByFilesystem(t *testing.T) {
	withNoTools(t)
	orig := doctorFilesystemOf
	t.Cleanup(func() { doctorFilesystemOf = orig })
	doctorFilesystemOf = func(path string) (uint64, bool) {
		if strings.HasPrefix(path, "/var/lib/docker") {
			return 2, true
		}
		return 1, true
	}
	const total, low = 195 << 30, 13 << 30
	withDoctorRootDisk(t, total, low)
	r := buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), []proto.PathUsage{
		{Kind: "models", Path: "/opt/gpuworks/models", TotalBytes: total, AvailableBytes: low},
		{Kind: "udocker", Path: "/opt/gpuworks/udocker", TotalBytes: total, AvailableBytes: low},
		{Kind: "docker", Path: "/var/lib/docker", TotalBytes: 10 << 40, AvailableBytes: 2 << 40},
		{Kind: "storage", Path: "/opt/gpuworks/models", TotalBytes: total, AvailableBytes: low},
	})
	var disks []proto.DoctorFinding
	for _, f := range r.Findings {
		if f.Code == proto.DoctorDiskLow {
			disks = append(disks, f)
		}
	}
	if len(disks) != 1 {
		t.Fatalf("disk findings = %+v", disks)
	}
	f := disks[0]
	if f.Recheck != "df -h / /opt/gpuworks/models /opt/gpuworks/udocker" || f.Disk == nil || f.Disk.Path != "/" || f.Disk.AvailBytes != low {
		t.Fatalf("recheck = %q", f.Recheck)
	}
	if len(f.Evidence) != 1 || f.Evidence[0].Name != "paths" || f.Evidence[0].Value != "/, /opt/gpuworks/models, /opt/gpuworks/udocker" {
		t.Fatalf("evidence = %+v", f.Evidence)
	}
	count := 0
	for _, d := range r.Host.Disks {
		if d.Path == "/opt/gpuworks/models" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("models listed %d times: %+v", count, r.Host.Disks)
	}
}

// A node without torch skips the NCCL check instead of reporting a failed
// multi-GPU check (seen on gpu3).
func TestDoctorNCCLCheckSkipsWithoutTorch(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	if exec.Command("python3", "-c", "import torch").Run() == nil {
		t.Skip("torch is installed here; the skip path cannot be exercised")
	}
	if got := diagnosticNCCL(context.Background(), 2, t.TempDir()); got != "SKIP (torch is not installed)" {
		t.Fatalf("diagnosticNCCL = %q", got)
	}
	if diagnosticFailed("SKIP (torch is not installed)") {
		t.Fatal("a skip is reported as a failure")
	}
}

// passedThroughProber is a host whose only card is lent to a virtual
// machine: the vendor tool exits non-zero every time, with nothing to
// describe, and the PCI roster lists the card.
type passedThroughProber struct{ gpu.FixedProber }

func (p *passedThroughProber) Probe(context.Context) ([]proto.GPUInfo, error) {
	return []proto.GPUInfo{{Index: -1, Name: "K100_AI", Vendor: "hygon", PCIeBDF: "0000:67:00.0", Ownership: accelerator.CardPassedThrough}},
		errors.New("hy-smi: No device available")
}

// Doctor and the status report agree on such a host: the tool has no card
// to describe, so its exit status is the host's steady state and not an
// error. Doctor alone used to call it one on every run.
func TestDoctorDoesNotFaultAToolWithNoCardToDescribe(t *testing.T) {
	p := &passedThroughProber{gpu.FixedProber{VendorName: "hygon"}}
	r := proto.DoctorReport{}
	gpus := doctorAccelerators(context.Background(), &r, p)
	if len(gpus) != 1 || r.Accelerators == nil || len(r.Accelerators.GPUs) != 1 {
		t.Fatalf("cards = %+v, report = %+v, want the one card listed", gpus, r.Accelerators)
	}
	for _, f := range r.Findings {
		if f.Code == proto.DoctorAcceleratorProbeFailed {
			t.Fatalf("finding %+v on a host whose only card is passed through", f)
		}
	}
}
