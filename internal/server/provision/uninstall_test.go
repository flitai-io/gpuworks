package provision

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUninstallAgentOverSSHChecksCompletion(t *testing.T) {
	for _, scenario := range []string{"success", "command-failure", "false-success", "foreign-service", "remaining-mac-process", "remaining-linux-process", "mac-success", "mac-probe-failure", "own-token", "another-nodes-service", "checklist-success", "checklist-blocked"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			toolsDir := filepath.Join(root, "tools")
			state := filepath.Join(root, "state")
			for _, dir := range []string{toolsDir, state} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			binary := filepath.Join(root, "gpuworks-agent")
			unit := filepath.Join(root, "unit")
			calls := filepath.Join(root, "calls")
			server := "https://uninstall-fixture.example"
			owner := server
			if scenario == "foreign-service" {
				owner = "https://another-control.example"
			}
			if err := os.WriteFile(unit, []byte("[Service]\nExecStart="+binary+" run --server "+owner+" --token secret-fixture-token --state "+state+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(toolsDir, "systemctl"), `case "$*" in
  "cat gpuworks-agent.service") cat "$UNIT" ;;
  *) echo 'unexpected systemctl command' >&2; exit 2 ;;
esac`)
			writeExecutable(t, filepath.Join(toolsDir, "id"), `echo 0`)
			writeExecutable(t, filepath.Join(toolsDir, "uname"), `echo Linux`)
			if scenario == "remaining-mac-process" || strings.HasPrefix(scenario, "mac-") {
				writeExecutable(t, filepath.Join(toolsDir, "uname"), `echo Darwin`)
				probe := "echo 123; exit 0"
				if scenario == "mac-success" {
					probe = "exit 1"
				} else if scenario == "mac-probe-failure" {
					probe = "exit 2"
				}
				writeExecutable(t, filepath.Join(toolsDir, "pgrep"), probe)
			}
			writeExecutable(t, binary, `if [ "$1" = uninstall ] && [ "$2" = --help ]; then
  case "$SCENARIO" in
    checklist-*) echo '  -keep-resources  -yes  -state DIR' ;;
    *) echo '  -state DIR' ;;
  esac
  exit 0
fi
printf '%s\n' "$@" >> "$CALLS"
case "$SCENARIO" in
  checklist-*) [ "$4" = --yes ] || { echo 'requires explicit --yes' >&2; exit 2; } ;;
esac
[ "$1" = uninstall ] && [ "$2" = --state ] && [ "$3" = "$STATE" ] || exit 2
case "$SCENARIO" in
  checklist-blocked) echo 'active workloads must be stopped first' >&2; exit 3 ;;
  command-failure) echo 'permission denied' >&2; exit 9 ;;
  false-success) echo 'service stopped'; exit 0 ;;
esac
rm "$UNIT"
echo 'service removed'
`)
			if scenario == "remaining-linux-process" {
				writeUninstallProc(t, root, 100, "gpuworks-agent", binary, "run", "--server", server, "--state", state, "--token", "secret-fixture-token")
			}
			host, port := startFakeSSHServer(t, "operator", "password", func(cmd string, input io.Reader) (string, string, uint32) {
				command := exec.Command("sh", "-c", cmd)
				if input != nil {
					body, err := io.ReadAll(input)
					if err != nil {
						return "", err.Error(), 1
					}
					command.Stdin = strings.NewReader(uninstallInspectFixtureScript(string(body), root))
				}
				command.Env = append(os.Environ(), "PATH="+toolsDir+":"+os.Getenv("PATH"), "HOME="+root,
					"UNIT="+unit, "CALLS="+calls, "STATE="+state, "SCENARIO="+scenario)
				var stdout, stderr strings.Builder
				command.Stdout, command.Stderr = &stdout, &stderr
				var status uint32
				if err := command.Run(); err != nil {
					status = 1
					if exit, ok := err.(*exec.ExitError); ok {
						status = uint32(exit.ExitCode())
					}
				}
				return stdout.String(), stderr.String(), status
			})
			installer := NewSSHInstaller(nil)
			// With the node's token only its own installation counts: the
			// unit of another node on the same host is never uninstalled.
			token := ""
			switch scenario {
			case "own-token":
				token = "secret-fixture-token"
			case "another-nodes-service":
				token = "another-nodes-token"
			}
			err := installer.UninstallAgent(t.Context(), SSHProbeRequest{
				Host: host, Port: port, User: "operator", ServerURL: server, Token: token,
				SSHAuth: SSHAuth{Mode: AuthPassword, Password: "password"},
			})
			if (err == nil) != (scenario == "success" || scenario == "mac-success" || scenario == "own-token" || scenario == "checklist-success") {
				t.Fatalf("scenario=%s err=%v", scenario, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-fixture-token") {
				t.Fatal("error exposed node token")
			}
			expectedArgs := "uninstall\n--state\n" + state + "\n"
			if strings.HasPrefix(scenario, "checklist-") {
				expectedArgs += "--yes\n"
			}
			data, readErr := os.ReadFile(calls)
			if scenario == "foreign-service" || scenario == "another-nodes-service" {
				if !os.IsNotExist(readErr) {
					t.Fatalf("another agent was uninstalled: %s", data)
				}
			} else if string(data) != expectedArgs {
				t.Fatalf("wrong command arguments: %q (%v)", data, readErr)
			}
		})
	}
}

func TestUninstallAgentRejectsUnknownInstallation(t *testing.T) {
	for _, out := range []string{"", "absent", "/bin/gpuworks-agent\n", "/bin/gpuworks-agent\n/var/lib/agent\nhttp://other", "/bin/not-agent\n/var/lib/agent\nhttp://control"} {
		if _, err := parseAgentUninstallInstallation(out, "http://control"); err == nil {
			t.Errorf("accepted unverified installation %q", out)
		}
	}
	got, err := parseAgentUninstallInstallation("/custom path/gpuworks-agent\n/custom state\nhttp://control/\n", "http://control")
	if err != nil || got.binary != "/custom path/gpuworks-agent" || got.state != "/custom state" {
		t.Fatalf("custom paths: %+v %v", got, err)
	}
}

func TestUninstallAgentDoesNotRunOnAuthenticationFailure(t *testing.T) {
	host, port := startFakeSSHServer(t, "operator", "correct", func(string, io.Reader) (string, string, uint32) {
		t.Error("command executed after failed authentication")
		return "", "", 1
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := NewSSHInstaller(nil).UninstallAgent(ctx, SSHProbeRequest{
		Host: host, Port: port, User: "operator", ServerURL: "http://control",
		SSHAuth: SSHAuth{Mode: AuthPassword, Password: "wrong"},
	})
	if err == nil {
		t.Fatal("authentication failure reported as success")
	}
}
