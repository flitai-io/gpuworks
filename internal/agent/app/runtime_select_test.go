package app

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Docker as the agent finds it: a CLI on PATH answering for a daemon on a
// local unix socket. `docker info --format {{.ServerVersion}}` is the
// liveness question auto-detection asks; `docker info --format {{json .}}`
// is the daemon's account of its runtimes, which decides GPU support.
const (
	daemonWithoutNVIDIA = `printf '%s\n' '{"ServerVersion":"27.5.0","Runtimes":{"runc":{}}}'`
	daemonWithNVIDIA    = `printf '%s\n' '{"ServerVersion":"27.5.0","Runtimes":{"runc":{},"nvidia":{}}}'`
	// The liveness question answered and the runtimes one did not: a
	// daemon restarting between the two, or busy past the timeout.
	daemonUnasked = `echo 'Cannot connect to the Docker daemon' >&2; exit 1`
)

type fakeDockerHost struct {
	t   *testing.T
	bin string
}

func newFakeDockerHost(t *testing.T, info string) *fakeDockerHost {
	t.Helper()
	if goruntime.GOOS != "linux" || (goruntime.GOARCH != "amd64" && goruntime.GOARCH != "arm64") {
		t.Skip("runveil, the fallback under test, runs on linux amd64/arm64 only")
	}
	t.Setenv("GPUWORKS_UDOCKER_LAUNCHER", "")
	t.Setenv("DOCKER_CONTEXT", "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	// A unix socket path must fit in sun_path, which a t.TempDir() path
	// under a long test name does not.
	sockDir, err := os.MkdirTemp("", "gwdk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	listener, err := net.Listen("unix", filepath.Join(sockDir, "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(sockDir, "docker.sock"))
	h := &fakeDockerHost{t: t, bin: bin}
	h.daemon(info)
	return h
}

// daemon sets what `docker info --format {{json .}}` answers.
func (h *fakeDockerHost) daemon(info string) {
	h.t.Helper()
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"'info --format {{.ServerVersion}}') echo 27.5.0 ;;\n" +
		"'info --format {{json .}}') " + info + " ;;\n" +
		// Everything else is about a container this daemon never had.
		"*) echo 'Error response from daemon: No such container: gpuworks' >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(h.bin, "docker"), []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

// installToolkit puts the NVIDIA Container Toolkit's CLI on PATH.
func (h *fakeDockerHost) installToolkit() {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.bin, "nvidia-ctk"), []byte("#!/bin/sh\necho 'NVIDIA Container Toolkit CLI version 1.17.3'\n"), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func driverName(d runtime.Driver) string {
	if d == nil {
		return ""
	}
	return d.Name()
}

// On an NVIDIA node, a Docker that cannot give containers the cards is
// passed over for runveil, and the node says which runtime it passed over
// and why. The reason names the remedy: no toolkit at all is "install it",
// a toolkit the daemon does not use is "configure the daemon".
func TestAutoDetectionPassesOverDockerThatCannotUseNVIDIACards(t *testing.T) {
	for _, tc := range []struct {
		name    string
		toolkit bool
		want    string
	}{
		{"no toolkit", false, accelerator.RuntimeGPUBlockedPluginMissing},
		{"toolkit the daemon does not use", true, accelerator.RuntimeGPUBlockedRuntimeUnconfigured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newFakeDockerHost(t, daemonWithoutNVIDIA)
			if tc.toolkit {
				host.installToolkit()
			}
			got := detectDriver(slog.New(slog.DiscardHandler), t.TempDir(), "", &gpu.SMIProber{})
			if driverName(got.driver) != "udocker" {
				t.Fatalf("driver = %q, want udocker (runveil)", driverName(got.driver))
			}
			if got.fallback != (runtimeFallback{from: "docker", reason: tc.want}) {
				t.Fatalf("fallback = %+v, want docker/%s", got.fallback, tc.want)
			}
		})
	}
}

// Everything that should keep Docker keeps it: a daemon that can serve the
// cards, one that could not be asked (taken on trust, as before — and
// marked so it is not switched to from a runtime known to work), a vendor
// whose cards runveil cannot serve, an operator who named Docker, and a
// caller that does not ask about accelerators at all.
func TestAutoDetectionKeepsDockerUnlessItCannotServeTheCards(t *testing.T) {
	ascend := &pluginProber{FixedProber: gpu.FixedProber{VendorName: "ascend"},
		plugin: runtime.PluginInfo{Name: accelerator.PluginAscendDockerRuntime}}
	for _, tc := range []struct {
		name       string
		daemon     string
		force      string
		prober     gpu.Prober
		unanswered bool
	}{
		{"daemon with the NVIDIA runtime", daemonWithNVIDIA, "", &gpu.SMIProber{}, false},
		{"daemon not asked", daemonUnasked, "", &gpu.SMIProber{}, true},
		{"Ascend without its Docker runtime", daemonWithoutNVIDIA, "", ascend, false},
		{"operator named docker", daemonWithoutNVIDIA, "docker", &gpu.SMIProber{}, false},
		{"no prober", daemonWithoutNVIDIA, "", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newFakeDockerHost(t, tc.daemon)
			got := detectDriver(slog.New(slog.DiscardHandler), t.TempDir(), tc.force, tc.prober)
			if driverName(got.driver) != "docker" {
				t.Fatalf("driver = %q, want docker", driverName(got.driver))
			}
			if got.fallback != (runtimeFallback{}) {
				t.Fatalf("fallback = %+v, want none", got.fallback)
			}
			if got.unanswered != tc.unanswered {
				t.Fatalf("unanswered = %v, want %v", got.unanswered, tc.unanswered)
			}
		})
	}
}

// The node built by auto-detection runs runveil, reports the fallback, and
// is not reported blocked: runveil needs no plugin, so nothing cordons it.
// A node whose operator named its runtime reports no fallback.
func TestNodeOnItsFallbackReportsItAndIsNotBlocked(t *testing.T) {
	newFakeDockerHost(t, daemonWithoutNVIDIA)
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, filepath.Join(t.TempDir(), "models"), "", &gpu.SMIProber{}, nil,
		engine.WithNetworkInterfaceResolver(nil))
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q, want udocker", got)
	}
	if from, reason, _ := n.RuntimeFallback(); from != "docker" || reason != accelerator.RuntimeGPUBlockedPluginMissing {
		t.Fatalf("fallback = %q/%q, want docker/%s", from, reason, accelerator.RuntimeGPUBlockedPluginMissing)
	}
	if _, blocked := n.RuntimeGPU(context.Background()); blocked != "" {
		t.Fatalf("blocked = %q on runveil", blocked)
	}

	forced := newNodeRuntime(slog.New(slog.DiscardHandler), nil, filepath.Join(t.TempDir(), "models"), "docker", &gpu.SMIProber{}, nil,
		engine.WithNetworkInterfaceResolver(nil))
	if got := forced.RuntimeDriverName(); got != "docker" {
		t.Fatalf("forced runtime = %q, want docker", got)
	}
	if from, reason, _ := forced.RuntimeFallback(); from != "" || reason != "" {
		t.Fatalf("forced node reports fallback %q/%q", from, reason)
	}
	if _, blocked := forced.RuntimeGPU(context.Background()); blocked != accelerator.RuntimeGPUBlockedPluginMissing {
		t.Fatalf("forced docker blocked = %q, want %s", blocked, accelerator.RuntimeGPUBlockedPluginMissing)
	}
}

// fallbackDriver stands in for runveil in the switch-back cases: a runtime
// that records what it is asked to do instead of starting PRoot.
type fallbackDriver struct{ *recording.Driver }

func (fallbackDriver) Name() string { return "udocker" }

// fallbackNode is an auto-detected node on runveil that passed Docker over.
func fallbackNode(t *testing.T) *NodeRuntime {
	t.Helper()
	n := newNodeRuntime(slog.New(slog.DiscardHandler), fallbackDriver{recording.New(filepath.Join(t.TempDir(), "rec.jsonl"))},
		filepath.Join(t.TempDir(), "models"), "", &gpu.SMIProber{}, nil, engine.WithNetworkInterfaceResolver(nil))
	n.runtimeChoice.reset(true, runtimeFallback{from: "docker", reason: accelerator.RuntimeGPUBlockedPluginMissing})
	return n
}

func waitIdle(t *testing.T, n *NodeRuntime) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !n.engine.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("engine never became idle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Once Docker can serve the cards, an idle node switches back to it on the
// next recheck and stops reporting a fallback. Until then the recheck
// leaves it where it is.
func TestIdleNodeSwitchesBackOnceDockerCanServeTheCards(t *testing.T) {
	host := newFakeDockerHost(t, daemonWithoutNVIDIA)
	n := fallbackNode(t)
	ctx := context.Background()
	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)

	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q while Docker still cannot serve the cards", got)
	}
	if from, reason, _ := n.RuntimeFallback(); from != "docker" || reason != accelerator.RuntimeGPUBlockedPluginMissing {
		t.Fatalf("fallback = %q/%q", from, reason)
	}

	host.installToolkit()
	host.daemon(daemonWithNVIDIA)
	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "docker" {
		t.Fatalf("runtime = %q, want docker after the daemon gained the NVIDIA runtime", got)
	}
	if from, reason, _ := n.RuntimeFallback(); from != "" || reason != "" {
		t.Fatalf("fallback = %q/%q after switching back", from, reason)
	}
}

// A node with an instance stays on runveil when Docker becomes able to
// serve the cards — switching recreates every container — and says Docker
// is ready instead. Once the instance is gone it switches.
func TestBusyNodeReportsDockerReadyAndSwitchesWhenIdle(t *testing.T) {
	host := newFakeDockerHost(t, daemonWithoutNVIDIA)
	n := fallbackNode(t)
	ctx := context.Background()
	n.engine.SetDesiredFull(ctx, []proto.InstanceDesiredState{{UUID: "inst-1", DesiredState: "running", Image: "img:1"}})

	host.installToolkit()
	host.daemon(daemonWithNVIDIA)
	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q: switched with an instance on the node", got)
	}
	if from, reason, _ := n.RuntimeFallback(); from != "docker" || reason != accelerator.RuntimeGPUReady {
		t.Fatalf("fallback = %q/%q, want docker/%s", from, reason, accelerator.RuntimeGPUReady)
	}

	// The daemon lost the runtime again while the instance ran: ready no more.
	host.daemon(daemonWithoutNVIDIA)
	n.recheckRuntimeFallback(ctx)
	if from, reason, _ := n.RuntimeFallback(); from != "docker" || reason != accelerator.RuntimeGPUBlockedRuntimeUnconfigured {
		t.Fatalf("fallback = %q/%q after the daemon lost the runtime", from, reason)
	}

	host.daemon(daemonWithNVIDIA)
	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)
	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "docker" {
		t.Fatalf("runtime = %q, want docker once the node is idle", got)
	}
}

// A daemon that cannot be asked is no reason to leave runveil, which is
// known to work: nothing about Docker's GPU support is known.
func TestRecheckDoesNotLeaveRunveilForAnUnaskedDaemon(t *testing.T) {
	newFakeDockerHost(t, daemonUnasked)
	n := fallbackNode(t)
	ctx := context.Background()
	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)
	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q, want udocker", got)
	}
	if from, reason, _ := n.RuntimeFallback(); from != "docker" || reason != accelerator.RuntimeGPUBlockedPluginMissing {
		t.Fatalf("fallback = %q/%q: an unasked daemon changed the last answer", from, reason)
	}
}

// The server replays the preference on every connect. A replayed "auto"
// does not recreate running instances; the same preference chosen by an
// operator does, because they asked for it. Naming a runtime ends the
// agent's own choice: nothing is passed over and nothing is rechecked.
func TestRuntimePreferenceReplayOnlySwitchesAnIdleNode(t *testing.T) {
	host := newFakeDockerHost(t, daemonWithoutNVIDIA)
	n := fallbackNode(t)
	ctx := context.Background()
	n.engine.SetDesiredFull(ctx, []proto.InstanceDesiredState{{UUID: "inst-1", DesiredState: "running", Image: "img:1"}})
	host.daemon(daemonWithNVIDIA)

	n.OnStateSync(ctx, proto.DesiredState{RuntimePreference: proto.RuntimePreferenceAuto,
		Instances: []proto.InstanceDesiredState{{UUID: "inst-1", DesiredState: "running", Image: "img:1"}}})
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q: a replayed preference recreated a running instance", got)
	}
	if from, reason, _ := n.RuntimeFallback(); from != "docker" || reason != accelerator.RuntimeGPUReady {
		t.Fatalf("fallback = %q/%q after the replay, want docker/%s", from, reason, accelerator.RuntimeGPUReady)
	}

	if err := n.ApplyRuntimePreference(ctx, proto.RuntimePreferenceUdocker); err != nil {
		t.Fatal(err)
	}
	if auto, fallback := n.runtimeChoice.get(); auto || fallback != (runtimeFallback{}) {
		t.Fatalf("after naming udocker: auto=%v fallback=%+v", auto, fallback)
	}
	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q: a named runtime was rechecked", got)
	}
}

// A node that started on a Docker able to serve the cards and lost that
// (the daemon restarted without its NVIDIA runtime) is blocked by the next
// report; the recheck then moves an idle node to runveil, where it is no
// longer blocked, instead of leaving it cordoned.
func TestIdleNodeOnABlockedDockerFallsBackToRunveil(t *testing.T) {
	host := newFakeDockerHost(t, daemonWithNVIDIA)
	host.installToolkit()
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, filepath.Join(t.TempDir(), "models"), "", &gpu.SMIProber{}, nil,
		engine.WithNetworkInterfaceResolver(nil))
	if got := n.RuntimeDriverName(); got != "docker" {
		t.Fatalf("runtime = %q, want docker", got)
	}
	ctx := context.Background()
	n.recheckRuntimeFallback(ctx) // nothing to watch: stays, asks nothing
	if got := n.RuntimeDriverName(); got != "docker" {
		t.Fatalf("runtime = %q after a recheck with nothing to watch", got)
	}

	host.daemon(daemonWithoutNVIDIA)
	if _, blocked := n.RuntimeGPU(ctx); blocked != accelerator.RuntimeGPUBlockedRuntimeUnconfigured {
		t.Fatalf("blocked = %q", blocked)
	}
	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)
	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q, want udocker", got)
	}
	if from, reason, _ := n.RuntimeFallback(); from != "docker" || reason != accelerator.RuntimeGPUBlockedRuntimeUnconfigured {
		t.Fatalf("fallback = %q/%q", from, reason)
	}
	if _, blocked := n.RuntimeGPU(ctx); blocked != "" {
		t.Fatalf("blocked = %q on runveil", blocked)
	}
}

// An agent started with --driver docker whose server preference is auto
// hands the choice back to auto-detection on the replay. Busy, it keeps
// Docker for now — but the choice is the agent's from then on, so once the
// node is idle the recheck moves it to runveil.
func TestAReplayedAutoOnABusyNodeKeepsWatching(t *testing.T) {
	newFakeDockerHost(t, daemonWithoutNVIDIA)
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, filepath.Join(t.TempDir(), "models"), "docker", &gpu.SMIProber{}, nil,
		engine.WithNetworkInterfaceResolver(nil))
	ctx := context.Background()
	if _, blocked := n.RuntimeGPU(ctx); blocked == "" {
		t.Fatal("docker without the NVIDIA runtime is not blocked")
	}
	busy := []proto.InstanceDesiredState{{UUID: "inst-1", DesiredState: "running", Image: "img:1"}}
	n.OnStateSync(ctx, proto.DesiredState{RuntimePreference: proto.RuntimePreferenceAuto, Instances: busy})
	if got := n.RuntimeDriverName(); got != "docker" {
		t.Fatalf("runtime = %q: a replay recreated a running instance", got)
	}
	if auto, _ := n.runtimeChoice.get(); !auto {
		t.Fatal("the replayed auto did not hand the choice back to auto-detection")
	}
	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)
	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "udocker" {
		t.Fatalf("runtime = %q, want udocker once idle", got)
	}
}

// The watch runs the recheck on its own, and stops with the agent.
func TestTheFallbackWatchSwitchesBackOnItsOwnAndStops(t *testing.T) {
	interval := runtimeFallbackInterval
	runtimeFallbackInterval = 5 * time.Millisecond
	t.Cleanup(func() { runtimeFallbackInterval = interval })
	host := newFakeDockerHost(t, daemonWithNVIDIA)
	host.installToolkit()
	n := fallbackNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)
	done := make(chan struct{})
	go func() { n.watchRuntimeFallback(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for n.RuntimeDriverName() != "docker" {
		if time.Now().After(deadline) {
			t.Fatalf("runtime = %q, the watch never switched back", n.RuntimeDriverName())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch outlived its context")
	}
}

// The recheck runs every minute on a node that fell back, and agent logs
// are shipped to the server: a round that finds what the last one found
// logs nothing, and a change of reason is logged once.
func TestTheRecheckLogsAFallbackOnlyWhenItChanges(t *testing.T) {
	host := newFakeDockerHost(t, daemonWithoutNVIDIA)
	var logs bytes.Buffer
	n := newNodeRuntime(slog.New(slog.NewTextHandler(&logs, nil)), fallbackDriver{recording.New(filepath.Join(t.TempDir(), "rec.jsonl"))},
		filepath.Join(t.TempDir(), "models"), "", &gpu.SMIProber{}, nil, engine.WithNetworkInterfaceResolver(nil))
	n.runtimeChoice.reset(true, runtimeFallback{from: "docker", reason: accelerator.RuntimeGPUBlockedPluginMissing})
	ctx := context.Background()
	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)
	for range 3 {
		n.recheckRuntimeFallback(ctx)
	}
	if got := strings.Count(logs.String(), "passed over"); got != 0 {
		t.Fatalf("%d passed-over lines for an unchanged fallback:\n%s", got, logs.String())
	}
	host.installToolkit() // the daemon still has no NVIDIA runtime: unconfigured now
	for range 3 {
		n.recheckRuntimeFallback(ctx)
	}
	if got := strings.Count(logs.String(), "passed over"); got != 1 {
		t.Fatalf("%d passed-over lines after the reason changed once, want 1:\n%s", got, logs.String())
	}
}
