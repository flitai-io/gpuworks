package provision

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const localDiskMounts = "/dev/sda1 %DIR% ext4 rw,relatime 0 0"

// blockedScenario is a node where every link of the chain holds: a root agent,
// no docker or nerdctl, the udocker account present, the filesystem accepting
// chown, and the install directory under a 0700 ancestor.
func blockedScenario(dir string) dirScenario {
	return dirScenario{
		mounts: localDiskMounts, runtimeID: "1001", useradd: true,
		modes: map[string]string{filepath.Dir(dir): "700 0 0"},
	}
}

// A root agent runs workloads as the udocker user; an install directory under
// a private ancestor (/root, a 0700 home) is invisible to it, and every launch
// then fails with `fork/exec .../proot-...: permission denied`.
func TestInstallDirTraversalBlockedByPrivateAncestor(t *testing.T) {
	dir := realTempDir(t)
	private := filepath.Dir(dir)
	out := probeScenario(t, blockedScenario(dir), dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "fail",
		private+" (mode 700, owner 0:0)", "udocker, uid 1001", "permission denied",
		"chmod o+x "+private, "choose an install directory outside it, such as /opt/gpuworks")
	if !out.Untraversable || out.Noexec {
		t.Fatalf("outcome = %+v", out)
	}
	wantCheck(t, out.Checks, CheckIDInstallDirOwnership, "pass")
	if !errors.Is(out.blockingError(), ErrInstallDirUntraversable) || !strings.Contains(out.blockingError().Error(), "chmod o+x "+private) {
		t.Fatalf("blocking error = %v", out.blockingError())
	}
}

// Every branch of the verdict. The install stops only on an error, and an
// error needs the whole chain; each missing link is a warning that names it.
func TestInstallDirTraversalVerdictTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*dirScenario)
		runtime string // the runtime the install names
		status  string
		blocks  bool
		detail  string
	}{
		{name: "nothing else answers: runveil is the pick", status: "fail", blocks: true},
		{name: "explicit udocker", runtime: "udocker", status: "fail", blocks: true},
		{name: "explicit auto", runtime: "auto", status: "fail", blocks: true},
		{name: "explicit udocker beats a docker that answers", runtime: "udocker", mutate: func(s *dirScenario) { s.docker = true }, status: "fail", blocks: true},
		{name: "explicit udocker beats a nerdctl that answers", runtime: "udocker", mutate: func(s *dirScenario) { s.nerdctl = true }, status: "fail", blocks: true},
		{name: "docker answers", mutate: func(s *dirScenario) { s.docker = true }, status: "warn", detail: "this node has docker"},
		{name: "nerdctl answers without docker (Ascend k8s node)", mutate: func(s *dirScenario) { s.nerdctl = true }, status: "warn", detail: "this node has nerdctl"},
		{name: "docker and nerdctl answer", mutate: func(s *dirScenario) { s.docker, s.nerdctl = true, true }, status: "warn", detail: "this node has docker"},
		{name: "explicit docker", runtime: "docker", status: "warn", detail: "selects the docker runtime"},
		{name: "explicit nerdctl", runtime: "nerdctl", status: "warn", detail: "selects the nerdctl runtime"},
		{name: "the filesystem refuses chown: workloads run as the agent", mutate: func(s *dirScenario) { s.chownErr = true }, status: "na", detail: "not needed: workloads run as the agent's own user"},
		{name: "account missing but useradd can create it", mutate: func(s *dirScenario) { s.runtimeID = "" }, status: "fail", blocks: true, detail: "created at install"},
		{name: "account missing and no useradd: the fallback to the agent's user", mutate: func(s *dirScenario) { s.runtimeID, s.useradd = "", false }, status: "warn", detail: "no useradd"},
		{name: "unsupported architecture has no runveil", mutate: func(s *dirScenario) { s.arch = "riscv64" }, status: "warn", detail: "does not support the riscv64 architecture"},
		{name: "arm64 has the embedded PRoot", mutate: func(s *dirScenario) { s.arch = "aarch64" }, status: "fail", blocks: true},
		{name: "not root and no sudo: the agent runs as that user", mutate: func(s *dirScenario) { s.uid, s.user, s.useradd = "1000", "ubuntu", false }, status: "na", detail: "ubuntu is not root"},
		// Under systemd the unit runs as root, under nohup the script starts
		// the agent as the SSH user: the probe cannot tell which this host gets.
		{name: "passwordless sudo: root only under systemd, so not certain", mutate: func(s *dirScenario) { s.uid, s.user, s.sudo = "1000", "ubuntu", true }, status: "warn", detail: "a nohup install runs as ubuntu"},
		{name: "an ACL on the closed directory may let the user in", mutate: func(s *dirScenario) { s.aclDir = "%ANCESTOR%" }, status: "warn", detail: "may carry an ACL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := realTempDir(t)
			sc := blockedScenario(dir)
			if tc.mutate != nil {
				tc.mutate(&sc)
			}
			sc.aclDir = strings.ReplaceAll(sc.aclDir, "%ANCESTOR%", filepath.Dir(dir))
			out := probeScenarioRuntime(t, sc, dir, dir, "vm", tc.runtime)
			c := checkByName(t, out.Checks, CheckIDInstallDirTraversal)
			if c.Status != tc.status || out.Untraversable != tc.blocks {
				t.Fatalf("status %s, blocks %v; want %s, %v: %s", c.Status, out.Untraversable, tc.status, tc.blocks, c.text())
			}
			if tc.detail != "" && !strings.Contains(c.text(), tc.detail) {
				t.Fatalf("detail lacks %q: %s", tc.detail, c.text())
			}
			if tc.status == "warn" && !strings.Contains(c.text(), "not stopping the install") {
				t.Fatalf("a warning must say why it does not stop the install: %s", c.text())
			}
			if (out.blockingError() != nil) != tc.blocks {
				t.Fatalf("blocking error = %v", out.blockingError())
			}
		})
	}
}

// Facts the probe could not establish are never an error.
func TestInstallDirTraversalVerdictOnFactsTheProbeCouldNotEstablish(t *testing.T) {
	base := installDirFacts{
		requested: "/root/gpuworks", uid: "0", user: "root", chown: "ok", ancestors: "blocked", runtimeUID: "1001",
		ancestorDir: "/root", ancestorMode: "700", ancestorUID: "0", ancestorGID: "0", ancestorACL: "no", arch: "x86_64",
	}
	if v := installDirTraversalVerdict(base); !v.blocks || v.status != "fail" {
		t.Fatalf("complete chain = %+v", v)
	}
	untested := base
	untested.chown = "skipped"
	if v := installDirTraversalVerdict(untested); v.blocks || v.status != "warn" || !strings.Contains(v.detail, "was not tested here") {
		t.Fatalf("chown not tested = %+v", v)
	}
	notRoot := base
	notRoot.uid, notRoot.user = "1000", "ubuntu"
	if v := installDirTraversalVerdict(notRoot); v.blocks || v.status != "warn" {
		t.Fatalf("not root, no sudo = %+v", v)
	}
	sudoOnly := base
	sudoOnly.uid, sudoOnly.user, sudoOnly.sudo = "1000", "ubuntu", true
	if v := installDirTraversalVerdict(sudoOnly); v.blocks || v.status != "warn" || !strings.Contains(v.detail, "nohup") {
		t.Fatalf("not root, passwordless sudo = %+v", v)
	}
	for _, acl := range []string{"yes", "unknown", ""} {
		aclUnsure := base
		aclUnsure.ancestorACL = acl
		if v := installDirTraversalVerdict(aclUnsure); v.blocks || v.status != "warn" || !strings.Contains(v.detail, "ACL") {
			t.Fatalf("ACL %q = %+v", acl, v)
		}
	}
	unreadable := base
	unreadable.ancestors = "unknown"
	if v := installDirTraversalVerdict(unreadable); v.blocks || v.status != "skip" || !strings.Contains(v.detail, "not checked") {
		t.Fatalf("unreadable = %+v", v)
	}
	mac := base
	mac.mac = true
	if v := installDirTraversalVerdict(mac); v.blocks || v.status != "na" {
		t.Fatalf("mac = %+v", v)
	}
}

// The blocked install: nothing is uploaded or run after the probe, and the
// report carries the row with the reason.
func TestInstallSSHStopsBeforeUploadOnUntraversableInstallDirectory(t *testing.T) {
	dir := realTempDir(t)
	run := runInstallNamingRuntime(t, blockedScenario(dir), dir, "")
	if !errors.Is(run.err, ErrInstallDirUntraversable) {
		t.Fatalf("install error = %v", run.err)
	}
	for _, want := range []string{"install directory path access", "chmod o+x " + filepath.Dir(dir), "permission denied"} {
		if !strings.Contains(run.err.Error(), want) {
			t.Fatalf("install error lacks %q: %v", want, run.err)
		}
	}
	wantCheck(t, run.report.Checks, CheckIDInstallDirTraversal, "fail", filepath.Dir(dir))
	assertStoppedAtTheProbe(t, run)
}

// Where the runtime is not certain the install goes on, reporting a warning:
// it got as far as reading the agent binary.
func TestInstallSSHGoesOnWhenTheRuntimeIsNotCertain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*dirScenario)
		runtime string
	}{
		{name: "docker", mutate: func(s *dirScenario) { s.docker = true }},
		{name: "nerdctl", mutate: func(s *dirScenario) { s.nerdctl = true }},
		{name: "explicit docker", runtime: "docker"},
		{name: "ownership refused", mutate: func(s *dirScenario) { s.chownErr = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := realTempDir(t)
			sc := blockedScenario(dir)
			if tc.mutate != nil {
				tc.mutate(&sc)
			}
			run := runInstallNamingRuntime(t, sc, dir, tc.runtime)
			if run.err == nil || !strings.Contains(run.err.Error(), "agent binary is not available") {
				t.Fatalf("install error = %v", run.err)
			}
		})
	}
	// An explicit udocker is certain even beside a docker that answers.
	dir := realTempDir(t)
	sc := blockedScenario(dir)
	sc.docker = true
	run := runInstallNamingRuntime(t, sc, dir, "udocker")
	if !errors.Is(run.err, ErrInstallDirUntraversable) {
		t.Fatalf("install error = %v", run.err)
	}
	assertStoppedAtTheProbe(t, run)
}

// The precheck reports the same verdict and never stops anything.
func TestPrecheckReportsTheSameVerdictAsTheInstall(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenarioRuntime(t, blockedScenario(dir), dir, dir, "vm", "udocker")
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "fail")
}

func TestInstallDirTraversalQuotesAnAwkwardDirectoryInTheFix(t *testing.T) {
	v := installDirTraversalVerdict(installDirFacts{
		requested: "/home/my user/gpuworks", uid: "0", chown: "ok", ancestors: "blocked", ancestorDir: "/home/my user",
		ancestorMode: "700", ancestorUID: "1000", ancestorGID: "1000", ancestorACL: "no", runtimeUID: "1001", arch: "x86_64",
	})
	if v.status != "fail" || !strings.Contains(v.detail, "`chmod o+x '/home/my user'`") {
		t.Fatalf("verdict = %+v", v)
	}
}

// Search for the runtime user comes from exactly one class, the way the
// kernel picks it: its uid, else its primary group, else others.
func TestInstallDirTraversalPicksTheClassTheKernelPicks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runtimeID string
		mode      string
		blocked   bool
	}{
		{"others have search", "1001", "701 0 0", false},
		{"others have only read and write", "1001", "706 0 0", true},
		{"group is the runtime user's and has search", "1001", "710 0 1001", false},
		{"group is the runtime user's and lacks search", "1001", "760 0 1001", true},
		{"another group has search, others do not", "1001", "710 0 0", true},
		{"owner class applies to the runtime user alone", "1001", "100 1001 1001", false},
		{"owner class blocks even where others would pass", "1001", "007 1001 1001", true},
		{"sticky world-writable /tmp", "1001", "1777 0 0", false},
		{"no account yet: only others count, closed", "", "750 0 0", true},
		{"no account yet: only others count, open", "", "751 0 0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := realTempDir(t)
			sc := blockedScenario(dir)
			sc.runtimeID = tc.runtimeID
			sc.modes = map[string]string{filepath.Dir(dir): tc.mode}
			out := probeScenario(t, sc, dir, dir, "vm")
			if tc.blocked {
				wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "fail", "mode "+strings.Fields(tc.mode)[0])
			} else {
				wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "pass", "can be searched")
			}
		})
	}
}

// The walk goes to the top: the blocking directory is named wherever it is,
// and the nearest closed one is the one reported.
func TestInstallDirTraversalNamesTheNearestClosedAncestor(t *testing.T) {
	dir := realTempDir(t)
	parent := filepath.Dir(dir)
	grand := filepath.Dir(parent)
	sc := blockedScenario(dir)
	sc.modes = map[string]string{grand: "700 0 0", parent: "750 0 0"}
	out := probeScenario(t, sc, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "fail", parent+" (mode 750")
	if strings.Contains(checkByName(t, out.Checks, CheckIDInstallDirTraversal).text(), grand+" (mode") {
		t.Fatalf("reported the farther directory: %+v", out.Checks)
	}
	sc.modes = map[string]string{grand: "700 0 0", parent: "755 0 0"}
	out = probeScenario(t, sc, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "fail", grand+" (mode 700")
}

// The install directory itself is the installer's: it opens it (0711), so its
// own mode says nothing.
func TestInstallDirTraversalDoesNotJudgeTheInstallDirectoryItself(t *testing.T) {
	dir := realTempDir(t)
	sc := blockedScenario(dir)
	sc.modes = map[string]string{dir: "700 0 0"}
	out := probeScenario(t, sc, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "pass", "can be searched")
	if out.Untraversable {
		t.Fatal("the install directory's own mode stopped the install")
	}
}

// A directory that does not exist yet is created under the nearest existing
// one, which is then the first that must be open.
func TestInstallDirTraversalOfADirectoryNotCreatedYet(t *testing.T) {
	parent := realTempDir(t)
	requested := filepath.Join(parent, "models", "gpuworks")
	sc := blockedScenario(parent)
	sc.modes = map[string]string{parent: "700 0 0"}
	out := probeScenario(t, sc, parent, requested, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "fail", parent+" (mode 700", "chmod o+x "+parent)
}

// Passwordless sudo makes the agent root, and stat runs with its privileges
// because the SSH user may be unable to look inside a private home.
func TestInstallDirTraversalNotNeededWithoutRoot(t *testing.T) {
	dir := realTempDir(t)
	sc := blockedScenario(dir)
	sc.uid, sc.user = "1000", "ubuntu"
	out := probeScenario(t, sc, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "na", "not needed", "ubuntu is not root")
}

// A stat that fails says nothing about the node: reported as not checked, not
// as a verdict, and never a block.
func TestInstallDirTraversalUnreadableAncestorIsNotAVerdict(t *testing.T) {
	dir := realTempDir(t)
	sc := blockedScenario(dir)
	sc.modes = map[string]string{filepath.Dir(dir): "unreadable"}
	out := probeScenario(t, sc, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "skip", "not checked", "could not be read")
	if out.Untraversable {
		t.Fatal("an unreadable ancestor stopped the install")
	}
}

func TestInstallDirTraversalNotNeededOnAMac(t *testing.T) {
	c := installDirTraversalCheck(installDirFacts{mac: true, ancestors: "blocked", ancestorDir: "/Users/x"})
	if c.Status != "na" || !strings.Contains(c.text(), "not needed on a Mac") {
		t.Fatalf("check = %+v", c)
	}
}

func TestParseInstallDirFactsReadsTheAncestorAndRuntimeFacts(t *testing.T) {
	f, err := parseInstallDirFacts("probed=/root\nchown=ok\nruntime_uid=1001\nuseradd=yes\nnerdctl=yes\narch=aarch64\nancestors=blocked\nancestor_dir=/root\nancestor_mode=700\nancestor_uid=0\nancestor_gid=0\nancestor_acl=no\n")
	if err != nil {
		t.Fatal(err)
	}
	if f.ancestors != "blocked" || f.ancestorDir != "/root" || f.ancestorMode != "700" || f.ancestorUID != "0" || f.ancestorGID != "0" || f.ancestorACL != "no" ||
		f.runtimeUID != "1001" || !f.useradd || !f.nerdctl || f.arch != "aarch64" {
		t.Fatalf("facts = %+v", f)
	}
}

// The kernel counts the user's supplementary groups (the device groups the
// installer adds it to) as its group class too.
func TestInstallDirTraversalCountsSupplementaryGroups(t *testing.T) {
	dir := realTempDir(t)
	sc := blockedScenario(dir)
	sc.modes = map[string]string{filepath.Dir(dir): "710 0 44"}
	out := probeScenario(t, sc, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "fail", "mode 710")
	sc.groups = "1001 44 109"
	out = probeScenario(t, sc, dir, dir, "vm")
	wantCheck(t, out.Checks, CheckIDInstallDirTraversal, "pass", "can be searched")
	if out.Untraversable {
		t.Fatal("a directory the user reaches through a supplementary group stopped the install")
	}
}

// The blocking error carries what the API turns into a stable code.
func TestInstallDirUntraversableErrorCarriesTheFacts(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, blockedScenario(dir), dir, dir, "vm")
	var blocked *InstallDirUntraversableError
	if !errors.As(out.blockingError(), &blocked) {
		t.Fatalf("blocking error = %v", out.blockingError())
	}
	if blocked.InstallDir != dir || blocked.Ancestor != filepath.Dir(dir) || blocked.Mode != "700" || blocked.FixCommand() != "chmod o+x "+filepath.Dir(dir) {
		t.Fatalf("error = %+v", blocked)
	}
	if !errors.Is(blocked, ErrInstallDirUntraversable) {
		t.Fatal("not an ErrInstallDirUntraversable")
	}
}

// Out-of-band recovery reinstalls an agent that already runs from the
// directory: bringing it back online matters more than workloads that cannot
// launch, so the same closed ancestor does not stop it.
func TestInstallSSHKeepsReinstallingAnAgentUnderAClosedAncestor(t *testing.T) {
	dir := realTempDir(t)
	inner := scenarioHandler(t, blockedScenario(dir), dir)
	host, port := startFakeSSHServer(t, "root", "pw", inner)
	installer := NewSSHInstaller(func(string) ([]byte, error) { return nil, errors.New("agent binary is not available in this test") })
	report, err := installer.InstallSSH(t.Context(), SSHInstallRequest{
		Target: "root@" + host, Port: port, ServerURL: "https://server.example", Token: "tok", InstallDir: dir,
		InstanceKind: "container", RuntimeDriver: "udocker", KeepSupervisor: true, KeepUntraversableInstallDir: true,
		SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
	})
	if err == nil || !strings.Contains(err.Error(), "agent binary is not available") {
		t.Fatalf("install error = %v", err)
	}
	wantCheck(t, report.Checks, CheckIDInstallDirTraversal, "warn", "not stopping the reinstall")
}
