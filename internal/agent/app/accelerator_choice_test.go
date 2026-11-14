package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// logBuffer is a log sink the engine's goroutines may write to while the
// test reads it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// restartProbe counts the agent restarts a switch asks for, in place of
// replacing the test process.
type restartProbe struct {
	mu    sync.Mutex
	calls int
	err   error
	done  chan struct{}
}

func newRestartProbe(err error) *restartProbe {
	return &restartProbe{err: err, done: make(chan struct{}, 8)}
}

func (r *restartProbe) restart() error {
	r.mu.Lock()
	r.calls++
	err := r.err
	r.mu.Unlock()
	r.done <- struct{}{}
	return err
}

// setErr changes what the next restart() call returns; a test that wants a
// restart to fail and then succeed sets this between the two instead of
// writing the field directly, which would race restart() reading it from
// another goroutine.
func (r *restartProbe) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *restartProbe) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *restartProbe) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never restarted")
	}
}

// acceleratorNode is a node managing Ascend cards on a host that also has
// NVIDIA ones, chosen automatically, with its restarts observed.
func acceleratorNode(t *testing.T, logs *logBuffer, restartErr error) (*NodeRuntime, *restartProbe) {
	t.Helper()
	old := acceleratorRestartDelay
	acceleratorRestartDelay = 0
	t.Cleanup(func() { acceleratorRestartDelay = old })
	log := slog.New(slog.DiscardHandler)
	if logs != nil {
		log = slog.New(slog.NewTextHandler(logs, nil))
	}
	n := newNodeRuntime(log, recording.New(filepath.Join(t.TempDir(), "rec.jsonl")),
		filepath.Join(t.TempDir(), "models"), "", &gpu.FixedProber{VendorName: "ascend"}, nil, engine.WithNetworkInterfaceResolver(nil))
	probe := newRestartProbe(restartErr)
	n.accelerator.preference = proto.AcceleratorPreferenceAuto
	n.accelerator.restart = probe.restart
	return n, probe
}

func storedPreference(t *testing.T, n *NodeRuntime) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(n.stateDir, acceleratorPreferenceFile))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

// The choice lives in the state directory, where the next start reads it
// before probing anything. Automatic is the file's absence, and anything
// the agent cannot use — garbage, a vendor it has no backend for, Apple,
// whose cards no bus lists — reads as automatic too.
func TestTheAcceleratorChoiceSurvivesARestartInTheStateDirectory(t *testing.T) {
	dir := t.TempDir()
	if got := readAcceleratorPreference(dir); got != proto.AcceleratorPreferenceAuto {
		t.Fatalf("no file reads as %q", got)
	}
	for _, vendor := range []string{"nvidia", "ascend", "hygon", "ppu"} {
		if err := writeAcceleratorPreference(dir, vendor); err != nil {
			t.Fatal(err)
		}
		if got := readAcceleratorPreference(dir); got != vendor {
			t.Fatalf("stored %q, read %q", vendor, got)
		}
	}
	if err := writeAcceleratorPreference(dir, proto.AcceleratorPreferenceAuto); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, acceleratorPreferenceFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("automatic left the file behind: %v", err)
	}
	if err := writeAcceleratorPreference(dir, proto.AcceleratorPreferenceAuto); err != nil {
		t.Fatalf("automatic twice: %v", err)
	}
	for _, junk := range []string{"apple", "intel", "", "  \n"} {
		if err := os.WriteFile(filepath.Join(dir, acceleratorPreferenceFile), []byte(junk), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readAcceleratorPreference(dir); got != proto.AcceleratorPreferenceAuto {
			t.Fatalf("file %q read as %q", junk, got)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, acceleratorPreferenceFile), []byte(" NVIDIA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readAcceleratorPreference(dir); got != "nvidia" {
		t.Fatalf("hand-written NVIDIA read as %q", got)
	}
}

// ClearAcceleratorPreference is what a kube node's enrollment calls when
// its identity's node UUID changes: the saved choice belonged to whichever
// cluster entry last enrolled this state directory.
func TestClearAcceleratorPreferenceResetsToAutomatic(t *testing.T) {
	dir := t.TempDir()
	if err := writeAcceleratorPreference(dir, "nvidia"); err != nil {
		t.Fatal(err)
	}
	if err := ClearAcceleratorPreference(dir); err != nil {
		t.Fatal(err)
	}
	if got := readAcceleratorPreference(dir); got != proto.AcceleratorPreferenceAuto {
		t.Fatalf("after ClearAcceleratorPreference = %q, want automatic", got)
	}
	if _, err := os.Stat(filepath.Join(dir, acceleratorPreferenceFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ClearAcceleratorPreference left the file behind: %v", err)
	}
	// Already automatic: no error.
	if err := ClearAcceleratorPreference(dir); err != nil {
		t.Fatalf("clearing an already-automatic preference: %v", err)
	}
}

// An idle node switches: it stores the choice and restarts to manage the
// other vendor's cards. Choosing what it already manages does neither.
func TestAnIdleNodeStoresTheChoiceAndRestarts(t *testing.T) {
	n, probe := acceleratorNode(t, nil, nil)
	ctx := context.Background()
	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)

	if got, err := n.SetAccelerator("auto"); err != nil || got != proto.AcceleratorPreferenceAuto {
		t.Fatalf("SetAccelerator(auto) = %q, %v", got, err)
	}
	if probe.count() != 0 {
		t.Fatal("choosing what the node runs restarted it")
	}

	if got, err := n.SetAccelerator(" NVIDIA "); err != nil || got != "nvidia" {
		t.Fatalf("SetAccelerator(nvidia) = %q, %v", got, err)
	}
	probe.wait(t)
	if got := storedPreference(t, n); got != "nvidia" {
		t.Fatalf("stored %q", got)
	}
	// The process is on its way out: a second request neither stores nor
	// schedules anything.
	if _, err := n.SetAccelerator("hygon"); err != nil {
		t.Fatal(err)
	}
	if got := storedPreference(t, n); got != "nvidia" {
		t.Fatalf("a request after the restart was scheduled stored %q", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := probe.count(); got != 1 {
		t.Fatalf("restarts = %d", got)
	}
}

// Anything the node runs would lose its cards, so the operator's switch is
// refused and nothing is stored.
func TestABusyNodeRefusesTheSwitch(t *testing.T) {
	n, probe := acceleratorNode(t, nil, nil)
	n.engine.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{{UUID: "inst-1", DesiredState: "running", Image: "img:1"}})

	if _, err := n.SetAccelerator("nvidia"); !errors.Is(err, errAcceleratorBusy) {
		t.Fatalf("err = %v, want %v", err, errAcceleratorBusy)
	}
	if got := storedPreference(t, n); got != "" {
		t.Fatalf("a refused switch stored %q", got)
	}
	time.Sleep(20 * time.Millisecond)
	if probe.count() != 0 {
		t.Fatal("a refused switch restarted the agent")
	}
}

// Before the server's desired state arrives the agent cannot know nothing
// runs, so it refuses then too.
func TestASwitchBeforeTheDesiredStateIsKnownIsRefused(t *testing.T) {
	n, _ := acceleratorNode(t, nil, nil)
	if _, err := n.SetAccelerator("nvidia"); !errors.Is(err, errAcceleratorBusy) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnUnknownAcceleratorIsRefused(t *testing.T) {
	n, probe := acceleratorNode(t, nil, nil)
	n.engine.SetDesiredFull(context.Background(), nil)
	waitIdle(t, n)
	for _, bad := range []string{"apple", "intel", "nvidia,ascend"} {
		if _, err := n.SetAccelerator(bad); err == nil || !strings.Contains(err.Error(), "accelerator must be") {
			t.Fatalf("SetAccelerator(%q) err = %v", bad, err)
		}
	}
	if probe.count() != 0 || storedPreference(t, n) != "" {
		t.Fatal("a refused choice changed something")
	}
}

// A replayed choice that differs waits while anything runs, logging that
// once, and applies at the first sync that finds the node idle.
func TestAReplayedChoiceWaitsForAnIdleNode(t *testing.T) {
	var logs logBuffer
	n, probe := acceleratorNode(t, &logs, nil)
	ctx := context.Background()
	busy := proto.DesiredState{AcceleratorPreference: "nvidia", Instances: []proto.InstanceDesiredState{{UUID: "inst-1", DesiredState: "running", Image: "img:1"}}}
	n.OnStateSync(ctx, busy)
	n.OnStateSync(ctx, busy)
	time.Sleep(20 * time.Millisecond)
	if probe.count() != 0 || storedPreference(t, n) != "" {
		t.Fatal("a busy node switched on a replayed choice")
	}
	if got := strings.Count(logs.String(), "accelerator choice waits"); got != 1 {
		t.Fatalf("logged the wait %d times:\n%s", got, logs.String())
	}

	n.OnStateSync(ctx, proto.DesiredState{AcceleratorPreference: "nvidia"})
	waitIdle(t, n)
	if probe.count() == 0 {
		// The engine may still have been tearing the instance down.
		n.OnStateSync(ctx, proto.DesiredState{AcceleratorPreference: "nvidia"})
	}
	probe.wait(t)
	if got := storedPreference(t, n); got != "nvidia" {
		t.Fatalf("stored %q", got)
	}
}

// A sync that states no choice (a server that predates it, or a node
// nobody chose for) and one that states what the node runs leave it alone.
func TestAReplayWithoutADifferentChoiceChangesNothing(t *testing.T) {
	n, probe := acceleratorNode(t, nil, nil)
	ctx := context.Background()
	n.OnStateSync(ctx, proto.DesiredState{})
	n.OnStateSync(ctx, proto.DesiredState{AcceleratorPreference: "auto"})
	waitIdle(t, n)
	time.Sleep(20 * time.Millisecond)
	if probe.count() != 0 || storedPreference(t, n) != "" {
		t.Fatal("a replay without a different choice switched")
	}
}

// When the restart itself fails, the stored choice still applies at the
// next start, and a later request may try again.
func TestAFailedRestartCanBeRetried(t *testing.T) {
	var logs logBuffer
	n, probe := acceleratorNode(t, &logs, errors.New("exec format error"))
	n.engine.SetDesiredFull(context.Background(), nil)
	waitIdle(t, n)
	if _, err := n.SetAccelerator("nvidia"); err != nil {
		t.Fatal(err)
	}
	probe.wait(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		n.accelerator.mu.Lock()
		switching := n.accelerator.switching
		n.accelerator.mu.Unlock()
		if !switching {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still switching after the restart failed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "exec format error") {
		t.Fatalf("the failure was not logged:\n%s", logs.String())
	}
	if got := storedPreference(t, n); got != "nvidia" {
		t.Fatalf("stored %q", got)
	}
	if _, err := n.SetAccelerator("nvidia"); err != nil {
		t.Fatal(err)
	}
	probe.wait(t)
}

// A state directory the agent cannot write to fails the switch instead of
// restarting into the same vendor.
func TestAnUnwritableStateDirectoryFailsTheSwitch(t *testing.T) {
	n, probe := acceleratorNode(t, nil, nil)
	n.engine.SetDesiredFull(context.Background(), nil)
	waitIdle(t, n)
	n.stateDir = filepath.Join(t.TempDir(), "missing", "dir")
	if _, err := n.SetAccelerator("nvidia"); err == nil || !strings.Contains(err.Error(), "store accelerator choice") {
		t.Fatalf("err = %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if probe.count() != 0 {
		t.Fatal("restarted without storing the choice")
	}
}

// The RPC carries the choice as {"accelerator": ...} and answers with the
// stored spelling.
func TestTheSetAcceleratorRPC(t *testing.T) {
	n, probe := acceleratorNode(t, nil, nil)
	ctx := context.Background()
	n.engine.SetDesiredFull(ctx, nil)
	waitIdle(t, n)
	if _, err := n.OnRPC(ctx, proto.RPCNodeSetAccelerator, []byte(`{`)); err == nil {
		t.Fatal("a malformed request was accepted")
	}
	got, err := n.OnRPC(ctx, proto.RPCNodeSetAccelerator, []byte(`{"accelerator":"NVIDIA"}`))
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := got.(map[string]string); !ok || m["accelerator"] != "nvidia" {
		t.Fatalf("reply = %#v", got)
	}
	probe.wait(t)
}

// Only an agent that probes the real host reports the vendors on its bus.
func TestAnAgentGivenItsProberReportsNoBus(t *testing.T) {
	n, _ := acceleratorNode(t, nil, nil)
	if got := n.AcceleratorVendors(); got != nil {
		t.Fatalf("vendors = %#v", got)
	}
}
