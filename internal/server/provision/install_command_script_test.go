package provision

import (
	"os/exec"
	"strings"
	"testing"
)

// The install line embeds values the operator typed (the server URL) and a
// secret (the token): each must stay one shell word, quotes included.
func TestScriptInstallCommandQuotesEveryArgument(t *testing.T) {
	got := ScriptInstallCommand("https://control.example/o'mal/", "tok'en", "agent_dial", "")
	want := `curl -fsSL 'https://control.example/o'\''mal/install.sh' | sh -s -- --server 'https://control.example/o'\''mal' --token 'tok'\''en'`
	if got != want {
		t.Fatalf("command = %s\nwant      %s", got, want)
	}
	got = ScriptInstallCommand("http://10.0.0.1:8800", "t", "server_dial", ":19090")
	want = `curl -fsSL 'http://10.0.0.1:8800/install.sh' | sh -s -- --server 'http://10.0.0.1:8800' --token 't' --connect-mode server_dial --listen ':19090'`
	if got != want {
		t.Fatalf("server_dial command = %s\nwant %s", got, want)
	}
}

// Unattended, the install must fail when the server cannot be reached; in a
// pipeline the status would be sh's, which succeeds on an empty script.
func TestScriptInstallStepsFailWhenTheDownloadFails(t *testing.T) {
	got := ScriptInstallSteps("http://10.0.0.1:8800/", "t'k", "agent_dial", "", ScriptOptions{})
	want := "set -e\n" +
		"script=$(mktemp)\n" +
		"trap 'rm -f \"$script\"' EXIT\n" +
		"n=0\n" +
		"until curl -fsSL --connect-timeout 10 -o \"$script\" 'http://10.0.0.1:8800/install.sh'; do n=$((n+1)); [ \"$n\" -ge 3 ] && exit 1; sleep 1; done\n" +
		`sh "$script" --server 'http://10.0.0.1:8800' --token 't'\''k'`
	if got != want {
		t.Fatalf("steps =\n%s\nwant\n%s", got, want)
	}
	for _, shell := range []string{"sh", "bash"} {
		if _, err := exec.LookPath(shell); err != nil {
			continue
		}
		// Port 9 on loopback refuses: the run must end non-zero, quickly.
		cmd := exec.Command(shell, "-c", ScriptInstallSteps("http://127.0.0.1:9", "t", "agent_dial", "", ScriptOptions{}))
		if err := cmd.Run(); err == nil {
			t.Fatalf("%s: install steps succeeded although the server was unreachable", shell)
		}
	}
}

// The operator's directory and supervisor reach install.sh as its own flags;
// the defaults add none, so install.sh keeps choosing them.
func TestScriptInstallStepsCarryTheOperatorsChoices(t *testing.T) {
	got := ScriptInstallSteps("http://10.0.0.1:8800", "t", "agent_dial", "", ScriptOptions{InstallDir: " /data/gpu works/ ", NoSystemd: true})
	if want := `sh "$script" --server 'http://10.0.0.1:8800' --token 't' --install-dir '/data/gpu works' --no-systemd`; !strings.HasSuffix(got, want) {
		t.Fatalf("steps =\n%s\nwant suffix %s", got, want)
	}
	got = ScriptInstallSteps("http://10.0.0.1:8800", "t", "agent_dial", "", ScriptOptions{InstallDir: "/data/gpuworks"})
	if !strings.HasSuffix(got, ` --install-dir '/data/gpuworks'`) || strings.Contains(got, "--no-systemd") {
		t.Fatalf("steps = %s", got)
	}
}

// The check after a script install looks for the agent in the directory the
// install was given, and names it.
func TestAgentRunningCheckFollowsTheInstallDirectory(t *testing.T) {
	if cmd := AgentRunningCheckCommand("/data/gpuworks/"); !strings.Contains(cmd, "dir='/data/gpuworks'") {
		t.Fatalf("command = %s", cmd)
	}
	if cmd := AgentRunningCheckCommand(""); !strings.Contains(cmd, "dir='/opt/gpuworks'") {
		t.Fatalf("default command = %s", cmd)
	}
	if got := AgentRunningCheck(agentRunningOK, "/data/gpuworks"); got.Status != "pass" || got.text() != "running with state directory /data/gpuworks" {
		t.Fatalf("ok check = %+v", got)
	}
	if got := AgentRunningCheck(agentRunningWarn, ""); got.Status != "warn" || !strings.Contains(got.text(), "/opt/gpuworks") {
		t.Fatalf("warn check = %+v", got)
	}
}
