package launchd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeLaunchd answers launchctl the way macOS 15 does. bootout releases a
// running job only after the next print (launchd lets go asynchronously),
// and a bootstrap of a label still held fails with "5: Input/output error".
type fakeLaunchd struct {
	sessions    map[string]bool   // gui/<uid> domains with a login session
	programs    map[string]string // service -> executable from bootstrap
	loaded      map[string]int    // service -> pid
	releasing   map[string]bool
	refuse      int    // bootstraps to refuse before accepting
	refuseOf    string // refuse every bootstrap of a plist containing this
	crashLoop   bool   // the job's pid changes on every look
	nextPID     int
	calls       []string
	bootstraped []string // plists, in order
}

type fakeLaunchctlExit int

func (e fakeLaunchctlExit) Error() string { return fmt.Sprintf("exit status %d", e) }
func (e fakeLaunchctlExit) ExitCode() int { return int(e) }

func newFakeLaunchd() *fakeLaunchd {
	return &fakeLaunchd{sessions: map[string]bool{"gui/501": true}, loaded: map[string]int{}, programs: map[string]string{}, releasing: map[string]bool{}, nextPID: 700}
}

func (f *fakeLaunchd) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	fail := func(code int, msg string) (string, error) { return msg, fakeLaunchctlExit(code) }
	switch args[0] {
	case "print":
		target := args[1]
		if !strings.Contains(target, "/"+AgentLabel) {
			if target == "system" || f.sessions[target] {
				return "domain = " + target, nil
			}
			return fail(113, "Could not find domain for "+target)
		}
		pid, ok := f.loaded[target]
		if !ok {
			return fail(113, "Could not find service \""+AgentLabel+"\" in domain")
		}
		if f.releasing[target] {
			delete(f.loaded, target)
			delete(f.releasing, target)
		}
		if f.crashLoop {
			f.nextPID++
			f.loaded[target] = f.nextPID
			pid = f.nextPID
		}
		return fmt.Sprintf("%s = {\n\tstate = running\n\tprogram = %s\n\tpid = %d\n}", target, f.programs[target], pid), nil
	case "bootout":
		if _, ok := f.loaded[args[1]]; !ok {
			return fail(3, "Boot-out failed: 3: No such process")
		}
		f.releasing[args[1]] = true
		return "", nil
	case "bootstrap":
		service := args[1] + "/" + AgentLabel
		if _, held := f.loaded[service]; held {
			return fail(5, "Bootstrap failed: 5: Input/output error")
		}
		data, _ := os.ReadFile(args[2])
		if f.refuse > 0 || (f.refuseOf != "" && strings.Contains(string(data), f.refuseOf)) {
			f.refuse--
			return fail(5, "Bootstrap failed: 5: Input/output error")
		}
		f.nextPID++
		f.loaded[service] = f.nextPID
		job, _ := ReadJob(data)
		f.programs[service] = job.Binary
		f.bootstraped = append(f.bootstraped, args[2])
		return "", nil
	}
	return fail(1, "unknown subcommand")
}

type testMac struct {
	*Host
	launchd *fakeLaunchd
	out     *bytes.Buffer
	chowned []string
}

var alice = Account{Name: "alice", UID: 501, GID: 20}

func newTestMac(t *testing.T, admin bool) *testMac {
	t.Helper()
	root := t.TempDir()
	f := newFakeLaunchd()
	m := &testMac{launchd: f, out: &bytes.Buffer{}}
	self := alice
	self.Home = "/Users/alice"
	m.Host = &Host{
		Launchctl: f.run, Root: root, Admin: admin, Self: self,
		ProcessTable: func(context.Context) (string, error) { return "1 /sbin/launchd\n", nil },
		ProcessAlive: func(pid int) (bool, error) {
			for _, running := range f.loaded {
				if running == pid {
					return true, nil
				}
			}
			return false, nil
		},
		Lookup: func(name string) (Account, error) {
			if name != "alice" {
				return Account{}, errors.New("unknown user " + name)
			}
			return self, nil
		},
		Chown: func(path string, uid, gid int) error {
			m.chowned = append(m.chowned, fmt.Sprintf("%s %d:%d", path, uid, gid))
			return nil
		},
		Out: m.out, Wait: time.Second, Tick: time.Millisecond, Stable: 5 * time.Millisecond,
	}
	return m
}

func testJob(t *testing.T, server string) Job {
	dir := t.TempDir()
	return Job{
		Binary: filepath.Join(dir, "gpuworks-agent"), Server: server, StateDir: dir,
		Env: map[string]string{"GPUWORKS_LOG_LEVEL": "debug"},
	}
}

// The plist runs `run --state DIR` and nothing else: the server, token and
// the rest are in DIR's configuration file, not in arguments ps shows or in
// a plist other users may read.
func TestPlistRunsTheStateDirectoryOnly(t *testing.T) {
	job := testJob(t, "http://gw:8800")
	job.User = "alice"
	data := job.Plist()
	text := string(data)
	for _, want := range []string{
		"<key>UserName</key><string>alice</string>",
		"<string>--state</string><string>" + job.StateDir + "</string>",
		"<key>AbandonProcessGroup</key><true/>",
		"<key>StandardErrorPath</key><string>" + filepath.Join(job.StateDir, StderrLog) + "</string>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plist lacks %q:\n%s", want, text)
		}
	}
	for _, refused := range []string{"--token", "--server", "GPUWORKS_TOKEN", "http://gw:8800"} {
		if strings.Contains(text, refused) {
			t.Fatalf("plist carries %q:\n%s", refused, text)
		}
	}
	got, err := ReadJob(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Binary != job.Binary || got.StateDir != job.StateDir || got.Server != "" || got.Token != "" || got.User != "alice" || got.Env["GPUWORKS_LOG_LEVEL"] != "debug" {
		t.Fatalf("ReadJob = %+v, want %+v", got, job)
	}
}

// An install from before the token moved to the environment wrote it as an
// argument; reading that plist still finds the server, token and state.
func TestReadJobOfAnEarlierInstall(t *testing.T) {
	old := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>Label</key><string>ai.gpuworks.agent</string>
  <key>UserName</key><string>huang</string>
  <key>ProgramArguments</key>
  <array>
    <string>/opt/gpuworks/gpuworks-agent</string><string>run</string>
    <string>--server</string><string>http://192.0.2.110:8800</string>
    <string>--token</string><string>a&amp;b</string>
    <string>--connect-mode</string><string>agent_dial</string>
    <string>--listen</string><string>:19090</string>
    <string>--state</string><string>/opt/gpuworks</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>GPUWORKS_RUNTIME_DRIVER</key><string></string>
  </dict>
  <key>KeepAlive</key><true/>
</dict>
</plist>`
	job, err := ReadJob([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if job.Server != "http://192.0.2.110:8800" || job.Token != "a&b" || job.StateDir != "/opt/gpuworks" || job.User != "huang" || job.Binary != "/opt/gpuworks/gpuworks-agent" {
		t.Fatalf("ReadJob = %+v", job)
	}
	if _, err := ReadJob([]byte("<plist><dict></dict></plist>")); err == nil {
		t.Fatal("a plist without arguments was read as a job")
	}
	if _, err := ReadJob([]byte("<plist><dict><key>ProgramArguments</key>")); err == nil {
		t.Fatal("a truncated plist was read as a job")
	}
}

func TestInstallWithoutAdministratorRightsRegistersALaunchAgent(t *testing.T) {
	m := newTestMac(t, false)
	job := testJob(t, "http://gw:8800")
	job.User = "alice" // ignored: a launch agent runs as its session's owner
	pid, err := m.Install(context.Background(), job)
	if err != nil {
		t.Fatalf("Install: %v\n%s", err, m.out)
	}
	plist := filepath.Join(m.Root, "/Users/alice/Library/LaunchAgents/ai.gpuworks.agent.plist")
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(plist); info.Mode().Perm() != 0o600 {
		t.Fatalf("plist mode %v; it carries the token", info.Mode())
	}
	if strings.Contains(string(data), "UserName") {
		t.Fatal("a launch agent names a user")
	}
	if pid != m.launchd.loaded["gui/501/"+AgentLabel] || pid == 0 {
		t.Fatalf("pid %d, launchd has %v", pid, m.launchd.loaded)
	}
	for _, call := range m.launchd.calls {
		if strings.HasPrefix(call, "kickstart") {
			t.Fatalf("kickstart restarts the agent RunAtLoad just started: %v", m.launchd.calls)
		}
	}
	if len(m.chowned) != 0 {
		t.Fatalf("a launch agent install changed owners: %v", m.chowned)
	}
}

func TestInstallAsAdministratorRunsADaemonAsTheUserAndReplacesTheirLaunchAgent(t *testing.T) {
	m := newTestMac(t, true)
	agentPlist := filepath.Join(m.Root, AgentPlist("/Users/alice"))
	if err := os.MkdirAll(filepath.Dir(agentPlist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentPlist, testJob(t, "http://gw:8800").Plist(), 0o600); err != nil {
		t.Fatal(err)
	}
	m.launchd.loaded["gui/501/"+AgentLabel] = 600
	job := testJob(t, "http://gw:8800")
	job.User = "alice"
	if _, err := m.Install(context.Background(), job); err != nil {
		t.Fatalf("Install: %v\n%s", err, m.out)
	}
	data, err := os.ReadFile(filepath.Join(m.Root, DaemonPlist()))
	if err != nil || !strings.Contains(string(data), "<key>UserName</key><string>alice</string>") {
		t.Fatalf("daemon plist: %v\n%s", err, data)
	}
	if _, err := os.Stat(agentPlist); !os.IsNotExist(err) {
		t.Fatal("the user's launch agent survived: both agents would run")
	}
	if _, ok := m.launchd.loaded["system/"+AgentLabel]; !ok {
		t.Fatalf("no daemon loaded: %v", m.launchd.calls)
	}
	// The test's files belong to whoever runs it; for alice they are
	// someone else's, so they are given to her.
	if os.Geteuid() != alice.UID && len(m.chowned) == 0 {
		t.Fatal("the state and install directories were not given to the agent's user")
	}
	for _, user := range []string{"", "root"} {
		job.User = user
		if _, err := m.Install(context.Background(), job); err == nil || !strings.Contains(err.Error(), "never runs as root") {
			t.Fatalf("user %q: %v", user, err)
		}
	}
}

// bootout returns before launchd lets go of the job; the install waits for it
// and retries a bootstrap refused meanwhile.
func TestReinstallWaitsForLaunchdToReleaseTheOldJob(t *testing.T) {
	m := newTestMac(t, false)
	job := testJob(t, "http://gw:8800")
	if _, err := m.Install(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	m.launchd.refuse = 2
	if _, err := m.Install(context.Background(), job); err != nil {
		t.Fatalf("reinstall: %v\n%v", err, m.launchd.calls)
	}
	if len(m.launchd.bootstraped) != 2 {
		t.Fatalf("bootstraps %v", m.launchd.bootstraped)
	}
}

// A new job launchd will not take does not leave the Mac without an agent:
// the previous plist is put back and started.
func TestAFailedReinstallRestoresThePreviousJob(t *testing.T) {
	m := newTestMac(t, false)
	oldJob := testJob(t, "http://old-server:8800")
	if _, err := m.Install(context.Background(), oldJob); err != nil {
		t.Fatal(err)
	}
	newJob := testJob(t, "http://new-server:8800")
	m.launchd.refuseOf = newJob.StateDir
	_, err := m.Install(context.Background(), newJob)
	if err == nil || !strings.Contains(err.Error(), "previous agent job was restored") {
		t.Fatalf("Install = %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(m.Root, AgentPlist("/Users/alice")))
	if job, _ := ReadJob(data); job.StateDir != oldJob.StateDir {
		t.Fatalf("plist now runs %q, want the previous job's %q", job.StateDir, oldJob.StateDir)
	}
	if _, ok := m.launchd.loaded["gui/501/"+AgentLabel]; !ok {
		t.Fatal("the previous job is not running")
	}
}

// A plist written before the configuration file names its server as an
// argument; replacing it for another server says so. (Plists written since
// name only the state directory; the install script compares configuration
// files.)
func TestInstallOverAnEarlierInstallsJobWarnsOfAnotherServer(t *testing.T) {
	m := newTestMac(t, false)
	plist := filepath.Join(m.Root, AgentPlist("/Users/alice"))
	legacy := Spec{Label: AgentLabel, Args: []string{"/opt/gpuworks/gpuworks-agent", "run", "--server", "http://old-server:8800", "--state", "/opt/gpuworks"}}.Plist()
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(context.Background(), testJob(t, "http://new-server:8800")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.out.String(), "WARNING: replacing the gpuworks agent installed for another server (http://old-server:8800)") {
		t.Fatalf("no warning about the other server:\n%s", m.out)
	}
}

func TestInstallReportsAnAgentLaunchdCannotKeepRunning(t *testing.T) {
	m := newTestMac(t, false)
	m.launchd.crashLoop = true
	m.Stable = 50 * time.Millisecond
	m.Wait = 30 * time.Millisecond
	job := testJob(t, "http://gw:8800")
	if err := os.WriteFile(filepath.Join(job.StateDir, StderrLog), []byte("starting\n--token is required\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := m.Install(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "does not keep the agent running") || !strings.Contains(err.Error(), "--token is required") {
		t.Fatalf("Install = %v", err)
	}
}

// Over SSH a user who is not logged in on the Mac has no session domain: the
// launch agent could not start, which is said at once and plainly.
func TestALaunchAgentNeedsALoginSession(t *testing.T) {
	m := newTestMac(t, false)
	delete(m.launchd.sessions, "gui/501")
	_, err := m.Install(context.Background(), testJob(t, "http://gw:8800"))
	if !errors.Is(err, ErrNoSession) || !strings.Contains(err.Error(), "alice is not logged in") {
		t.Fatalf("Install = %v", err)
	}
	for _, call := range m.launchd.calls {
		if strings.HasPrefix(call, "bootstrap") {
			t.Fatalf("bootstrapped without a session: %v", m.launchd.calls)
		}
	}
}

// A launch agent beside a daemon would be a second agent on the same GPU,
// and without administrator rights the daemon cannot be replaced.
func TestALaunchAgentInstallRefusesBesideADaemon(t *testing.T) {
	m := newTestMac(t, false)
	daemon := filepath.Join(m.Root, DaemonPlist())
	if err := os.MkdirAll(filepath.Dir(daemon), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemon, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(context.Background(), testJob(t, "http://gw:8800")); err == nil || !strings.Contains(err.Error(), "launch daemon is installed") {
		t.Fatalf("Install = %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Root, AgentPlist("/Users/alice"))); !os.IsNotExist(err) {
		t.Fatal("a launch agent was written beside the daemon")
	}
}

func TestUninstallRemovesBothJobsAndNamesTheirState(t *testing.T) {
	m := newTestMac(t, true)
	job := testJob(t, "http://gw:8800")
	job.User = "alice"
	if _, err := m.Install(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	dirs, err := m.Uninstall(context.Background(), job.Binary, nil)
	if err != nil || len(dirs) != 1 || dirs[0] != job.StateDir {
		t.Fatalf("Uninstall = %v, %v", dirs, err)
	}
	if _, err := os.Stat(filepath.Join(m.Root, DaemonPlist())); !os.IsNotExist(err) {
		t.Fatal("daemon plist left")
	}
	// Nothing left: uninstalling again succeeds and does nothing.
	if dirs, err := m.Uninstall(context.Background(), job.Binary, nil); err != nil || len(dirs) != 0 {
		t.Fatalf("second Uninstall = %v, %v", dirs, err)
	}

	// Without administrator rights the daemon cannot be removed, and the
	// command says so rather than reporting success.
	if _, err := m.Install(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	m.Admin = false
	if _, err := m.Uninstall(context.Background(), job.Binary, nil); err == nil || !strings.Contains(err.Error(), "sudo gpuworks-agent uninstall") {
		t.Fatalf("Uninstall without rights = %v", err)
	}
}

func TestAccountLookupOfTheCurrentUser(t *testing.T) {
	name := os.Getenv("USER")
	if name == "" {
		t.Skip("USER unset")
	}
	account, err := LookupAccount(name)
	if err != nil || strconv.Itoa(account.UID) != strconv.Itoa(os.Getuid()) || account.Home == "" {
		t.Fatalf("LookupAccount(%s) = %+v, %v", name, account, err)
	}
}

func TestRunningPIDReadsLaunchctlPrint(t *testing.T) {
	// What macOS 15 prints, trimmed: the job's own state and pid come first;
	// nested sections repeat "state =" for other things.
	running := "system/ai.gpuworks.server = {\n\tactive count = 1\n\tpath = /Library/LaunchDaemons/ai.gpuworks.server.plist\n\tstate = running\n\n\tprogram = /opt/gpuworks/server/gpuworks\n\tpid = 4321\n\tendpoints = {\n\t}\n}"
	if got := RunningPID(running); got != 4321 {
		t.Fatalf("RunningPID = %d", got)
	}
	waiting := "system/ai.gpuworks.server = {\n\tstate = spawn scheduled\n\tlast exit code = 1\n}"
	if got := RunningPID(waiting); got != 0 {
		t.Fatalf("RunningPID of a job between restarts = %d", got)
	}
}
