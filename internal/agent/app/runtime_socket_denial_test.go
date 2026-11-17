package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// accountDB puts id(1) and getent(1) on the fake host's PATH, answering in
// the C locale as NSS would: `id` for the agent's own process, `id <account>`
// for the account database (read from a file, so a test can run gpasswd),
// and `getent group <gid>`. The socket the fake host listens on belongs to
// the test's own group; the account's groups leave it out.
func (h *fakeDockerHost) accountDB(self, listed, group string) (setListed func(string)) {
	h.t.Helper()
	listedFile := filepath.Join(h.t.TempDir(), "listed")
	setListed = func(s string) {
		if err := os.WriteFile(listedFile, []byte(s+"\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	setListed(listed)
	id := "#!/bin/sh\nif [ $# -eq 0 ]; then echo '" + self + "'; exit 0; fi\n" +
		"while IFS= read -r line; do echo \"$line\"; done < " + listedFile + "\n"
	getent := "#!/bin/sh\n[ \"$1\" = group ] || exit 2\necho '" + group + "'\n"
	for name, script := range map[string]string{"id": id, "getent": getent} {
		if err := os.WriteFile(filepath.Join(h.bin, name), []byte(script), 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
	return setListed
}

// nohupState is a state directory a nohup install left, on a host whose
// agent is in no systemd unit.
func nohupState(t *testing.T) string {
	t.Helper()
	cgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(cgroup, []byte("0::/user.slice/user-51001.slice/session-3.scope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := selfCgroupFile
	selfCgroupFile = cgroup
	t.Cleanup(func() { selfCgroupFile = previous })
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "agent-restart.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return state
}

const gpusvcInDirectoryDocker = "uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc),51002(docker)"

// An agent whose account may not open the Docker socket — the E-HPC case,
// where the account is in a directory service's "docker" group and the
// socket belongs to the host's — passes Docker over for runveil, says why
// in a WARN with the facts, and reports them for the operator: socket,
// group, account, the same-named group, and how the agent is restarted.
func TestAutoDetectionReportsADockerSocketTheAccountMayNotOpen(t *testing.T) {
	host := newFakeDockerHost(t, daemonWithNVIDIA)
	host.liveness(daemonNoAccess, -1, daemonWithNVIDIA)
	gid := os.Getegid()
	host.accountDB(gpusvcInDirectoryDocker, gpusvcInDirectoryDocker, fmt.Sprintf("docker:x:%d:", gid))
	state := nohupState(t)
	var logs bytes.Buffer
	got := detectDriver(slog.New(slog.NewTextHandler(&logs, nil)), state, "", &gpu.SMIProber{})
	if driverName(got.driver) != "udocker" {
		t.Fatalf("driver = %q, want udocker (runveil)", driverName(got.driver))
	}
	socket := strings.TrimPrefix(os.Getenv("DOCKER_HOST"), "unix://")
	want := runtimeFallback{from: "docker", reason: proto.RuntimeFallbackSocketPermissionDenied, socket: proto.RuntimeSocketDenial{
		Socket: socket, Group: "docker", GID: gid, Account: "gpusvc", SameNameGID: 51002,
		Supervisor: proto.AgentSupervisorNohup, StateDir: state,
	}}
	if got.fallback != want {
		t.Fatalf("fallback = %+v\nwant       %+v", got.fallback, want)
	}
	out := logs.String()
	for _, line := range []string{"level=WARN msg=\"runtime driver passed over: the agent's account may not open its daemon socket",
		"account=gpusvc", "same_name_gid=51002", "permission denied while trying to connect"} {
		if !strings.Contains(out, line) {
			t.Fatalf("log lacks %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "no docker daemon") {
		t.Fatalf("the detected runtime is labelled as if there were no daemon:\n%s", out)
	}
	if !strings.Contains(out, "udocker (docker socket "+socket+": permission denied for gpusvc)") {
		t.Fatalf("detected label does not say why:\n%s", out)
	}
}

// The facts reach the status report only with their reason, and the
// minute-level recheck neither churns on them (an unchanged finding is not
// logged again) nor misses the operator's gpasswd: once the account
// database lists the account in the socket's group, the report says only a
// restart is left. The recheck cannot do more — the running process keeps
// the groups it started with.
func TestTheRecheckOfADeniedSocketTurnsIntoRestartPending(t *testing.T) {
	host := newFakeDockerHost(t, daemonWithNVIDIA)
	host.liveness(daemonNoAccess, -1, daemonWithNVIDIA)
	gid := os.Getegid()
	self := "uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc)"
	setListed := host.accountDB(self, self, fmt.Sprintf("docker:x:%d:", gid))
	var logs bytes.Buffer
	n := fallbackNode(t)
	n.log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx := context.Background()
	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)

	n.recheckRuntimeFallback(ctx)
	from, reason, socket := n.RuntimeFallback()
	if from != "docker" || reason != proto.RuntimeFallbackSocketPermissionDenied || socket == nil || socket.Account != "gpusvc" || socket.RestartPending {
		t.Fatalf("fallback = %q/%q/%+v, want docker/%s for gpusvc, no restart pending", from, reason, socket, proto.RuntimeFallbackSocketPermissionDenied)
	}
	for range 3 {
		n.recheckRuntimeFallback(ctx)
	}
	if got := strings.Count(logs.String(), "passed over"); got != 1 {
		t.Fatalf("%d passed-over lines for one change of finding, want 1:\n%s", got, logs.String())
	}

	setListed(fmt.Sprintf("uid=51001(gpusvc) gid=51001(gpusvc) groups=51001(gpusvc),%d(docker)", gid))
	n.recheckRuntimeFallback(ctx)
	if _, _, socket := n.RuntimeFallback(); socket == nil || !socket.RestartPending {
		t.Fatalf("socket = %+v after gpasswd, want restart pending", socket)
	}
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q: the process cannot hold the group before a restart", got)
	}
}

// Only the socket reason carries socket facts.
func TestOtherFallbacksCarryNoSocketFacts(t *testing.T) {
	n := fallbackNode(t)
	if _, _, socket := n.RuntimeFallback(); socket != nil {
		t.Fatalf("plugin-missing fallback carries socket %+v", socket)
	}
}

func TestAgentSupervisor(t *testing.T) {
	cgroup := filepath.Join(t.TempDir(), "cgroup")
	previous := selfCgroupFile
	selfCgroupFile = cgroup
	t.Cleanup(func() { selfCgroupFile = previous })
	write := func(s string) {
		if err := os.WriteFile(cgroup, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	state := t.TempDir()
	write("0::/system.slice/gpuworks-agent.service\n")
	if got := agentSupervisor(state); got != proto.AgentSupervisorSystemd {
		t.Fatalf("unit cgroup (v2): %q", got)
	}
	write("12:pids:/system.slice/gpuworks-agent.service\n1:name=systemd:/system.slice/gpuworks-agent.service\n")
	if got := agentSupervisor(state); got != proto.AgentSupervisorSystemd {
		t.Fatalf("unit cgroup (v1): %q", got)
	}
	write("0::/user.slice/user-1000.slice/session-2.scope\n")
	if got := agentSupervisor(state); got != "" {
		t.Fatalf("started by hand: %q", got)
	}
	if err := os.WriteFile(filepath.Join(state, "agent-restart.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := agentSupervisor(state); got != proto.AgentSupervisorNohup {
		t.Fatalf("nohup install: %q", got)
	}
	selfCgroupFile = filepath.Join(t.TempDir(), "missing")
	if got := agentSupervisor(""); got != "" {
		t.Fatalf("nothing known: %q", got)
	}
}
