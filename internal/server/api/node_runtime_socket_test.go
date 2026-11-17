package api

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A node whose agent may not open the Docker socket says so on the wire,
// with the facts and the server's reading of them by exact key and the
// command the notice copies; the agent's state directory goes into that
// command and not onto the wire itself. An unread gid is absent rather than
// root's 0. A node that fell back for another reason has no socket object.
func TestANodeSaysWhichSocketItsAgentMayNotOpenOnTheWire(t *testing.T) {
	n := newNetworkAPI(t)
	ok := true
	for _, uuid := range []string{"denied", "unread", "not-ready"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	denied := proto.RuntimeSocketDenial{Socket: "/var/run/docker.sock", Group: "docker", GID: 999, Account: "gpusvc",
		SameNameGID: 1002, Supervisor: proto.AgentSupervisorNohup, StateDir: "/home/gpusvc/gpuworks"}
	unread := proto.RuntimeSocketDenial{Socket: "/var/run/docker.sock", GID: proto.GIDUnknown, Account: "gpusvc"}
	for uuid, report := range map[string]struct {
		reason string
		socket *proto.RuntimeSocketDenial
	}{
		"denied":    {proto.RuntimeFallbackSocketPermissionDenied, &denied},
		"unread":    {proto.RuntimeFallbackSocketPermissionDenied, &unread},
		"not-ready": {proto.RuntimeFallbackDaemonNotReady, &denied},
	} {
		if err := n.store.UpdateNodeStatus(uuid, proto.NodeStatus{GPUProbeOK: &ok, Sys: proto.SysInfo{GPUVendor: "nvidia", RuntimeDriver: "udocker",
			RuntimeFallbackFrom: "docker", RuntimeFallbackReason: report.reason, RuntimeFallbackSocket: report.socket}}); err != nil {
			t.Fatal(err)
		}
	}
	socketOf := func(uuid string) (map[string]string, string) {
		t.Helper()
		var node struct {
			Socket map[string]json.RawMessage `json:"runtime_fallback_socket"`
		}
		if err := json.Unmarshal(n.memberGet("/api/v1/nodes/"+uuid), &node); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for k, v := range node.Socket {
			got[k] = string(v)
		}
		var command string
		if raw, ok := node.Socket["command"]; ok {
			if err := json.Unmarshal(raw, &command); err != nil {
				t.Fatal(err)
			}
			got["command"] = "(checked apart)"
		}
		return got, command
	}

	got, command := socketOf("denied")
	want := map[string]string{
		"cause": `"not_in_group"`, "socket": `"/var/run/docker.sock"`, "group": `"docker"`, "gid": `999`,
		"account": `"gpusvc"`, "same_name_gid": `1002`, "supervisor": `"nohup"`, "command": "(checked apart)",
	}
	if !reflect.DeepEqual(got, want) || command != runtimeSocketCommand(denied) ||
		!strings.HasPrefix(command, "sudo gpasswd -a gpusvc docker\nsudo -u gpusvc sh -c ") {
		t.Fatalf("socket on the wire:\n%v\nwant\n%v\ncommand %q", got, want, command)
	}

	got, command = socketOf("unread")
	if want := map[string]string{"cause": `"unknown"`, "socket": `"/var/run/docker.sock"`, "account": `"gpusvc"`}; !reflect.DeepEqual(got, want) || command != "" {
		t.Fatalf("unread socket on the wire:\n%v\nwant\n%v (command %q)", got, want, command)
	}

	other := map[string]any{}
	if err := json.Unmarshal(n.memberGet("/api/v1/nodes/not-ready"), &other); err != nil {
		t.Fatal(err)
	}
	assertJSONKeysAbsent(t, other, "runtime_fallback_socket")
}

// Group membership is advised only where it is the cause: not when the
// agent already holds the group (the socket's mode or SELinux/AppArmor
// refuses it), not for the root group, not when the gid is unread.
func TestRuntimeSocketCommandFollowsWhatTheAgentFound(t *testing.T) {
	base := proto.RuntimeSocketDenial{Socket: "/var/run/docker.sock", Group: "docker", GID: 999, Account: "gpusvc"}
	with := func(edit func(*proto.RuntimeSocketDenial)) proto.RuntimeSocketDenial {
		d := base
		edit(&d)
		return d
	}
	systemd := func(d *proto.RuntimeSocketDenial) { d.Supervisor = proto.AgentSupervisorSystemd }
	for _, tc := range []struct {
		name, cause string
		d           proto.RuntimeSocketDenial
		want        string
	}{
		{"systemd", dto.RuntimeSocketCauseNotInGroup, with(systemd),
			"sudo gpasswd -a gpusvc docker\nsudo systemctl restart gpuworks-agent"},
		{"supervisor unknown: the notice says how to restart", dto.RuntimeSocketCauseNotInGroup, base, "sudo gpasswd -a gpusvc docker"},
		{"nohup with a relative state directory: no restart line", dto.RuntimeSocketCauseNotInGroup, with(func(d *proto.RuntimeSocketDenial) {
			d.Supervisor, d.StateDir = proto.AgentSupervisorNohup, "gpuworks"
		}), "sudo gpasswd -a gpusvc docker"},
		{"already in the group: only the restart", dto.RuntimeSocketCauseRestartPending, with(func(d *proto.RuntimeSocketDenial) {
			d.RestartPending, d.Supervisor = true, proto.AgentSupervisorSystemd
		}), "sudo systemctl restart gpuworks-agent"},
		{"already in the group, supervisor unknown", dto.RuntimeSocketCauseRestartPending, with(func(d *proto.RuntimeSocketDenial) { d.RestartPending = true }), ""},
		{"the agent holds the group already", dto.RuntimeSocketCauseGroupHeld, with(func(d *proto.RuntimeSocketDenial) {
			systemd(d)
			d.GroupHeld = true
		}), ""},
		{"the root group", dto.RuntimeSocketCauseRootGroup, with(func(d *proto.RuntimeSocketDenial) {
			systemd(d)
			d.Group, d.GID = "root", 0
		}), ""},
		{"gid unread", dto.RuntimeSocketCauseUnknown, with(func(d *proto.RuntimeSocketDenial) {
			systemd(d)
			d.Group, d.GID = "", proto.GIDUnknown
		}), ""},
		{"account unknown", dto.RuntimeSocketCauseNotInGroup, with(func(d *proto.RuntimeSocketDenial) { d.Account = "" }), ""},
		{"group without a name", dto.RuntimeSocketCauseNotInGroup, with(func(d *proto.RuntimeSocketDenial) { d.Group = "" }), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtimeSocketCause(tc.d); got != tc.cause {
				t.Fatalf("cause = %q, want %q", got, tc.cause)
			}
			if got := runtimeSocketCommand(tc.d); got != tc.want {
				t.Fatalf("command =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// TestMain lets this test binary be the nohup agent the restart command
// looks for: nohupHost starts it through a link named gpuworks-agent, so
// its command line reads `.../gpuworks-agent run --state <dir>`, and it
// sleeps instead of running the tests ("deaf" also ignores SIGTERM).
func TestMain(m *testing.M) {
	if mode := os.Getenv("GPUWORKS_FAKE_NOHUP_AGENT"); mode != "" {
		if mode == "deaf" {
			signal.Ignore(syscall.SIGTERM)
		}
		// Ready: the signal disposition is set.
		_ = os.WriteFile(os.Getenv("GPUWORKS_FAKE_NOHUP_READY"), nil, 0o644)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// nohupHost is a host with a nohup install in a state directory whose path
// a shell would split: agent-restart.sh records that it ran, sudo and
// gpasswd are stand-ins that record their arguments and run the rest as is.
type nohupHost struct {
	t                  *testing.T
	state, bin, record string
}

func newNohupHost(t *testing.T) *nohupHost {
	t.Helper()
	h := &nohupHost{t: t, state: filepath.Join(t.TempDir(), "it's state"), bin: t.TempDir(), record: filepath.Join(t.TempDir(), "record")}
	if err := os.MkdirAll(h.state, 0o700); err != nil {
		t.Fatal(err)
	}
	h.write(filepath.Join(h.bin, "sudo"), "#!/bin/sh\necho \"sudo $1 $2\" >> \"$RECORD\"\nif [ \"$1\" = -u ]; then shift 2; fi\nexec \"$@\"\n")
	h.write(filepath.Join(h.bin, "gpasswd"), "#!/bin/sh\necho \"gpasswd $*\" >> \"$RECORD\"\n")
	h.write(filepath.Join(h.state, "agent-restart.sh"), "#!/bin/sh\necho restarted >> \"$RECORD\"\n")
	return h
}

func (h *nohupHost) write(path, body string) {
	h.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

// start runs a process until the test ends, reaping it as soon as it exits
// so kill -0 stops seeing it; the channel closes then.
func (h *nohupHost) start(cmd *exec.Cmd) (pid int, exited <-chan struct{}) {
	h.t.Helper()
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	h.t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	return cmd.Process.Pid, done
}

// agent starts a fake nohup agent of stateDir; mode "deaf" ignores SIGTERM.
func (h *nohupHost) agent(stateDir, mode string) (int, <-chan struct{}) {
	h.t.Helper()
	self, err := os.Executable()
	if err != nil {
		h.t.Fatal(err)
	}
	link := filepath.Join(h.t.TempDir(), "gpuworks-agent")
	if err := os.Symlink(self, link); err != nil {
		h.t.Fatal(err)
	}
	if mode == "" {
		mode = "plain"
	}
	ready := filepath.Join(h.t.TempDir(), "ready")
	cmd := exec.Command(link, "run", "--state", stateDir)
	cmd.Env = append(os.Environ(), "GPUWORKS_FAKE_NOHUP_AGENT="+mode, "GPUWORKS_FAKE_NOHUP_READY="+ready)
	pid, exited := h.start(cmd)
	// Wait until the agent has set itself up (a deaf one ignores SIGTERM
	// only from then on).
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatal("fake agent never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return pid, exited
}

func (h *nohupHost) pidFile(pid int) {
	h.write(filepath.Join(h.state, "gpuworks-agent.pid"), strconv.Itoa(pid)+"\n")
}

// run runs the server's command for this host and returns what the host
// recorded and whether the command succeeded.
func (h *nohupHost) run() (record []string, out string, err error) {
	h.t.Helper()
	command := runtimeSocketCommand(proto.RuntimeSocketDenial{Socket: "/var/run/docker.sock", Group: "docker", GID: 999,
		Account: "gpusvc", Supervisor: proto.AgentSupervisorNohup, StateDir: h.state})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+h.bin+string(os.PathListSeparator)+os.Getenv("PATH"), "RECORD="+h.record)
	raw, err := cmd.CombinedOutput()
	b, _ := os.ReadFile(h.record)
	return strings.Split(strings.TrimSpace(string(b)), "\n"), string(raw), err
}

func gone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// The nohup restart does what the install script's stop_nohup_agent does,
// and no more: it stops this state directory's agent, found by its command
// line (so a stale or missing pid file changes nothing), never a process
// that merely holds the recorded pid or another state directory's agent,
// and starts the new agent only once the old one is gone. An agent that
// will not stop fails the command, and no second agent is started.
func TestTheNohupRestartCommand(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("the restart command reads /proc, which only Linux has")
	}
	added := []string{"sudo gpasswd -a", "gpasswd -a gpusvc docker", "sudo -u gpusvc"}
	t.Run("the running agent, with its pid file", func(t *testing.T) {
		h := newNohupHost(t)
		pid, exited := h.agent(h.state, "")
		h.pidFile(pid)
		record, out, err := h.run()
		if err != nil || !gone(exited) || !reflect.DeepEqual(record, append(added, "restarted")) {
			t.Fatalf("err=%v agent gone=%t record=%q\n%s", err, gone(exited), record, out)
		}
	})
	t.Run("the running agent, pid file missing", func(t *testing.T) {
		h := newNohupHost(t)
		_, exited := h.agent(h.state, "")
		record, out, err := h.run()
		if err != nil || !gone(exited) || !reflect.DeepEqual(record, append(added, "restarted")) {
			t.Fatalf("err=%v agent gone=%t record=%q\n%s", err, gone(exited), record, out)
		}
	})
	t.Run("a stale pid file now held by another process", func(t *testing.T) {
		h := newNohupHost(t)
		pid, other := h.start(exec.Command("sleep", "60"))
		h.pidFile(pid)
		_, elsewhere := h.agent(filepath.Join(t.TempDir(), "another state"), "")
		record, out, err := h.run()
		if err != nil || gone(other) || gone(elsewhere) || !reflect.DeepEqual(record, append(added, "restarted")) {
			t.Fatalf("err=%v other gone=%t other agent gone=%t record=%q\n%s", err, gone(other), gone(elsewhere), record, out)
		}
	})
	t.Run("an agent that does not stop", func(t *testing.T) {
		wait := agentStopWait
		agentStopWait = 1
		t.Cleanup(func() { agentStopWait = wait })
		h := newNohupHost(t)
		pid, exited := h.agent(h.state, "deaf")
		h.pidFile(pid)
		record, out, err := h.run()
		if err == nil || gone(exited) || !reflect.DeepEqual(record, added) || !strings.Contains(out, "did not stop; not starting another") {
			t.Fatalf("err=%v agent gone=%t record=%q\n%s", err, gone(exited), record, out)
		}
	})
}
