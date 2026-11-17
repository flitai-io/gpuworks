package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// fakeDiagnosticTools puts nvidia-smi and python3 stand-ins first on PATH.
// Each records its call in the returned log; python3 answers as a passing
// check would.
func fakeDiagnosticTools(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	for name, body := range map[string]string{
		"nvidia-smi": "echo nvidia-smi >> " + log + "\necho 580.126.09\n",
		"python3":    "echo python3 >> " + log + "\necho PASS fake\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func readCalls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func stubCUDACheck(t *testing.T, result string) {
	t.Helper()
	orig := doctorCUDACheck
	doctorCUDACheck = func(context.Context) string { return result }
	t.Cleanup(func() { doctorCUDACheck = orig })
}

func fact(r proto.DoctorReport, code string) string {
	v, _ := factValue(r, code)
	return v
}

// On zstone-164 a card that failed to initialize holds every nvidia-smi on
// the driver lock for up to a minute and cuInit returns 101. Doctor then
// takes the driver version from the inventory instead of asking nvidia-smi
// again, and does not run peer access and NCCL on a driver the CUDA check
// already found unusable: together they ran the report past the server's
// wait.
func TestDoctorNVIDIAChecksSpareASlowBrokenDriver(t *testing.T) {
	log := fakeDiagnosticTools(t)
	stubCUDACheck(t, "FAIL: cuInit=101")
	r := proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{Vendor: "nvidia", Driver: "580.126.09"}}

	doctorNVIDIAChecks(context.Background(), &r, 4, t.TempDir())

	if calls := readCalls(t, log); len(calls) != 0 {
		t.Fatalf("doctor ran %v; want neither nvidia-smi nor the python checks", calls)
	}
	if fabric := fact(r, proto.DoctorFactFabricManager); !strings.HasPrefix(fabric, "driver=580.126.09 ") || !strings.Contains(fabric, "fabric_manager=") {
		t.Fatalf("fabric fact = %q", fabric)
	}
	for _, code := range []string{proto.DoctorFactPeerAccess, proto.DoctorFactNCCL} {
		if got := fact(r, code); got != cudaDependentSkip {
			t.Fatalf("%s = %q, want %q", code, got, cudaDependentSkip)
		}
	}
	var codes []string
	for _, f := range r.Findings {
		codes = append(codes, f.Code)
	}
	// The CUDA failure, and the two checks it held back, each skipped with
	// why: not run is not passed.
	if want := []string{proto.DoctorCUDAInitFailed, proto.DoctorSkipCUDANotPassed, proto.DoctorSkipCUDANotPassed}; !reflect.DeepEqual(codes, want) {
		t.Fatalf("findings = %v, want %v", codes, want)
	}
	if r.Findings[1].ID != proto.DoctorCheckPeerAccess || r.Findings[2].ID != proto.DoctorCheckNCCL || r.Findings[1].Status != proto.CheckSkip {
		t.Fatalf("skipped checks = %+v", r.Findings[1:])
	}
}

// The gate only spares a driver the CUDA check failed on: after a pass the
// multi-GPU checks run, and with one card they still say why they skip.
func TestDoctorNVIDIAChecksRunMultiGPUChecksAfterCUDAPasses(t *testing.T) {
	log := fakeDiagnosticTools(t)
	stubCUDACheck(t, "PASS cuInit=ok")
	r := proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{Vendor: "nvidia", Driver: "580.126.09"}}
	doctorNVIDIAChecks(context.Background(), &r, 2, t.TempDir())
	if calls := readCalls(t, log); len(calls) != 2 || calls[0] != "python3" || calls[1] != "python3" {
		t.Fatalf("calls = %v, want the peer access and NCCL checks", calls)
	}
	if got := fact(r, proto.DoctorFactPeerAccess); got != "PASS fake" {
		t.Fatalf("peer access = %q", got)
	}

	stubCUDACheck(t, "FAIL: cuInit=101")
	r = proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{Vendor: "nvidia"}}
	doctorNVIDIAChecks(context.Background(), &r, 1, t.TempDir())
	if got := fact(r, proto.DoctorFactPeerAccess); got != "SKIP (fewer than 2 GPUs)" {
		t.Fatalf("one-card peer access = %q", got)
	}
	// One card has nothing for a multi-GPU check to look at: not
	// applicable, so no skip is reported for them.
	for _, f := range r.Findings {
		if f.ID == proto.DoctorCheckPeerAccess || f.ID == proto.DoctorCheckNCCL {
			t.Fatalf("a one-card host reports a multi-GPU finding: %+v", f)
		}
	}
}

// A CUDA check that cannot run on this host, and a multi-GPU check whose
// script says it cannot (no torch), are skips with why, never passes.
func TestDoctorNVIDIAChecksThatCannotRunAreSkipped(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'SKIP (torch is not installed)'\n"
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stubCUDACheck(t, "SKIP (python3 is not installed; the periodic per-card runtime check does not run either)")
	r := proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{Vendor: "nvidia"}}
	doctorNVIDIAChecks(context.Background(), &r, 2, t.TempDir())
	got := map[string]proto.DoctorFinding{}
	for _, f := range r.Findings {
		got[f.ID] = f
	}
	cuda := got[proto.DoctorCheckCUDAInit]
	if cuda.Status != proto.CheckSkip || cuda.Code != proto.DoctorSkipPrerequisiteMissing || !strings.Contains(cuda.Evidence[0].Value, "python3 is not installed") {
		t.Fatalf("cuda = %+v", cuda)
	}
	// The CUDA check did not pass, so the multi-GPU checks are held back
	// for that reason, not for their own.
	for _, id := range []string{proto.DoctorCheckPeerAccess, proto.DoctorCheckNCCL} {
		if f := got[id]; f.Status != proto.CheckSkip || f.Code != proto.DoctorSkipCUDANotPassed {
			t.Fatalf("%s = %+v", id, f)
		}
	}

	stubCUDACheck(t, "PASS cuInit=ok")
	r = proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{Vendor: "nvidia"}}
	doctorNVIDIAChecks(context.Background(), &r, 2, t.TempDir())
	got = map[string]proto.DoctorFinding{}
	for _, f := range r.Findings {
		got[f.ID] = f
	}
	if f := got[proto.DoctorCheckNCCL]; f.Status != proto.CheckSkip || f.Code != proto.DoctorSkipPrerequisiteMissing || f.Evidence[0].Value != "SKIP (torch is not installed)" {
		t.Fatalf("nccl = %+v, all = %+v", f, r.Findings)
	}
}

// TestRunPythonDiagnosticUsesTheHostCommandBuilder: the doctor's python3
// diagnostics must be placed the same way a vendor CLI runs (gpu.HostCommand),
// not a raw exec.CommandContext, because a Kubernetes DaemonSet pod's image
// has no python3 of its own.
func TestRunPythonDiagnosticUsesTheHostCommandBuilder(t *testing.T) {
	var gotName string
	var gotArgs []string
	old := pythonCommand
	pythonCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotName, gotArgs = name, args
		return exec.CommandContext(ctx, "true")
	}
	t.Cleanup(func() { pythonCommand = old })

	runPythonDiagnostic(context.Background(), "print(1)")
	if gotName != "python3" || len(gotArgs) != 2 || gotArgs[0] != "-c" || gotArgs[1] != "print(1)" {
		t.Fatalf("pythonCommand called with %q %v, want python3 -c <script>", gotName, gotArgs)
	}
}

// TestRunPythonFileDiagnosticChoosesItsDirectory: the NCCL check's script
// file goes under the state directory when CLIs run on the host (the same
// path there as in the agent's own container, which os.TempDir() is not),
// and under os.TempDir() otherwise or when no state directory is offered.
func TestRunPythonFileDiagnosticChoosesItsDirectory(t *testing.T) {
	var gotDir string
	oldCreate := createDiagnosticScript
	createDiagnosticScript = func(dir, pattern string) (*os.File, error) {
		gotDir = dir
		return os.CreateTemp(dir, pattern)
	}
	t.Cleanup(func() { createDiagnosticScript = oldCreate })
	oldPython := pythonCommand
	pythonCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}
	t.Cleanup(func() { pythonCommand = oldPython })

	t.Run("off the host: os.TempDir() regardless of a given state dir", func(t *testing.T) {
		gotDir = ""
		oldOnHost := cliOnHostForDoctor
		cliOnHostForDoctor = func() bool { return false }
		t.Cleanup(func() { cliOnHostForDoctor = oldOnHost })
		runPythonFileDiagnostic(context.Background(), t.TempDir(), "print(1)")
		if gotDir != os.TempDir() {
			t.Fatalf("dir = %q, want os.TempDir() (%q)", gotDir, os.TempDir())
		}
	})

	t.Run("on the host with no state dir offered: os.TempDir()", func(t *testing.T) {
		gotDir = ""
		oldOnHost := cliOnHostForDoctor
		cliOnHostForDoctor = func() bool { return true }
		t.Cleanup(func() { cliOnHostForDoctor = oldOnHost })
		runPythonFileDiagnostic(context.Background(), "", "print(1)")
		if gotDir != os.TempDir() {
			t.Fatalf("dir = %q, want os.TempDir() (%q)", gotDir, os.TempDir())
		}
	})

	t.Run("on the host with a state dir: the state dir itself", func(t *testing.T) {
		gotDir = ""
		oldOnHost := cliOnHostForDoctor
		cliOnHostForDoctor = func() bool { return true }
		t.Cleanup(func() { cliOnHostForDoctor = oldOnHost })
		state := t.TempDir()
		runPythonFileDiagnostic(context.Background(), state, "print(1)")
		if gotDir != state {
			t.Fatalf("dir = %q, want the state directory %q", gotDir, state)
		}
	})
}

// deadlineProber records the deadline doctor probes under.
type deadlineProber struct {
	gpu.FixedProber
	deadline time.Time
	ok       bool
}

func (p *deadlineProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	p.deadline, p.ok = ctx.Deadline()
	return nil, nil
}

// A doctor the server asked for ends within its own budget, so the report
// reaches the server before the server stops waiting for it.
func TestDoctorRPCRunsWithinItsBudget(t *testing.T) {
	p := &deadlineProber{}
	n := &NodeRuntime{prober: p}
	start := time.Now()
	result, err := n.OnRPC(context.Background(), proto.RPCNodeDoctor, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.(proto.DoctorReport); !ok {
		t.Fatalf("result = %T", result)
	}
	end := time.Now()
	if !p.ok || p.deadline.Before(start.Add(doctorBudget)) || p.deadline.After(end.Add(doctorBudget)) {
		t.Fatalf("probe deadline = %v after the call (set %v), want %s", p.deadline.Sub(start), p.ok, doctorBudget)
	}
}
