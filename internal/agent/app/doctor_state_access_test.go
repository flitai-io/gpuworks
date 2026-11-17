package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// stateAccessDriver is a runveil-like driver whose run identity is a fixture;
// its verdict comes from the real runtime.CheckAncestorTraversal on real
// directories, the call the runtime itself makes.
type stateAccessDriver struct {
	imagePullTestDriver
	stateDir string
	owner    runtime.MountOwner
	reads    int
}

func (*stateAccessDriver) Name() string { return "udocker" }

func (d *stateAccessDriver) StateAccessProblem() (string, *runtime.TraversalError) {
	d.reads++
	var blocked *runtime.TraversalError
	if errors.As(runtime.CheckAncestorTraversal(filepath.Dir(d.stateDir), d.owner), &blocked) {
		return d.stateDir, blocked
	}
	return d.stateDir, nil
}

// privateHome lays out /tmp/x/home (0700, the operator's private directory)
// holding the agent state directory. The runtime identity is another uid, so
// the test needs no root.
func privateHome(t *testing.T) (stateDir, home string, owner runtime.MountOwner) {
	t.Helper()
	outer, err := os.MkdirTemp("/tmp", "gpuworks-doctor-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outer) })
	if err := os.Chmod(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(outer, "home")
	stateDir = filepath.Join(home, "gpuworks")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return stateDir, home, runtime.MountOwner{UID: os.Geteuid() + 1000, GID: os.Getegid() + 1000, Username: "udocker"}
}

// The field failure: the state directory sits under a 0700 directory, every
// launch fails with permission denied on the runtime's PRoot, and the doctor
// used to read clean.
func TestDoctorReportsAnAncestorOfTheStateDirectoryTheRuntimeUserCannotSearch(t *testing.T) {
	stateDir, home, owner := privateHome(t)
	driver := &stateAccessDriver{stateDir: stateDir, owner: owner}
	r := doctorWith(t, driver)

	found := findingsByCode(r, proto.DoctorRuntimeStateUntraversable)
	if len(found) != 1 {
		t.Fatalf("findings = %+v", r.Findings)
	}
	f := found[0]
	if f.Status != proto.CheckFail {
		t.Fatalf("level = %s, want error: workloads cannot launch", f.Level)
	}
	want := map[string]string{
		"directory": home, "mode": "0700", "owner": strings.Join([]string{itoa(os.Geteuid()), itoa(os.Getegid())}, ":"),
		"user": "udocker", "state_dir": stateDir, "fix": "chmod o+x " + home,
	}
	got := map[string]string{}
	for _, e := range f.Evidence {
		got[e.Name] = e.Value
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("evidence %s = %q, want %q (all: %+v)", name, got[name], value, f.Evidence)
		}
	}
	if f.Recheck != "namei -m "+stateDir || !strings.Contains(f.Probe, stateDir) || !strings.Contains(f.Probe, "udocker") {
		t.Fatalf("probe %q, recheck %q", f.Probe, f.Recheck)
	}
	// The text report (the CLI and agents without the structured form) says
	// the same, with the blocking directory and the fix.
	for _, w := range []string{"[fail] RUNTIME_STATE_UNTRAVERSABLE", "directory = " + home, "fix = chmod o+x " + home, "namei -m " + stateDir, "every launch fails with permission denied"} {
		if !strings.Contains(r.Report, w) {
			t.Errorf("report lacks %q:\n%s", w, r.Report)
		}
	}
	// Errors come first, as for every other finding.
	if r.Findings[0].Code != proto.DoctorRuntimeStateUntraversable {
		t.Fatalf("order = %+v", r.Findings)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Opening the directory ends the finding on the next run, with no restart.
func TestDoctorStateAccessClearsOnceTheDirectoryIsOpened(t *testing.T) {
	stateDir, home, owner := privateHome(t)
	driver := &stateAccessDriver{stateDir: stateDir, owner: owner}
	if r := doctorWith(t, driver); len(findingsByCode(r, proto.DoctorRuntimeStateUntraversable)) != 1 {
		t.Fatalf("not reported while closed: %+v", r.Findings)
	}
	if err := os.Chmod(home, 0o711); err != nil {
		t.Fatal(err)
	}
	r := doctorWith(t, driver)
	if got := findingsByCode(r, proto.DoctorRuntimeStateUntraversable); len(got) != 0 {
		t.Fatalf("still reported after chmod o+x: %+v", got)
	}
	if strings.Contains(r.Report, "RUNTIME_STATE_UNTRAVERSABLE") {
		t.Fatalf("report still names it:\n%s", r.Report)
	}
	if driver.reads != 2 {
		t.Fatalf("reads = %d: the doctor must ask on every run", driver.reads)
	}
}

// A runtime that does not launch as another identity (docker, a non-root
// agent's runveil) has nothing to report.
func TestDoctorStateAccessIsSilentForRuntimesWithoutTheCondition(t *testing.T) {
	stateDir, _, _ := privateHome(t)
	// Same directories, but the workloads run as the agent's own user.
	own := runtime.MountOwner{UID: os.Geteuid(), GID: os.Getegid()}
	r := doctorWith(t, &stateAccessDriver{stateDir: stateDir, owner: own})
	if got := findingsByCode(r, proto.DoctorRuntimeStateUntraversable); len(got) != 0 {
		t.Fatalf("findings = %+v", got)
	}
	// A driver without the capability.
	r = doctorWith(t, &imagePullTestDriver{})
	if got := findingsByCode(r, proto.DoctorRuntimeStateUntraversable); len(got) != 0 {
		t.Fatalf("findings = %+v", got)
	}
}

type fixedStateAccess struct{ stateDir string }

func (f fixedStateAccess) StateAccessProblem() (string, *runtime.TraversalError) {
	return f.stateDir, &runtime.TraversalError{Dir: "/root", Reason: "is not traversable", Owner: runtime.MountOwner{UID: 1001, GID: 1001, Username: "udocker"}, Mode: 0o700}
}

// `gpuworks-agent doctor` runs beside the agent and has no driver: the state
// directory it is pointed at says whether the runtime was ever set up there.
func TestCLIDoctorChecksTheStateDirectoryOfANodeThatUsesTheRuntime(t *testing.T) {
	withNoTools(t)
	state := t.TempDir()
	var asked []string
	saved := doctorRuntimeAccess
	doctorRuntimeAccess = func(dir string) runtime.StateAccessReporter {
		asked = append(asked, dir)
		return fixedStateAccess{stateDir: dir}
	}
	t.Cleanup(func() { doctorRuntimeAccess = saved })

	// No udocker directory: the runtime was never used here, nothing to judge.
	r := doctorReportForState(context.Background(), doctorFixedNonNVIDIAProber(), state)
	if len(asked) != 0 || len(findingsByCode(r, proto.DoctorRuntimeStateUntraversable)) != 0 {
		t.Fatalf("judged a node that never used the runtime: asked %v, findings %+v", asked, r.Findings)
	}

	if err := os.Mkdir(filepath.Join(state, "udocker"), 0o755); err != nil {
		t.Fatal(err)
	}
	r = doctorReportForState(context.Background(), doctorFixedNonNVIDIAProber(), state)
	found := findingsByCode(r, proto.DoctorRuntimeStateUntraversable)
	if len(asked) != 1 || asked[0] != state || len(found) != 1 || found[0].Recheck != "namei -m "+state {
		t.Fatalf("asked %v, findings %+v", asked, r.Findings)
	}
	if !strings.Contains(r.Report, "RUNTIME_STATE_UNTRAVERSABLE") || !strings.Contains(r.Report, "fix = chmod o+x /root") {
		t.Fatalf("report:\n%s", r.Report)
	}
}

// The real runtime, asked about a state directory with the runtime's directory
// in it, only reads.
func TestCLIDoctorWithTheRealRuntimeReadsOnly(t *testing.T) {
	withNoTools(t)
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "udocker"), 0o755); err != nil {
		t.Fatal(err)
	}
	doctorReportForState(context.Background(), doctorFixedNonNVIDIAProber(), state)
	entries, _ := os.ReadDir(state)
	if len(entries) != 1 {
		t.Fatalf("doctor changed the state directory: %v", entries)
	}
}

// The structured form is what the node page and the diagnose button read.
func TestDoctorStateAccessWireForm(t *testing.T) {
	stateDir, home, owner := privateHome(t)
	r := doctorWith(t, &stateAccessDriver{stateDir: stateDir, owner: owner})
	f := findingsByCode(r, proto.DoctorRuntimeStateUntraversable)[0]
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"runtime_state_access","status":"fail","code":"RUNTIME_STATE_UNTRAVERSABLE",` +
		`"probe":"search permission on every directory above ` + stateDir + ` for udocker (uid ` + itoa(owner.UID) + `, gid ` + itoa(owner.GID) + `)",` +
		`"evidence":[{"name":"directory","value":"` + home + `"},{"name":"mode","value":"0700"},{"name":"owner","value":"` + itoa(os.Geteuid()) + `:` + itoa(os.Getegid()) + `"},` +
		`{"name":"user","value":"udocker"},{"name":"state_dir","value":"` + stateDir + `"},{"name":"fix","value":"chmod o+x ` + home + `"}],` +
		`"recheck":"namei -m ` + stateDir + `"}`
	if string(out) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", out, want)
	}
}
