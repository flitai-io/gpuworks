package installscript

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slurmAgentStub is the agent binary of a Slurm node's install: configure
// writes what the real one does for --slurm-node, the enrollment token read
// from stdin as SLURM_ENROLL_TOKEN with the node's Slurm name, and no TOKEN.
const slurmAgentStub = `#!/bin/sh
echo "agent $*" >> "$CALLS"
if [ "$1" = configure ]; then
  shift
  state=""; server=""; node=""
  while [ $# -gt 0 ]; do
    case "$1" in --state) state="$2"; shift 2 ;; --server) server="$2"; shift 2 ;; --slurm-node) node="$2"; shift 2 ;; *) shift ;; esac
  done
  read -r token
  if [ -n "$node" ]; then
    printf 'SERVER=%s\nTOKEN=\nSLURM_ENROLL_TOKEN=%s\nSLURM_NODE=%s\n' "$server" "$token" "$node" > "$state/gpuworks-agent.conf"
  else
    printf 'SERVER=%s\nTOKEN=%s\n' "$server" "$token" > "$state/gpuworks-agent.conf"
  fi
fi
exit 0
`

// slurmHost is a Linux host as the health check meets it: root, systemd
// faked, with an agent stub that knows --slurm-node, accounts from a fixed
// list (id -u NAME) and chown recording what it is asked.
func slurmHost(t *testing.T, accounts ...string) *linuxHost {
	t.Helper()
	h := newLinuxHost(t)
	if err := os.WriteFile(h.uploaded, []byte(slurmAgentStub), 0o755); err != nil {
		t.Fatal(err)
	}
	known := strings.Join(accounts, " ")
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(h.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("id", `case "$1" in
  -u) if [ -n "$2" ]; then for a in `+known+`; do [ "$a" = "$2" ] && { echo 1001; exit 0; }; done; echo "id: '$2': no such user" >&2; exit 1; fi; echo 0 ;;
  -g) echo 0 ;;
esac`)
	stub("chown", `echo "chown $*" >> "$CALLS"`)
	return h
}

func (h *linuxHost) waitForCall(line string) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls := h.read(h.calls)
		if strings.Contains(calls, line+"\n") {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("agent did not record %q:\n%s", line, calls)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The health check installs an agent with the cluster entry's enrollment
// token: the node's Slurm name and the token reach `configure` (the token on
// stdin, never in an argument list), the unit runs as root, and the agent is
// not given a node token of its own.
func TestScriptInstallsASlurmNodeWithTheEnrollmentToken(t *testing.T) {
	requireSystemd(t)
	h := slurmHost(t)
	out, err := h.run([]string{"GPUWORKS_SLURM_ENROLL_TOKEN=enroll-token", "GPUWORKS_SLURM_NODE=cn000"}, "--server", "http://gw:8800", "--supervisor", "systemd")
	if err != nil {
		t.Fatalf("install = %v\n%s", err, out)
	}
	calls := h.read(h.calls)
	if !strings.Contains(calls, "agent configure --state "+h.stateDir+" --connect-mode agent_dial --listen  --driver  --mount-root  --udocker-bundle  --server http://gw:8800 --slurm-node cn000") {
		t.Fatalf("configure call:\n%s", calls)
	}
	if strings.Contains(calls, "enroll-token") || strings.Contains(out, "enroll-token") {
		t.Fatalf("the enrollment token reached an argument list or the output:\n%s\n%s", calls, out)
	}
	if conf := h.read(filepath.Join(h.stateDir, "gpuworks-agent.conf")); conf != "SERVER=http://gw:8800\nTOKEN=\nSLURM_ENROLL_TOKEN=enroll-token\nSLURM_NODE=cn000\n" {
		t.Fatalf("configuration = %q", conf)
	}
	unit := h.read(h.written)
	if strings.Contains(unit, "User=") || strings.Contains(unit, "enroll-token") || !strings.Contains(unit, "ExecStart="+h.stateDir+"/gpuworks-agent run --state "+h.stateDir+"\n") {
		t.Fatalf("unit:\n%s", unit)
	}
	if strings.Contains(calls, "chown") {
		t.Fatalf("a root install changed ownership:\n%s", calls)
	}
	for _, want := range []string{"systemctl enable gpuworks-agent", "systemctl restart gpuworks-agent"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("calls lack %q:\n%s", want, calls)
		}
	}
}

// --token with --slurm-node is the enrollment token too, for an install by
// hand; the server address is still required, as for any agent that dials.
func TestScriptSlurmNodeFlagsAndRequirements(t *testing.T) {
	requireSystemd(t)
	h := slurmHost(t)
	if out, err := h.run(nil, "--server", "http://gw:8800", "--token", "enroll-token", "--slurm-node", "cn001", "--supervisor", "systemd"); err != nil {
		t.Fatalf("install = %v\n%s", err, out)
	}
	if conf := h.read(filepath.Join(h.stateDir, "gpuworks-agent.conf")); conf != "SERVER=http://gw:8800\nTOKEN=\nSLURM_ENROLL_TOKEN=enroll-token\nSLURM_NODE=cn001\n" {
		t.Fatalf("configuration = %q", conf)
	}
	h2 := slurmHost(t)
	out, err := h2.run([]string{"GPUWORKS_SLURM_NODE=cn000"}, "--server", "http://gw:8800")
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || !strings.Contains(out, "--token is required") {
		t.Fatalf("a Slurm node with no enrollment token = %v\n%s", err, out)
	}
}

// Keep recognises the unit of a Slurm node by its enrollment token: the same
// entry's unit is taken over, another entry's (or a node token's) is left alone.
func TestScriptKeepRecognisesASlurmNodesUnit(t *testing.T) {
	requireSystemd(t)
	for _, tc := range []struct {
		name, conf string
		systemd    bool
	}{
		{"this entry's agent", "SERVER=http://gw:8800\nTOKEN=\nSLURM_ENROLL_TOKEN=enroll-token\nSLURM_NODE=cn000\n", true},
		{"another entry's agent", "SERVER=http://gw:8800\nTOKEN=\nSLURM_ENROLL_TOKEN=other\nSLURM_NODE=cn000\n", false},
		{"an agent with a node token", "SERVER=http://gw:8800\nTOKEN=enroll-token\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := slurmHost(t)
			unitState := t.TempDir()
			if err := os.WriteFile(filepath.Join(unitState, "gpuworks-agent.conf"), []byte(tc.conf), 0o600); err != nil {
				t.Fatal(err)
			}
			h.withUnit("--state " + unitState)
			out, err := h.run([]string{"GPUWORKS_SLURM_ENROLL_TOKEN=enroll-token", "GPUWORKS_SLURM_NODE=cn000"}, "--server", "http://gw:8800", "--supervisor", "keep")
			if err != nil {
				t.Fatalf("install = %v\n%s", err, out)
			}
			if !tc.systemd {
				h.waitForCall("agent run --state " + h.stateDir)
			}
			if wrote := strings.Contains(h.read(h.calls), "tee /etc/systemd/system/gpuworks-agent.service"); wrote != tc.systemd {
				t.Fatalf("wrote the unit = %v, want %v\n%s", wrote, tc.systemd, out)
			}
		})
	}
}

// --run-as runs the unit as an existing account that owns what the agent
// keeps; the installer creates no account.
func TestScriptRunAsWritesUserIntoTheUnitAndHandsOverTheDirectories(t *testing.T) {
	requireSystemd(t)
	h := slurmHost(t, "gpusvc")
	out, err := h.run([]string{"GPUWORKS_SLURM_ENROLL_TOKEN=enroll-token", "GPUWORKS_SLURM_NODE=cn000"}, "--server", "http://gw:8800", "--supervisor", "systemd", "--run-as", "gpusvc")
	if err != nil {
		t.Fatalf("install = %v\n%s", err, out)
	}
	unit := h.read(h.written)
	if !strings.Contains(unit, "[Service]\nUser=gpusvc\nExecStartPre=") {
		t.Fatalf("unit lacks User=gpusvc:\n%s", unit)
	}
	calls := h.read(h.calls)
	chown := "chown -R gpusvc " + h.stateDir + " " + h.stateDir
	if !strings.Contains(calls, chown) {
		t.Fatalf("calls lack %q:\n%s", chown, calls)
	}
	// The configuration is handed over after it is written and before the
	// service starts, or the agent could not read its own token.
	if iConfigure, iChown, iStart := strings.Index(calls, "agent configure"), strings.Index(calls, "chown -R"), strings.Index(calls, "systemctl restart"); !(iConfigure < iChown && iChown < iStart) {
		t.Fatalf("order of configure, chown and start:\n%s", calls)
	}
	// The account root is no account to switch to.
	h = slurmHost(t)
	if out, err := h.run(nil, "--server", "http://gw:8800", "--token", "t", "--supervisor", "systemd", "--run-as", "root"); err != nil || strings.Contains(h.read(h.written), "User=") || strings.Contains(h.read(h.calls), "chown") {
		t.Fatalf("--run-as root = %v\n%s\n%s", err, out, h.read(h.written))
	}
}

func TestScriptRunAsRefusals(t *testing.T) {
	requireSystemd(t)
	for _, tc := range []struct {
		name string
		env  []string
		args []string
		code int
		want string
	}{
		{"unknown account", nil, []string{"--run-as", "ghost"}, 1, "account ghost does not exist"},
		{"no systemd to write the unit", []string{"GPUWORKS_NO_SYSTEMD=1"}, []string{"--run-as", "gpusvc"}, 1, "--run-as needs the systemd supervisor"},
		{"nohup supervisor", nil, []string{"--run-as", "gpusvc", "--supervisor", "nohup"}, 1, "--run-as needs the systemd supervisor"},
		{"unsafe name", nil, []string{"--run-as", "x;rm"}, 2, "--run-as must be a login name"},
		{"upper case", nil, []string{"--run-as", "Gpu"}, 2, "--run-as must be a login name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := slurmHost(t, "gpusvc")
			out, err := h.run(tc.env, append([]string{"--server", "http://gw:8800", "--token", "t"}, tc.args...)...)
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != tc.code || !strings.Contains(out, tc.want) {
				t.Fatalf("install = %v\n%s", err, out)
			}
			if strings.Contains(h.read(h.calls), "configure") || strings.Contains(h.read(h.calls), "systemctl restart") {
				t.Fatalf("a refused install went on:\n%s", h.read(h.calls))
			}
		})
	}
}

// The enrollment token reaches the installer in its environment and goes no
// further: the agent a nohup install starts, and the restart script that
// starts it again, run without it.
func TestScriptSlurmEnrollmentDoesNotStayInTheAgentsEnvironment(t *testing.T) {
	h := slurmHost(t)
	stub := strings.TrimSuffix(slurmAgentStub, "exit 0\n") + `if [ "$1" = run ]; then
  echo "run-env token=${GPUWORKS_SLURM_ENROLL_TOKEN-unset} node=${GPUWORKS_SLURM_NODE-unset}" >> "$CALLS"
fi
exit 0
`
	if err := os.WriteFile(h.uploaded, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := h.run([]string{"GPUWORKS_SLURM_ENROLL_TOKEN=enroll-token", "GPUWORKS_SLURM_NODE=cn000", "GPUWORKS_NO_SYSTEMD=1"}, "--server", "http://gw:8800")
	if err != nil {
		t.Fatalf("install = %v\n%s", err, out)
	}
	h.waitForCall("run-env token=unset node=unset")
	restart := h.read(filepath.Join(h.stateDir, "agent-restart.sh"))
	if !strings.Contains(restart, "unset GPUWORKS_SLURM_ENROLL_TOKEN GPUWORKS_SLURM_NODE") || strings.Contains(restart, "enroll-token") {
		t.Fatalf("restart script:\n%s", restart)
	}
}
