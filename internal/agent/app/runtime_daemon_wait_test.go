package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// What `docker info` prints while dockerd is not up yet, and what it prints
// to a user outside the docker group: the first passes, the second does not.
const (
	daemonStarting   = `echo 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?' >&2; exit 1`
	daemonNoAccess   = `echo 'permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.47/info": dial unix /var/run/docker.sock: connect: permission denied' >&2; exit 1`
	daemonAnswering  = `echo 27.5.0`
	livenessQuestion = "'info --format {{.ServerVersion}}'"
)

// liveness makes the liveness question fail with failing for the first
// failures asks (all of them when failures < 0), then answer. It returns a
// function reporting how many times it was asked. PATH holds only the fake
// CLI, so the script keeps to shell builtins.
func (h *fakeDockerHost) liveness(failing string, failures int, info string) func() int {
	h.t.Helper()
	counter := filepath.Join(h.t.TempDir(), "asked")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		livenessQuestion + ") n=0; [ -f " + counter + " ] && read n < " + counter + "; n=$((n+1)); echo $n > " + counter + "\n" +
		"  if [ " + strconv.Itoa(failures) + " -lt 0 ] || [ $n -le " + strconv.Itoa(failures) + " ]; then " + failing + "; fi; " + daemonAnswering + " ;;\n" +
		"'info --format {{json .}}') " + info + " ;;\n" +
		"*) echo 'Error response from daemon: No such container: gpuworks' >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(h.bin, "docker"), []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
	return func() int {
		b, _ := os.ReadFile(counter)
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
}

func fastDaemonRetry(t *testing.T) {
	delay := daemonRetryDelay
	daemonRetryDelay = 5 * time.Millisecond
	t.Cleanup(func() { daemonRetryDelay = delay })
}

// At boot the agent can start before dockerd. Startup waits for a daemon
// that is installed and still coming up, instead of taking runveil and
// starting the node's instances there.
func TestStartupWaitsForADockerDaemonStillStarting(t *testing.T) {
	fastDaemonRetry(t)
	host := newFakeDockerHost(t, daemonWithNVIDIA)
	asked := host.liveness(daemonStarting, 3, daemonWithNVIDIA)
	got := detectDriverWaiting(slog.New(slog.DiscardHandler), t.TempDir(), "", nil, 10*time.Second)
	if driverName(got.driver) != "docker" || got.fallback != (runtimeFallback{}) {
		t.Fatalf("selection = %q/%+v, want docker with no fallback", driverName(got.driver), got.fallback)
	}
	if n := asked(); n != 4 {
		t.Fatalf("liveness asked %d times, want 4 (three refusals, then an answer)", n)
	}
}

// A daemon that never answers within the wait is passed over, and the node
// says so: the reason the minute-level recheck and the node page work from.
func TestStartupPassesOverADaemonThatNeverAnswers(t *testing.T) {
	fastDaemonRetry(t)
	host := newFakeDockerHost(t, daemonWithNVIDIA)
	host.liveness(daemonStarting, -1, daemonWithNVIDIA)
	got := detectDriverWaiting(slog.New(slog.DiscardHandler), t.TempDir(), "", nil, 50*time.Millisecond)
	if driverName(got.driver) != "udocker" {
		t.Fatalf("driver = %q, want udocker (runveil)", driverName(got.driver))
	}
	if got.fallback != (runtimeFallback{from: "docker", reason: proto.RuntimeFallbackDaemonNotReady}) {
		t.Fatalf("fallback = %+v, want docker/%s", got.fallback, proto.RuntimeFallbackDaemonNotReady)
	}
}

// Only a daemon that is coming up is waited for. One this agent may not use
// does not pass by waiting, and the recheck asks without waiting.
func TestNoWaitForADaemonTheAgentMayNotUseOrOnRecheck(t *testing.T) {
	fastDaemonRetry(t)
	for _, tc := range []struct {
		name, failing string
		wait          time.Duration
		reason        string
	}{
		{"permission denied at startup", daemonNoAccess, time.Minute, proto.RuntimeFallbackSocketPermissionDenied},
		{"not answering on recheck", daemonStarting, 0, proto.RuntimeFallbackDaemonNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeDockerHost(t, daemonWithNVIDIA)
			asked := host.liveness(tc.failing, -1, daemonWithNVIDIA)
			got := detectDriverWaiting(slog.New(slog.DiscardHandler), t.TempDir(), "", nil, tc.wait)
			if driverName(got.driver) != "udocker" || got.fallback.from != "docker" || got.fallback.reason != tc.reason {
				t.Fatalf("selection = %q/%+v, want udocker/docker/%s", driverName(got.driver), got.fallback, tc.reason)
			}
			if n := asked(); n != 1 {
				t.Fatalf("liveness asked %d times, want once", n)
			}
		})
	}
}

// A node that started on runveil because dockerd was not answering moves to
// Docker once it answers: at once when idle, and when the node is busy it
// says Docker is ready and switches once nothing would be disturbed.
func TestRecheckSwitchesToDockerOnceItsDaemonAnswers(t *testing.T) {
	host := newFakeDockerHost(t, daemonWithNVIDIA)
	host.installToolkit()
	host.liveness(daemonStarting, -1, daemonWithNVIDIA)
	n := fallbackNode(t)
	n.runtimeChoice.reset(true, runtimeFallback{from: "docker", reason: proto.RuntimeFallbackDaemonNotReady})
	ctx := context.Background()
	n.engine.SetDesiredFull(ctx, []proto.InstanceDesiredState{{UUID: "inst-1", DesiredState: "running", Image: "img:1"}})

	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q while dockerd is not answering", got)
	}
	if from, reason, _ := n.RuntimeFallback(); from != "docker" || reason != proto.RuntimeFallbackDaemonNotReady {
		t.Fatalf("fallback = %q/%q, want docker/%s", from, reason, proto.RuntimeFallbackDaemonNotReady)
	}

	host.liveness(daemonStarting, 0, daemonWithNVIDIA)
	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q: switched with an instance on the node", got)
	}
	if from, reason, _ := n.RuntimeFallback(); from != "docker" || reason != accelerator.RuntimeGPUReady {
		t.Fatalf("fallback = %q/%q, want docker/%s", from, reason, accelerator.RuntimeGPUReady)
	}

	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)
	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "docker" {
		t.Fatalf("runtime = %q, want docker once the node is idle", got)
	}
	if from, reason, _ := n.RuntimeFallback(); from != "" || reason != "" {
		t.Fatalf("fallback = %q/%q after switching", from, reason)
	}
}
