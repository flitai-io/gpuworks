package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// fakeLateProber is an injectable gpu.Prober for watchForLateAccelerator's
// tests: only Vendor and Available matter to the watch, so every other
// method answers emptily.
type fakeLateProber struct {
	vendor    string
	available bool
}

func (p *fakeLateProber) Vendor() string  { return p.vendor }
func (p *fakeLateProber) Available() bool { return p.available }
func (p *fakeLateProber) Probe(context.Context) ([]proto.GPUInfo, error) {
	return nil, nil
}
func (p *fakeLateProber) DriverVersion(context.Context) string          { return "" }
func (p *fakeLateProber) Topology(context.Context) string               { return "" }
func (p *fakeLateProber) Metrics(context.Context) ([]gpu.Metric, error) { return nil, nil }
func (p *fakeLateProber) RuntimeInjection([]int) gpu.RuntimeInjection {
	return gpu.RuntimeInjection{}
}

// lateWatchNode is an idle node with the late-accelerator restart hook
// replaced by a counted probe, in place of replacing the test binary.
func lateWatchNode(t *testing.T) (*NodeRuntime, *restartProbe) {
	t.Helper()
	n := newNodeRuntime(slog.New(slog.DiscardHandler), recording.New(filepath.Join(t.TempDir(), "rec.jsonl")),
		filepath.Join(t.TempDir(), "models"), "", &gpu.FixedProber{VendorName: "nvidia"}, nil, engine.WithNetworkInterfaceResolver(nil))
	n.engine.SetDesiredFull(context.Background(), nil)
	waitIdle(t, n)
	probe := newRestartProbe(nil)
	old := lateAcceleratorRestart
	lateAcceleratorRestart = probe.restart
	t.Cleanup(func() { lateAcceleratorRestart = old })
	return n, probe
}

// tickN sends n ticks on ch, one at a time, waiting for each to be
// received before sending the next (an unbuffered channel blocks
// otherwise, which would make the test send ticks the watch never reads).
func tickN(t *testing.T, ch chan time.Time, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case ch <- time.Now():
		case <-time.After(5 * time.Second):
			t.Fatalf("tick %d: the watch never read it", i)
		}
	}
}

// Appears after N ticks: the watch restarts exactly once and stops.
func TestWatchForLateAcceleratorRestartsOnceTheToolAnswers(t *testing.T) {
	n, probe := lateWatchNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	var calls atomic.Int64
	redetect := func() gpu.Prober {
		n := calls.Add(1)
		return &fakeLateProber{vendor: "nvidia", available: n >= 3}
	}
	done := make(chan struct{})
	go func() { n.watchForLateAccelerator(ctx, redetect, ticks); close(done) }()

	tickN(t, ticks, 2)
	time.Sleep(20 * time.Millisecond)
	if probe.count() != 0 {
		t.Fatalf("restarted before the tool answered: %d calls, %d redetects", probe.count(), calls.Load())
	}

	tickN(t, ticks, 1)
	probe.wait(t)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch kept running after it restarted")
	}
	if probe.count() != 1 {
		t.Fatalf("restarts = %d, want 1", probe.count())
	}
}

// Never appears: redetect is asked every tick and the watch never restarts
// or spins between ticks (it only reacts to a value on ticks).
func TestWatchForLateAcceleratorNeverAppearingDoesNotBusyLoop(t *testing.T) {
	n, probe := lateWatchNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	var calls atomic.Int64
	redetect := func() gpu.Prober {
		calls.Add(1)
		return &fakeLateProber{vendor: "", available: false}
	}
	done := make(chan struct{})
	go func() { n.watchForLateAccelerator(ctx, redetect, ticks); close(done) }()

	tickN(t, ticks, 5)
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 5 {
		t.Fatalf("redetect called %d times, want exactly 5 (once per tick)", got)
	}
	if probe.count() != 0 {
		t.Fatal("a tool that never answers restarted the agent")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not stop when ctx ended")
	}
}

// A flaky or disappearing tool (redetect alternates, or returns nil) never
// crashes the watch, and it still restarts once the tool settles on
// available.
func TestWatchForLateAcceleratorToleratesAFlickeringTool(t *testing.T) {
	n, probe := lateWatchNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	sequence := []gpu.Prober{
		nil, // redetect itself found nothing this time
		&fakeLateProber{vendor: "nvidia", available: false}, // the vendor is known but the CLI still fails
		nil, // flickers again
		&fakeLateProber{vendor: "nvidia", available: true}, // settles; the watch restarts and stops reading ticks
	}
	i := 0
	redetect := func() gpu.Prober {
		p := sequence[i]
		if i < len(sequence)-1 {
			i++
		}
		return p
	}
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("watchForLateAccelerator panicked: %v", r)
			}
			close(done)
		}()
		n.watchForLateAccelerator(ctx, redetect, ticks)
	}()

	tickN(t, ticks, len(sequence))
	probe.wait(t)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch never stopped after restarting")
	}
	if probe.count() != 1 {
		t.Fatalf("restarts = %d, want 1", probe.count())
	}
}

// Instances still run on the node: the restart is deferred, not forced,
// and tried again once the node is idle.
func TestWatchForLateAcceleratorDefersWhileTheNodeIsBusy(t *testing.T) {
	n, probe := lateWatchNode(t)
	n.engine.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{
		{UUID: "inst-1", DesiredState: "running", Image: "img:1"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	redetect := func() gpu.Prober { return &fakeLateProber{vendor: "nvidia", available: true} }
	done := make(chan struct{})
	go func() { n.watchForLateAccelerator(ctx, redetect, ticks); close(done) }()

	tickN(t, ticks, 3)
	time.Sleep(20 * time.Millisecond)
	if probe.count() != 0 {
		t.Fatal("restarted while instances were running")
	}

	n.engine.SetDesiredFull(context.Background(), nil)
	waitIdle(t, n)
	tickN(t, ticks, 1)
	probe.wait(t)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not stop after restarting")
	}
	if probe.count() != 1 {
		t.Fatalf("restarts = %d, want 1", probe.count())
	}
}

// A failed restart is logged and retried on the next tick, not fatal to
// the watch.
func TestWatchForLateAcceleratorRetriesAfterAFailedRestart(t *testing.T) {
	n, probe := lateWatchNode(t)
	probe.setErr(errors.New("exec failed"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	redetect := func() gpu.Prober { return &fakeLateProber{vendor: "nvidia", available: true} }
	done := make(chan struct{})
	go func() { n.watchForLateAccelerator(ctx, redetect, ticks); close(done) }()

	tickN(t, ticks, 1)
	probe.wait(t)
	time.Sleep(20 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the watch stopped after a failed restart")
	default:
	}

	probe.setErr(nil)
	tickN(t, ticks, 1)
	probe.wait(t)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch never stopped after the retry succeeded")
	}
	if probe.count() != 2 {
		t.Fatalf("restart attempts = %d, want 2 (one failed, one succeeded)", probe.count())
	}
}
