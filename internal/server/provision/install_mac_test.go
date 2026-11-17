package provision

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/server/installscript"
)

// fakeMac answers the commands the installer sends to a Mac. sudoPassword is
// the account password sudo accepts ("" when sudo needs one the test never
// gives); passwordless makes sudo -n succeed. launchdErr makes the installer
// fail the way install-service does without a login session.
type fakeMac struct {
	arch         string
	sudoPassword string
	passwordless bool
	launchdErr   bool
	// agentDir is where an earlier install left the agent; empty means none.
	agentDir string

	mu       sync.Mutex
	uploads  map[string][]byte // staged path -> decompressed payload
	install  string            // the install command
	stdin    string            // what the install command read on stdin
	removed  bool
	commands []string
}

func (m *fakeMac) handle(cmd string, stdin io.Reader) (string, string, uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands = append(m.commands, cmd)
	readAll := func() string { b, _ := io.ReadAll(stdin); return string(b) }
	switch {
	case cmd == "uname -s":
		return "Darwin\n", "", 0
	case strings.HasPrefix(cmd, "uname -m"):
		return m.arch + "\n", "", 0
	case strings.Contains(cmd, `.gpuworks-install" && mkdir -p`):
		return "/Users/me/.gpuworks-install", "", 0
	case strings.Contains(cmd, "sha256sum -c -"):
		// uploadShellCommand: mkdir -p DIR && cat > STAGED.gz ... mv -f STAGED PATH
		fields := strings.Fields(cmd)
		path := strings.Trim(fields[len(fields)-1], "'")
		zr, err := gzip.NewReader(stdin)
		if err != nil {
			return "", err.Error(), 1
		}
		data, _ := io.ReadAll(zr)
		m.uploads[path] = data
		return "", "", 0
	case cmd == "id -u":
		return "501\n", "", 0
	case cmd == "sudo -n true":
		if m.passwordless {
			return "", "", 0
		}
		return "", "sudo: a password is required\n", 1
	case cmd == "sudo -S -p '' true":
		if m.sudoPassword != "" && readAll() == m.sudoPassword+"\n" {
			return "", "", 0
		}
		return "", "Sorry, try again.\nsudo: no password was provided\n", 1
	case strings.Contains(cmd, "GPUWORKS_AGENT_BINARY="):
		m.install = cmd
		if strings.HasPrefix(cmd, "sudo -S") {
			m.stdin = readAll()
		}
		if m.launchdErr {
			// What install-service says when the user has no login session.
			return "gpuworks: installing agent from http://server:8800 (darwin/arm64)\n", "gpuworks-agent install-service: no login session: me is not logged in on this Mac, so a launch agent cannot start\n", 1
		}
		return "gpuworks: installing agent from http://server:8800 (darwin/arm64)\ngpuworks: agent running under launchd (system/ai.gpuworks.agent, pid 4242); logs: /opt/gpuworks/agent.log\ngpuworks: installed in /opt/gpuworks\n", "", 0
	case cmd == `printf '%s' "$HOME"`:
		return "/Users/me", "", 0
	case strings.HasSuffix(cmd, `gw_mac_install_dir "$HOME"`):
		if m.agentDir != "" {
			return m.agentDir, "", 0
		}
		return "/Users/me/gpuworks", "", 0
	case strings.HasPrefix(cmd, "rm -rf "):
		m.removed = true
		return "", "", 0
	}
	return "", "unexpected command: " + cmd, 127
}

func installOnFakeMac(t *testing.T, mac *fakeMac, auth SSHAuth, installDir string) (SSHInstallReport, error) {
	t.Helper()
	mac.uploads = map[string][]byte{}
	host, port := startFakeSSHServer(t, "me", "pw", mac.handle)
	installer := NewSSHInstaller(func(name string) ([]byte, error) {
		if name != "gpuworks-agent-darwin-arm64" {
			return nil, errors.New("no artifact " + name)
		}
		return []byte("darwin agent binary"), nil
	})
	if auth.Mode == "" {
		auth = SSHAuth{Mode: AuthPassword, Password: "pw"}
	}
	return installer.InstallSSH(t.Context(), SSHInstallRequest{
		Target: "me@" + host, Port: port, SSHAuth: auth, ServerURL: "http://server:8800/", Token: "tok'en",
		InstallDir: installDir,
	})
}

func checkNamed(checks []CheckResult, name string) (CheckResult, bool) {
	for _, c := range checks {
		if c.ID == name {
			return c, true
		}
	}
	return CheckResult{}, false
}

func TestInstallSSHOnAMacRunsTheInstallScriptWithSudoFromThePassword(t *testing.T) {
	mac := &fakeMac{arch: "arm64", sudoPassword: "pw"}
	report, err := installOnFakeMac(t, mac, SSHAuth{}, "")
	if err != nil {
		t.Fatalf("install: %v (checks %+v)", err, report.Checks)
	}
	if report.BinaryName != "gpuworks-agent-darwin-arm64" || report.Arch != "arm64" || report.InstallDir != "/Users/me/gpuworks" {
		t.Fatalf("report = %+v", report)
	}
	var bin, script []byte
	for path, data := range mac.uploads {
		switch {
		case strings.Contains(path, "/Users/me/.gpuworks-install/gpuworks-agent."):
			bin = data
		case strings.Contains(path, "/Users/me/.gpuworks-install/install."):
			script = data
		}
	}
	if string(bin) != "darwin agent binary" {
		t.Fatalf("uploaded binary = %q (uploads %v)", bin, mac.uploads)
	}
	if !bytes.Equal(script, []byte(installscript.Script)) {
		t.Fatal("the uploaded installer is not the script /install.sh serves")
	}
	for _, want := range []string{
		"sudo -S -p '' env GPUWORKS_AGENT_BINARY='/Users/me/.gpuworks-install/gpuworks-agent.",
		" sh '/Users/me/.gpuworks-install/install.",
		"--server 'http://server:8800'",
		`--token 'tok'\''en'`,
		"--connect-mode 'agent_dial'",
	} {
		if !strings.Contains(mac.install, want) {
			t.Errorf("install command lacks %q:\n%s", want, mac.install)
		}
	}
	// The directory is the server's decision, passed explicitly.
	if !strings.Contains(mac.install, "--install-dir '/Users/me/gpuworks'") || strings.Contains(mac.install, "pw") {
		t.Errorf("install command lacks the directory, or carries the password:\n%s", mac.install)
	}
	if logged := commandlog.Redact(mac.install); !strings.Contains(logged, "--token <redacted>") || strings.Contains(logged, "tok'") {
		t.Errorf("the command log would show the node token: %s", logged)
	}
	if mac.stdin != "pw\n" {
		t.Errorf("sudo read %q on stdin, want the password", mac.stdin)
	}
	if c, _ := checkNamed(report.Checks, "privilege"); c.Status != "pass" || !strings.Contains(c.text(), "launch daemon") {
		t.Errorf("privilege = %+v", c)
	}
	if c, _ := checkNamed(report.Checks, "start_agent"); c.Status != "pass" || !strings.Contains(c.text(), "pid 4242") {
		t.Errorf("start agent = %+v", c)
	}
	if !mac.removed {
		t.Error("the staging directory was left behind")
	}
}

func TestInstallSSHOnAMacWithoutAdminRightsInstallsALaunchAgent(t *testing.T) {
	mac := &fakeMac{arch: "arm64"}
	report, err := installOnFakeMac(t, mac, SSHAuth{}, "/Users/me/agent/")
	if err != nil {
		t.Fatalf("install: %v (checks %+v)", err, report.Checks)
	}
	if strings.HasPrefix(mac.install, "sudo") || !strings.Contains(mac.install, "--install-dir '/Users/me/agent'") {
		t.Fatalf("install command = %s", mac.install)
	}
	if c, _ := checkNamed(report.Checks, "privilege"); c.Status != "warn" || !strings.Contains(c.text(), "launch agent") {
		t.Fatalf("privilege = %+v", c)
	}
	if report.InstallDir != "/Users/me/agent" {
		t.Fatalf("install dir = %q", report.InstallDir)
	}
}

// Without rights and without a directory asked for, the agent goes to the
// user's home, as the precheck said it would.
func TestInstallSSHOnAMacWithoutAdminRightsDefaultsToTheHome(t *testing.T) {
	mac := &fakeMac{arch: "arm64"}
	report, err := installOnFakeMac(t, mac, SSHAuth{}, "")
	if err != nil {
		t.Fatalf("install: %v (checks %+v)", err, report.Checks)
	}
	if report.InstallDir != "/Users/me/gpuworks" || !strings.Contains(mac.install, "--install-dir '/Users/me/gpuworks'") {
		t.Fatalf("install dir %q, command %s", report.InstallDir, mac.install)
	}
}

// An agent an earlier install left in /opt/gpuworks is reinstalled there, with
// its models, rather than beside a fresh one in the home.
func TestInstallSSHOnAMacKeepsAnEarlierInstallDirectory(t *testing.T) {
	mac := &fakeMac{arch: "arm64", sudoPassword: "pw", agentDir: "/opt/gpuworks"}
	report, err := installOnFakeMac(t, mac, SSHAuth{}, "")
	if err != nil {
		t.Fatalf("install: %v (checks %+v)", err, report.Checks)
	}
	if report.InstallDir != "/opt/gpuworks" || !strings.Contains(mac.install, "--install-dir '/opt/gpuworks'") {
		t.Fatalf("install dir %q, command %s", report.InstallDir, mac.install)
	}
}

func TestInstallSSHOnAMacPrefersPasswordlessSudo(t *testing.T) {
	mac := &fakeMac{arch: "arm64", passwordless: true}
	if _, err := installOnFakeMac(t, mac, SSHAuth{}, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(mac.install, "sudo -n env GPUWORKS_AGENT_BINARY=") {
		t.Fatalf("install command = %s", mac.install)
	}
}

func TestInstallSSHOnAnIntelMacStopsBeforeUploading(t *testing.T) {
	mac := &fakeMac{arch: "x86_64"}
	report, err := installOnFakeMac(t, mac, SSHAuth{}, "")
	if err == nil || !strings.Contains(err.Error(), "Apple silicon") {
		t.Fatalf("err = %v", err)
	}
	if len(mac.uploads) != 0 || mac.install != "" {
		t.Fatalf("uploaded %v / ran %q on an Intel Mac", mac.uploads, mac.install)
	}
	if c, _ := checkNamed(report.Checks, "arch"); c.Status != "fail" {
		t.Fatalf("arch check = %+v", c)
	}
}

func TestInstallSSHOnAMacReportsTheInstallerFailure(t *testing.T) {
	mac := &fakeMac{arch: "arm64", launchdErr: true}
	report, err := installOnFakeMac(t, mac, SSHAuth{}, "")
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("a launch agent without a session = %v; the error must say why", err)
	}
	c, _ := checkNamed(report.Checks, "start_agent")
	if c.Status != "fail" || !strings.Contains(c.text(), "not logged in") {
		t.Fatalf("start check = %+v", c)
	}
	if !mac.removed {
		t.Error("the staging directory was left behind after a failure")
	}
}

// launchdMac is a Mac whose agent runs as a launchd job, for RestartAgent.
type launchdMac struct {
	domain     string // where the job is: system, gui/501, or "" for none
	server     string // what the job's plist names; "" when unreadable
	uid        string
	sudo       bool
	mu         sync.Mutex
	commands   []string
	kickstarts []string
}

func (m *launchdMac) handle(cmd string, _ io.Reader) (string, string, uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commands = append(m.commands, cmd)
	switch {
	case cmd == "uname -s":
		return "Darwin\n", "", 0
	case cmd == existingAgentCommand:
		if m.domain == "" {
			return "", "", 0
		}
		return "unit " + m.server + "\n", "", 0
	case cmd == macJobDomainCommand:
		if m.domain == "" {
			return "", "", 0
		}
		return m.domain + "\n", "", 0
	case cmd == "id -u":
		return m.uid + "\n", "", 0
	case cmd == "sudo -n true":
		if m.sudo {
			return "", "", 0
		}
		return "", "sudo: a password is required\n", 1
	case strings.Contains(cmd, "launchctl kickstart -k "):
		m.kickstarts = append(m.kickstarts, cmd)
		return "", "", 0
	}
	return "", "unexpected command: " + cmd, 127
}

// A Mac's agent is restarted through launchd, never started beside it: the
// job keeps its own arguments, and a second agent would share the GPU.
func TestRestartAgentOnAMacKickstartsItsLaunchdJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		mac  *launchdMac
		want string // the kickstart command, or the error
	}{
		{"daemon with sudo", &launchdMac{domain: "system", uid: "501", sudo: true}, "sudo -n launchctl kickstart -k system/ai.gpuworks.agent"},
		{"daemon as root", &launchdMac{domain: "system", uid: "0"}, "launchctl kickstart -k system/ai.gpuworks.agent"},
		{"launch agent", &launchdMac{domain: "gui/501", server: "http://server:8800", uid: "501"}, "launchctl kickstart -k gui/501/ai.gpuworks.agent"},
		{"daemon without sudo", &launchdMac{domain: "system", uid: "501"}, "error: needs root or passwordless sudo"},
		{"no job", &launchdMac{uid: "501"}, "error: no gpuworks launchd job"},
		{"another server's job", &launchdMac{domain: "gui/501", server: "http://other:8800", uid: "501"}, "error: installed for another server (http://other:8800)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mac := tc.mac
			host, port := startFakeSSHServer(t, "me", "pw", mac.handle)
			err := (&SSHInstaller{}).RestartAgent(t.Context(), SSHProbeRequest{
				Host: host, Port: port, User: "me", SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
				ServerURL: "http://server:8800/", Token: "node-token",
			})
			for _, cmd := range mac.commands {
				if strings.Contains(cmd, "node-token") || strings.Contains(cmd, "nohup") {
					t.Fatalf("started an agent beside launchd: %s", cmd)
				}
			}
			if want, isErr := strings.CutPrefix(tc.want, "error: "); isErr {
				if err == nil || !strings.Contains(err.Error(), want) || len(mac.kickstarts) != 0 {
					t.Fatalf("RestartAgent = %v, kickstarts %v", err, mac.kickstarts)
				}
				return
			}
			if err != nil || len(mac.kickstarts) != 1 || mac.kickstarts[0] != tc.want {
				t.Fatalf("RestartAgent = %v, kickstarts %v", err, mac.kickstarts)
			}
		})
	}
}
