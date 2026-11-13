package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentProcessProbePreservesFilterFailures(t *testing.T) {
	for _, failure := range []string{"missing awk", "awk fails after output"} {
		t.Run(failure, func(t *testing.T) {
			dir, bin := connectionDiagnosticFixture(t, "owned", "http://server")
			for _, tool := range []string{"sh", "sed", "head", "tail", "dirname"} {
				path, err := exec.LookPath(tool)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
					t.Fatal(err)
				}
			}
			writeExecutable(t, filepath.Join(dir, "gpuworks-agent"), "exit 0")
			line := dir + "/gpuworks-agent run --state " + dir
			writeExecutable(t, filepath.Join(bin, "ps"), "printf '%s\\n' "+shellQuote(line))
			writeExecutable(t, filepath.Join(bin, "curl"), "printf 200")
			if failure == "awk fails after output" {
				writeExecutable(t, filepath.Join(bin, "awk"), "printf '%s\\n' "+shellQuote(line)+"; exit 2")
			}

			recovery := exec.Command("sh", "-c", agentRecoveryProbeCommand(dir, "http://server", "owned"))
			recovery.Env = []string{"PATH=" + bin}
			if out, err := recovery.Output(); err == nil || strings.TrimSpace(string(out)) != "" {
				t.Errorf("failed process inspection returned a recovery verdict: output=%q err=%v", out, err)
			}

			diagnostic := exec.Command("sh", "-c", accessInspectionCommand(dir, "http://server", "agent_dial"))
			diagnostic.Env = []string{"PATH=" + bin}
			diagnostic.Stdin = strings.NewReader("owned\n")
			raw, err := diagnostic.Output()
			if err != nil {
				t.Fatalf("independent checks did not finish: %v", err)
			}
			got := parseAccessInspection(string(raw), AccessInspection{Reason: "inspection_unavailable"})
			if got.Process != "unavailable" {
				t.Errorf("failed process inspection must be unavailable, got %+v", got)
			}
			if got.Installation != "passed" || got.Connection.Outcome != "http_ok" {
				t.Errorf("filter failure discarded independent checks: %+v", got)
			}
		})
	}
}
