package provision

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/launchd"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// manualAgentLine is how `ps axww` listed an agent started by hand on the PPU
// host, beside the systemd one.
const ownUnitLine = "2172144 ?        Ssl    0:05 /opt/gpuworks/gpuworks-agent run --server http://198.51.100.122:8800 --token 0123456789abcdef --state /opt/gpuworks --connect-mode agent_dial --listen :19090"

const manualAgentLine = "2996222 ?        Sl     6:23 /var/tmp/ppu-own-agent-recovery-r3-once-20261011/gpuworks-agent run --server http://192.0.2.60:8800 --token 9a7fc99fd545dadc7ed4a711a462137893cef679a5dfd4c3 --state /var/tmp/ppu-own-agent-recovery-r3-once-20261011/state --driver docker --connect-mode agent_dial"

func TestExistingAgentCheckWarnsBeforeAnInstallReplacesAnAgent(t *testing.T) {
	const self = "http://198.51.100.122:8800"
	for _, tc := range []struct {
		name   string
		host   *unitHost
		status string
		code   string
		detail string
	}{
		{name: "clean-host", host: &unitHost{}, status: "pass", code: messages.CheckExistingAgentNone, detail: "no gpuworks agent on the host"},
		{name: "own-unit", host: &unitHost{unitServer: self + "/", active: true, processes: []string{ownUnitLine}},
			status: "warn", code: messages.CheckExistingAgentThisServer, detail: "the gpuworks-agent service on the host already runs for this server; installing replaces it and the node it belongs to goes offline"},
		{name: "another-servers-unit", host: &unitHost{unitServer: "http://192.0.2.156:8800", active: true},
			status: "warn", code: messages.CheckExistingAgentOtherServer, detail: "the gpuworks-agent service on the host belongs to another server (http://192.0.2.156:8800); installing replaces it and that server loses this host"},
		{name: "another-servers-stopped-unit", host: &unitHost{unitServer: "http://192.0.2.156:8800"},
			status: "warn", code: messages.CheckExistingAgentOtherServer, detail: "the gpuworks-agent service on the host belongs to another server (http://192.0.2.156:8800); installing replaces it and that server loses this host"},
		{name: "agent-started-by-hand", host: &unitHost{processes: []string{manualAgentLine}},
			status: "warn", code: messages.CheckExistingAgentOthers, detail: "agents for other servers (http://192.0.2.60:8800) run outside the gpuworks-agent service; installing leaves them running"},
		// What 192.0.2.210 looked like to its own server on 2026-10-14.
		{name: "own-unit-and-another-by-hand", host: &unitHost{unitServer: self, active: true, processes: []string{manualAgentLine, ownUnitLine}},
			status: "warn", code: messages.CheckExistingAgentThisServer, detail: "the gpuworks-agent service on the host already runs for this server; installing replaces it and the node it belongs to goes offline; " +
				"agents for other servers (http://192.0.2.60:8800) run outside the gpuworks-agent service; installing leaves them running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port := startFakeSSHServer(t, "root", "pw", tc.host.handler(t))
			got := existingAgentCheck(t.Context(), dialFakeSSH(t, host, port, "root", "pw"), self)
			if got.ID != CheckIDExistingAgent || got.Status != tc.status || got.Code != tc.code || got.text() != tc.detail {
				t.Fatalf("check = %+v, want status %q detail containing %q", got, tc.status, tc.detail)
			}
			if strings.Contains(got.text(), "86daa49d") || strings.Contains(got.text(), "9a7fc99f") {
				t.Fatalf("check leaked a node token: %q", got.text())
			}
		})
	}
}

func TestGuardedInstallStopsBeforeChangingAHostWithAnAgent(t *testing.T) {
	foreign := &unitHost{unitServer: "http://192.0.2.156:8800", active: true}
	inner := foreign.handler(t)
	var mu sync.Mutex
	var cmds []string
	host, port := startFakeSSHServer(t, "root", "pw", func(cmd string, stdin io.Reader) (string, string, uint32) {
		mu.Lock()
		cmds = append(cmds, cmd)
		mu.Unlock()
		return inner(cmd, stdin)
	})
	installer := NewSSHInstaller(func(string) ([]byte, error) { return nil, errors.New("agent binary is not available in this test") })
	req := SSHInstallRequest{
		Target: "root@" + host, Port: port, ServerURL: "http://198.51.100.122:8800", Token: "tok",
		SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"}, GuardExistingAgent: true,
	}
	_, err := installer.InstallSSH(t.Context(), req)
	var existing *ExistingAgentError
	if !errors.As(err, &existing) || existing.Found.UnitServer != "http://192.0.2.156:8800" {
		t.Fatalf("install err = %v", err)
	}
	mu.Lock()
	ran := append([]string(nil), cmds...)
	mu.Unlock()
	if len(ran) != 1 || ran[0] != existingAgentCommand {
		t.Fatalf("a guarded install ran more than the read-only look: %q", ran)
	}

	// Confirmed, it goes on to install; this host stops it at the binary read.
	req.ReplaceExistingAgent = true
	report, err := installer.InstallSSH(t.Context(), req)
	if errors.As(err, &existing) || err == nil {
		t.Fatalf("confirmed install err = %v", err)
	}
	found := false
	for _, check := range report.Checks {
		if check.ID == CheckIDExistingAgent && check.Status == "warn" && strings.Contains(check.text(), "replaced on request") {
			found = true
		}
	}
	if !found {
		t.Fatalf("confirmed install did not record the replaced agent: %+v", report.Checks)
	}
}

func TestParseExistingAgentDedupesServers(t *testing.T) {
	got := parseExistingAgent("unit http://a:8800/\nprocess http://a:8800\nprocess http://b:8800\nprocess http://b:8800/\nnoise\n")
	if !got.Unit || got.UnitServer != "http://a:8800" || strings.Join(got.ProcessServers, ",") != "http://a:8800,http://b:8800" {
		t.Fatalf("parsed = %+v", got)
	}
	if !got.Replaces("http://b:8800") {
		t.Fatal("the unit is replaced whichever server installs")
	}
	if empty := parseExistingAgent("unit \n"); !empty.Found() || empty.UnitServer != "" || !strings.Contains(existingAgentDetail(empty, "http://a"), "server cannot be read") {
		t.Fatalf("unit with unreadable server = %+v", empty)
	}
}

func TestAnInstallLeavesOtherServersHandStartedAgentsAlone(t *testing.T) {
	byHand := parseExistingAgent("process http://192.0.2.60:8800\n")
	if byHand.Replaces("http://198.51.100.122:8800") {
		t.Fatal("another server's agent outside the unit keeps running, so it does not stop the install")
	}
	if !byHand.Replaces("http://192.0.2.60:8800/") {
		t.Fatal("this server's agent started by hand is stopped by the rootless start")
	}
}

// The look for an existing agent reads a Mac's launchd job with the shell,
// not a plist parser: it finds the server in the configuration file of the
// state directory the plist names, or, for a plist an earlier install wrote,
// in its arguments.
func TestExistingAgentCommandReadsTheServerOfAMacsJob(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	state := filepath.Join(t.TempDir(), "secrets", "agent-state")
	const token = "mac-job-fixture-token-c8b31d"
	if err := agentconf.Write(state, agentconf.Config{Server: "http://192.0.2.110:8800", Token: token}); err != nil {
		t.Fatal(err)
	}
	for name, plistData := range map[string][]byte{
		"configured":      launchd.Job{Binary: state + "/gpuworks-agent", StateDir: state}.Plist(),
		"earlier install": launchd.Spec{Label: launchd.AgentLabel, Args: []string{"/Users/a/.gpuworks/gpuworks-agent", "run", "--server", "http://192.0.2.110:8800", "--state", "/Users/a/.gpuworks"}}.Plist(),
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			plist := filepath.Join(home, launchd.AgentPlist(""))
			if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plist, plistData, 0o600); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			// Only process discovery is synthetic; plist and configuration reads use
			// the real shell tools and files, without inspecting this host's agents.
			writeExecutable(t, filepath.Join(bin, "ps"), `case "$*" in
  'axww -o args='|'axww') printf '%s\n' "$FIXTURE_PS" ;;
  *) exit 2 ;;
esac
`)
			writeExecutable(t, filepath.Join(bin, "systemctl"), "exit 3\n")
			for _, process := range []struct {
				name string
				line string
			}{
				{name: "no process"},
				{
					name: "configured process with secrets directory",
					line: state + "/gpuworks-agent run --state " + state + " --server http://stale.example:8800",
				},
			} {
				t.Run(process.name, func(t *testing.T) {
					cmd := exec.Command("sh", "-c", existingAgentCommand)
					cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "FIXTURE_PS=" + process.line}
					out, err := cmd.Output()
					if err != nil {
						t.Fatal(err)
					}
					found := parseExistingAgent(string(out))
					if !found.Unit || found.UnitServer != "http://192.0.2.110:8800" || strings.Contains(string(out), token) {
						t.Fatalf("found %+v from %q", found, out)
					}
					if process.line == "" {
						if len(found.ProcessServers) != 0 || len(found.ProcessStates) != 0 {
							t.Fatalf("unexpected processes: %+v from %q", found, out)
						}
					} else if len(found.ProcessServers) != 1 || found.ProcessServers[0] != "http://192.0.2.110:8800" ||
						len(found.ProcessStates) != 1 || found.ProcessStates[0] != state || !strings.Contains(string(out), "/secrets/") {
						t.Fatalf("configured process: %+v from %q", found, out)
					}
				})
			}
		})
	}
}

// A nohup install runs beside the host's unit and its other agents: it goes
// ahead without asking unless an agent already runs from its own directory.
func TestGuardedNohupInstallAsksOnlyAboutItsOwnDirectory(t *testing.T) {
	host := &unitHost{unitServer: "http://198.51.100.122:8800", active: true, processes: []string{ownUnitLine, manualAgentLine}}
	h, port := startFakeSSHServer(t, "root", "pw", host.handler(t))
	installer := NewSSHInstaller(func(string) ([]byte, error) { return nil, errors.New("agent binary is not available in this test") })
	req := SSHInstallRequest{
		Target: "root@" + h, Port: port, ServerURL: "http://198.51.100.122:8800", Token: "tok",
		SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"}, GuardExistingAgent: true, NoSystemd: true,
	}
	var existing *ExistingAgentError
	// The default directory is where the unit's agent runs.
	if _, err := installer.InstallSSH(t.Context(), req); !errors.As(err, &existing) || existing.InstallDir != "/opt/gpuworks" ||
		err.Error() != "a gpuworks agent already runs from /opt/gpuworks; installing there replaces it" {
		t.Fatalf("install into the unit's directory err = %v", err)
	}
	// Another directory installs (and stops at the binary read) without asking.
	req.InstallDir = "/data/agent2"
	report, err := installer.InstallSSH(t.Context(), req)
	if err == nil || errors.As(err, &existing) {
		t.Fatalf("install beside the unit err = %v", err)
	}
	for _, check := range report.Checks {
		if check.ID == CheckIDExistingAgent {
			t.Fatalf("an install that replaces nothing reported a replaced agent: %+v", check)
		}
	}
}

func TestParseExistingAgentReadsStateDirectories(t *testing.T) {
	got := parseExistingAgent("process http://a:8800 /opt/gpuworks/\nprocess http://a:8800 /data/b\nprocess http://b:8800\n")
	if strings.Join(got.ProcessStates, ",") != "/data/b,/opt/gpuworks" || strings.Join(got.ProcessServers, ",") != "http://a:8800,http://b:8800" {
		t.Fatalf("parsed = %+v", got)
	}
	if !got.ReplacesNohup("/opt/gpuworks") || got.ReplacesNohup("/data/c") {
		t.Fatalf("ReplacesNohup = %+v", got)
	}
}

// A running agent the server dials has no server: it is still an agent on
// the host, known by its state directory.
func TestParseExistingAgentCountsAServerDialAgent(t *testing.T) {
	found := parseExistingAgent("process - /opt/gpuworks\nprocess http://gw:8800/ /data/b\n")
	if !found.Found() || len(found.ProcessServers) != 1 || found.ProcessServers[0] != "http://gw:8800" ||
		strings.Join(found.ProcessStates, ",") != "/data/b,/opt/gpuworks" {
		t.Fatalf("found %+v", found)
	}
	if !found.ReplacesNohup("/opt/gpuworks/") {
		t.Fatal("a nohup install into the server_dial agent's directory replaces it")
	}
}
