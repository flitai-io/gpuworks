package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/collector"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

type delayedLogDriver struct {
	imagePullTestDriver
	calls atomic.Int32
}

func (d *delayedLogDriver) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	if d.calls.Add(1) == 1 {
		return nil, errors.New("runtime log file not created yet")
	}
	return io.NopCloser(strings.NewReader("engine boot\nfatal startup detail\n")), nil
}

type metricsOnlyProber struct{}

func (metricsOnlyProber) Vendor() string                                 { return "ascend" }
func (metricsOnlyProber) Available() bool                                { return true }
func (metricsOnlyProber) Probe(context.Context) ([]proto.GPUInfo, error) { return nil, nil }
func (metricsOnlyProber) DriverVersion(context.Context) string           { return "test" }
func (metricsOnlyProber) Topology(context.Context) string                { return "" }
func (metricsOnlyProber) RuntimeInjection([]int) gpu.RuntimeInjection    { return gpu.RuntimeInjection{} }
func (metricsOnlyProber) Metrics(context.Context) ([]gpu.Metric, error) {
	return []gpu.Metric{{
		Index: 0, UUID: "ascend-0", Vendor: "ascend",
		Values: map[string]float64{"gpuworks_gpu_utilization": 42},
	}}, nil
}

func TestStartBackgroundCollectsWithoutContainerEngine(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := newNodeRuntime(log, nil, t.TempDir(), "none", metricsOnlyProber{}, nil)
	if n.engine != nil {
		t.Fatal("test requires a node without a container engine")
	}
	if got := n.runningInstances(); len(got) != 0 {
		t.Fatalf("runningInstances = %+v, want empty", got)
	}
	n.collectorRunner = func(ctx context.Context, col *collector.Collector) { col.RunOnce(ctx) }

	batches := make(chan proto.MetricBatch, 1)
	n.Bind(func(kind string, body any) error {
		if kind == proto.TMetrics {
			batches <- body.(proto.MetricBatch)
		}
		return nil
	})
	n.StartBackground(t.Context(), "node-no-runtime")

	select {
	case batch := <-batches:
		seenGPU, seenHost := false, false
		for _, sample := range batch.Samples {
			switch sample.Metric {
			case "gpuworks_gpu_utilization":
				seenGPU = sample.Labels["node"] == "node-no-runtime" && sample.Value == 42
			case "gpuworks_node_mem_total_bytes":
				seenHost = sample.Labels["node"] == "node-no-runtime"
			}
		}
		if !seenGPU || !seenHost {
			t.Fatalf("batch lacks GPU/host metrics: %+v", batch.Samples)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not emit without a container engine")
	}
}

// runningInstances is a hand-written adapter from engine specs to collector
// instances; this pins that it forwards DeploymentID rather than silently
// dropping it, which would leave every inference/dGPU sample from that
// instance without the "deployment" label the monitor groups replicas by.
func TestRunningInstancesForwardsDeploymentID(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	driver := &imagePullTestDriver{}
	n := newNodeRuntime(log, driver, t.TempDir(), "", metricsOnlyProber{}, nil)
	if n.engine == nil {
		t.Fatal("test requires a node with a container engine")
	}
	n.engine.SetDesiredFull(t.Context(), []proto.InstanceDesiredState{
		{UUID: "inst-1", DesiredState: "running", APIPort: 8000, DeploymentID: 42},
		{UUID: "inst-2", DesiredState: "running", APIPort: 8001}, // no deployment
	})

	got := n.runningInstances()
	byUUID := map[string]collector.RunningInstance{}
	for _, ri := range got {
		byUUID[ri.UUID] = ri
	}
	if len(byUUID) != 2 {
		t.Fatalf("runningInstances = %+v, want 2", got)
	}
	if byUUID["inst-1"].DeploymentID != 42 {
		t.Fatalf("inst-1 DeploymentID = %d, want 42", byUUID["inst-1"].DeploymentID)
	}
	if byUUID["inst-2"].DeploymentID != 0 {
		t.Fatalf("inst-2 DeploymentID = %d, want 0", byUUID["inst-2"].DeploymentID)
	}
}

func TestFollowOneRetriesUntilRuntimeLogExists(t *testing.T) {
	driver := &delayedLogDriver{}
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), driver, t.TempDir(), "", metricsOnlyProber{}, nil)
	batches := make(chan proto.LogBatch, 1)
	n.Bind(func(kind string, body any) error {
		if kind == proto.TLogs {
			batches <- body.(proto.LogBatch)
		}
		return nil
	})

	go n.followOne(t.Context(), "instance-with-delayed-log", nil)
	select {
	case batch := <-batches:
		if driver.calls.Load() < 2 {
			t.Fatalf("Logs calls = %d, want an initial failure followed by retry", driver.calls.Load())
		}
		if len(batch.Lines) != 2 || batch.Lines[0].Line != "engine boot" || batch.Lines[1].Line != "fatal startup detail" {
			t.Fatalf("log batch = %+v", batch.Lines)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("log follower did not retry after the runtime log appeared")
	}
}

// eofPerCallDriver hands out a fresh one-line stream on every Logs call, like a
// runtime whose container keeps getting replaced.
type eofPerCallDriver struct {
	imagePullTestDriver
	calls atomic.Int32
}

func (d *eofPerCallDriver) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	n := d.calls.Add(1)
	return io.NopCloser(strings.NewReader(fmt.Sprintf("stream %d\n", n))), nil
}

func TestFollowOneReleasesWhenStreamEnds(t *testing.T) {
	driver := &eofPerCallDriver{}
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), driver, t.TempDir(), "", metricsOnlyProber{}, nil)
	batches := make(chan proto.LogBatch, 8)
	n.Bind(func(kind string, body any) error {
		if kind == proto.TLogs {
			select {
			case batches <- body.(proto.LogBatch):
			default:
			}
		}
		return nil
	})

	f := n.instanceLogs.start(t.Context(), "inst-eof")
	released := make(chan string, 1)
	release := f.release
	f.release = func() { release(); released <- "inst-eof" }
	go n.followOne(f.ctx, "inst-eof", f)

	select {
	case batch := <-batches:
		if len(batch.Lines) != 1 || batch.Lines[0].Line != "stream 1" {
			t.Fatalf("log batch = %+v", batch.Lines)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not ship the first stream")
	}
	select {
	case uuid := <-released:
		if uuid != "inst-eof" {
			t.Fatalf("released uuid = %q", uuid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not release its shipper slot when the stream ended")
	}
	if n.logFollowerRestarts.Load() != 1 {
		t.Fatalf("logFollowerRestarts = %d, want 1", n.logFollowerRestarts.Load())
	}
	if !n.instanceLogs.wantsFollower("inst-eof") {
		t.Fatal("the shipper slot is still held after the follower exited")
	}
}

// TestLogShipperRestartsFollowerWhoseStreamEnds pins the shipper-loop behavior
// the release hook exists for: while an instance keeps running, a follower
// that exits must be restarted, otherwise a container restart permanently
// stops that instance's log shipping (observed live as hours of silence).
func TestLogShipperRestartsFollowerWhoseStreamEnds(t *testing.T) {
	driver := &eofPerCallDriver{}
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), driver, t.TempDir(), "", metricsOnlyProber{}, nil)
	var mu sync.Mutex
	shipped := map[string]bool{}
	n.Bind(func(kind string, body any) error {
		if kind == proto.TLogs {
			mu.Lock()
			for _, l := range body.(proto.LogBatch).Lines {
				shipped[l.Line] = true
			}
			mu.Unlock()
		}
		return nil
	})

	seen := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(shipped)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go n.runLogShipper(ctx, 20*time.Millisecond, logShipperDeps{
		running: func() []string { return []string{"inst-restart"} },
		follow: func(uuid string, f *logFollower) {
			go n.followOne(f.ctx, uuid, f)
		},
	})

	deadline := time.After(3 * time.Second)
	for seen() < 3 {
		select {
		case <-deadline:
			t.Fatalf("shipper restarted %d distinct streams, want >=3 (restart after stream end)", seen())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// blockingLogDriver keeps one endless log stream open per instance (like a
// healthy `logs -f`), so a follower stays alive until its context is cancelled.
type blockingLogDriver struct {
	imagePullTestDriver
	calls atomic.Int32
}

func (d *blockingLogDriver) Logs(ctx context.Context, _ string, _ bool, _ int) (io.ReadCloser, error) {
	d.calls.Add(1)
	pr, pw := io.Pipe()
	go func() {
		<-ctx.Done()
		pw.CloseWithError(io.EOF)
	}()
	go func() {
		for i := 0; ; i++ {
			time.Sleep(20 * time.Millisecond)
			if _, err := pw.Write([]byte(fmt.Sprintf("tick %d\n", i))); err != nil {
				return
			}
		}
	}()
	return pr, nil
}

// TestLogShipperStopsFollowerForStoppedInstance covers the other direction: a
// follower is cancelled once its instance leaves the running set.
func TestLogShipperStopsFollowerForStoppedInstance(t *testing.T) {
	driver := &blockingLogDriver{}
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), driver, t.TempDir(), "", metricsOnlyProber{}, nil)

	var running atomic.Value
	running.Store([]string{"inst-stop"})
	runningSet := func() []string { return running.Load().([]string) }

	cancelled := make(chan string, 1)
	ctx, cancel := context.WithCancel(t.Context())
	shipperDone := make(chan struct{})
	go func() {
		defer close(shipperDone)
		n.runLogShipper(ctx, 10*time.Millisecond, logShipperDeps{
			running: runningSet,
			follow: func(uuid string, f *logFollower) {
				go func() {
					<-f.ctx.Done()
					cancelled <- uuid
				}()
				go n.followOne(f.ctx, uuid, f)
			},
		})
	}()
	defer func() {
		cancel()
		select {
		case <-shipperDone:
		case <-time.After(2 * time.Second):
			t.Fatal("runLogShipper did not exit after cancel")
		}
	}()

	deadline := time.After(2 * time.Second)
	for driver.calls.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("shipper never started a follower")
		case <-time.After(10 * time.Millisecond):
		}
	}
	running.Store([]string{})
	select {
	case uuid := <-cancelled:
		if uuid != "inst-stop" {
			t.Fatalf("cancelled uuid = %q", uuid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shipper did not cancel the follower after the instance stopped running")
	}
}

// mixedStreamDriver ships one batch containing scraper noise plus real INFO
// and ERROR lines, then ends the stream.
type mixedStreamDriver struct {
	imagePullTestDriver
}

func (d *mixedStreamDriver) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(strings.Join([]string{
		`(APIServer pid=1) INFO:     127.0.0.1:49254 - "GET /metrics HTTP/1.1" 200 OK`,
		`(APIServer pid=97413) INFO 09-18 20:41:29 [utils.py:299] API server ready`,
		`(APIServer pid=83923) [ERROR] 2026-09-18-20:39:28 (PID:83923) ERR99999 UNKNOWN applicaiton exception`, //nolint:misspell // Preserve the exact error text emitted by the engine fixture.
	}, "\n"))), nil
}

// TestFollowOneDropsNoiseAndStampsLevels pins the two ingest-time behaviors the
// server relies on: scraper access logs never reach the store (they were 52% of
// rows on a live deployment), and real lines carry a parsed severity so the
// log center can filter and color by level without re-parsing.
func TestFollowOneDropsNoiseAndStampsLevels(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), &mixedStreamDriver{}, t.TempDir(), "", metricsOnlyProber{}, nil)
	batches := make(chan proto.LogBatch, 1)
	n.Bind(func(kind string, body any) error {
		if kind == proto.TLogs {
			batches <- body.(proto.LogBatch)
		}
		return nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.followOne(t.Context(), "inst-levels", nil)
	}()
	select {
	case batch := <-batches:
		if len(batch.Lines) != 2 {
			t.Fatalf("shipped %d lines, want 2 (noise line dropped): %+v", len(batch.Lines), batch.Lines)
		}
		first, second := batch.Lines[0], batch.Lines[1]
		if first.Level != "info" || !strings.Contains(first.Line, "API server ready") {
			t.Fatalf("first line = %+v, want info-stamped server-ready line", first)
		}
		if second.Level != "error" || !strings.Contains(second.Line, "ERR99999") {
			t.Fatalf("second line = %+v, want error-stamped ERR99999 line", second)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not ship the mixed stream")
	}
	<-done
	if n.logNoiseDropped.Load() != 1 {
		t.Fatalf("logNoiseDropped = %d, want 1", n.logNoiseDropped.Load())
	}
}

// TestLogShipperWaitsForContainerToExist pins the gate: a pulling instance
// sits in the desired set long before its container exists, and starting a
// follower then would emit a daemon "No such container" line every reconcile
// tick. The follower must start only once the runtime actually has the
// container; the gate failing open (probe error) keeps old behavior.
func TestLogShipperWaitsForContainerToExist(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, t.TempDir(), "none", metricsOnlyProber{}, nil)

	var exists atomic.Bool
	started := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	var shippers sync.WaitGroup
	defer func() {
		cancel()
		waitWithTimeout(t, &shippers, 2*time.Second, "runLogShipper")
	}()
	shippers.Add(1)
	go func() {
		defer shippers.Done()
		n.runLogShipper(ctx, 10*time.Millisecond, logShipperDeps{
			running:         func() []string { return []string{"inst-pulling"} },
			containerExists: func(_ context.Context, _ string) bool { return exists.Load() },
			follow: func(uuid string, _ *logFollower) {
				started <- uuid
			},
		})
	}()

	// Absent container: several reconcile ticks pass, no follower starts.
	select {
	case uuid := <-started:
		t.Fatalf("follower started before the container existed (%s)", uuid)
	case <-time.After(80 * time.Millisecond):
	}

	// Container appears: the next tick starts the follower.
	exists.Store(true)
	select {
	case uuid := <-started:
		if uuid != "inst-pulling" {
			t.Fatalf("followed %q, want inst-pulling", uuid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower never started after the container appeared")
	}

	// Gate failing open: a probe error must not block shipping.
	failing := make(chan string, 4)
	shippers.Add(1)
	go func() {
		defer shippers.Done()
		n.runLogShipper(ctx, 10*time.Millisecond, logShipperDeps{
			running:         func() []string { return []string{"inst-flaky"} },
			containerExists: func(_ context.Context, _ string) bool { return true }, // optimistic path
			follow:          func(uuid string, _ *logFollower) { failing <- uuid },
		})
	}()
	select {
	case <-failing:
	case <-time.After(2 * time.Second):
		t.Fatal("optimistic gate must still start followers")
	}
}

// waitWithTimeout waits for wg to complete, failing the test if it does not
// within timeout. Used to prove a runLogShipper goroutine actually exits
// after its context is cancelled, instead of leaking past the test.
func waitWithTimeout(t *testing.T, wg *sync.WaitGroup, timeout time.Duration, name string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("%s did not exit after cancel", name)
	}
}
