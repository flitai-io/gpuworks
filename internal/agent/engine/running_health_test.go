package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
)

func waitHealthProbes(t *testing.T, e *Engine) {
	t.Helper()
	e.mu.Lock()
	var done []<-chan struct{}
	for _, task := range e.healthProbes {
		done = append(done, task.done)
	}
	e.mu.Unlock()
	for _, task := range done {
		select {
		case <-task:
		case <-time.After(5 * time.Second):
			t.Fatal("HTTP probe task did not finish")
		}
	}
}

func TestRunningHealthRetriesBeforeRetiring(t *testing.T) {
	for _, recover := range []bool{true, false} {
		t.Run(fmt.Sprintf("recovers=%v", recover), func(t *testing.T) {
			var checking atomic.Bool
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" && checking.Load() {
					call := calls.Add(1)
					if !recover || call < 3 {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
				}
			}))
			t.Cleanup(server.Close)
			driver := newLivenessDriver()
			e, reports := newLivenessEngine(t, driver)
			e.healthProbeInterval = time.Millisecond
			spec := runningSpec("health-retries", serverPort(t, server))
			spec.Engine = engines.VLLM
			serve(t, e, reports, spec)
			checking.Store(true)
			e.checkLiveness(context.Background())
			waitHealthProbes(t, e)
			if calls.Load() != 3 {
				t.Fatalf("probe calls=%d, want 3", calls.Load())
			}
			failed := reports.states(proto.InstFailed)
			if recover {
				if len(failed) != 0 || !driver.exists(containerName(spec.UUID)) {
					t.Fatalf("transient failure retired the instance: %+v", reports.snapshot())
				}
			} else {
				if len(failed) != 1 || !strings.Contains(failed[0].Message, "HTTP 503") || driver.exists(containerName(spec.UUID)) {
					t.Fatalf("persistent HTTP failure was not retired: %+v", reports.snapshot())
				}
				for range 3 {
					e.checkLiveness(context.Background())
				}
				if calls.Load() != 3 || len(reports.states(proto.InstFailed)) != 1 {
					t.Fatal("failed instance was probed or reported again")
				}
			}
		})
	}
}

func TestRunningHealthProbeDoesNotBlockContainerChecksOrOverlap(t *testing.T) {
	var block atomic.Bool
	var calls atomic.Int32
	entered := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && block.Load() {
			calls.Add(1)
			entered <- struct{}{}
			<-r.Context().Done()
		}
	}))
	t.Cleanup(server.Close)
	driver := newLivenessDriver()
	e, reports := newLivenessEngine(t, driver)
	e.healthProbeTimeout = time.Minute
	slow := runningSpec("slow-health", serverPort(t, server))
	other := runningSpec("other-container", healthyEngine(t))
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{slow, other})
	reports.waitFor(t, proto.InstRunning, 2)
	waitWatched(t, e, slow.UUID)
	waitWatched(t, e, other.UUID)
	block.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	round := make(chan struct{})
	go func() { e.checkLiveness(ctx); close(round) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("HTTP probe did not start")
	}
	select {
	case <-round:
	case <-time.After(time.Second):
		t.Fatal("HTTP probe blocked the liveness round")
	}
	for range 3 {
		e.checkLiveness(ctx)
	}
	if calls.Load() != 1 {
		t.Fatalf("overlapping HTTP probes=%d", calls.Load())
	}
	driver.exit(containerName(other.UUID), 9, false)
	e.checkLiveness(ctx)
	failed := reports.waitFor(t, proto.InstFailed, 1)
	if failed[0].UUID != other.UUID {
		t.Fatalf("wrong instance failed: %+v", failed)
	}
	if !driver.exists(containerName(slow.UUID)) {
		t.Fatal("slow HTTP alone reclaimed a container")
	}
	cancel()
	waitHealthProbes(t, e)
	if len(reports.states(proto.InstFailed)) != 1 {
		t.Fatal("canceled HTTP probe was treated as failure")
	}
}

func TestRunningHealthChecksContainersFoundByInspect(t *testing.T) {
	for _, listingError := range []bool{false, true} {
		t.Run(fmt.Sprintf("listingError=%v", listingError), func(t *testing.T) {
			var fail atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" && fail.Load() {
					w.WriteHeader(503)
				}
			}))
			t.Cleanup(server.Close)
			driver := newLivenessDriver()
			e, reports := newLivenessEngine(t, driver)
			e.healthProbeInterval = time.Millisecond
			spec := runningSpec("inspect-health", serverPort(t, server))
			serve(t, e, reports, spec)
			driver.set(func(d *livenessDriver) {
				if listingError {
					d.listErrs = 1
				} else {
					d.unlisted[containerName(spec.UUID)] = true
				}
			})
			fail.Store(true)
			e.checkLiveness(context.Background())
			failed := reports.waitFor(t, proto.InstFailed, 1)
			if !strings.Contains(failed[0].Message, "HTTP 503") {
				t.Fatalf("failure=%+v", failed)
			}
		})
	}
}

func TestRunningHealthSkipsSleepAndHeadlessHTTP(t *testing.T) {
	for _, kind := range []string{"desired sleep", "actual sleep", "headless"} {
		t.Run(kind, func(t *testing.T) {
			var checking atomic.Bool
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" && checking.Load() {
					calls.Add(1)
					w.WriteHeader(503)
				}
			}))
			t.Cleanup(server.Close)
			driver := newLivenessDriver()
			e, reports := newLivenessEngine(t, driver)
			spec := runningSpec("skip-health", serverPort(t, server))
			if kind == "headless" {
				spec.Headless = true
			}
			if kind == "desired sleep" {
				spec.DesiredSleep = 1
			}
			serve(t, e, reports, spec)
			if kind == "actual sleep" {
				e.mu.Lock()
				e.setSleepLevelLocked(spec.UUID, 1)
				e.mu.Unlock()
			}
			checking.Store(true)
			for range 3 {
				e.checkLiveness(context.Background())
			}
			waitHealthProbes(t, e)
			if calls.Load() != 0 || len(reports.states(proto.InstFailed)) != 0 {
				t.Fatalf("non-serving HTTP endpoint was probed: calls=%d reports=%+v", calls.Load(), reports.snapshot())
			}
			driver.exit(containerName(spec.UUID), 9, false)
			e.checkLiveness(context.Background())
			reports.waitFor(t, proto.InstFailed, 1)
		})
	}
}

func TestRunningHealthFailedCleanupIsReportedRemoving(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && fail.Load() {
			w.WriteHeader(503)
		}
	}))
	t.Cleanup(server.Close)
	driver := newLivenessDriver()
	e, reports := newLivenessEngine(t, driver)
	e.healthProbeInterval = time.Millisecond
	spec := runningSpec("health-cleanup-failure", serverPort(t, server))
	serve(t, e, reports, spec)
	driver.set(func(d *livenessDriver) { d.removeErrs = 1 })
	fail.Store(true)
	e.checkLiveness(context.Background())
	removing := reports.waitFor(t, proto.InstRemoving, 1)
	if len(reports.states(proto.InstFailed)) != 0 || !driver.exists(containerName(spec.UUID)) || !strings.Contains(removing[0].Message, "HTTP 503; cleanup pending") {
		t.Fatalf("cleanup failure=%+v", reports.snapshot())
	}
	if f := removing[0].RuntimeFailure; f == nil || f.Code != proto.ErrorEngineUnresponsive || f.Phase != proto.PhaseServing || !strings.Contains(f.Detail, "HTTP 503") {
		t.Fatalf("removing reason = %+v", f)
	}
	waitHealthProbes(t, e)
	spec.DesiredState = "stopped"
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	reports.waitFor(t, proto.InstStopped, 1)
}

func TestRunningHealthRemovalCancelsTheProbe(t *testing.T) {
	var block atomic.Bool
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && block.Load() {
			entered <- struct{}{}
			<-r.Context().Done()
		}
	}))
	t.Cleanup(server.Close)
	driver := newLivenessDriver()
	e, reports := newLivenessEngine(t, driver)
	e.healthProbeTimeout = time.Minute
	spec := runningSpec("removed-during-health", serverPort(t, server))
	serve(t, e, reports, spec)
	block.Store(true)
	e.checkLiveness(context.Background())
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("HTTP probe did not start")
	}
	e.ApplyUpdate(context.Background(), nil, []string{spec.UUID})
	waitHealthProbes(t, e)
	reports.waitFor(t, proto.InstStopped, 1)
	if len(reports.states(proto.InstFailed)) != 0 || driver.exists(containerName(spec.UUID)) {
		t.Fatalf("removed instance was failed: %+v", reports.snapshot())
	}
}

func TestStartupHealthKeepsItsOwnRetryBudget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && calls.Add(1) <= 3 {
			w.WriteHeader(503)
		}
	}))
	t.Cleanup(server.Close)
	driver := newLivenessDriver()
	e, reports := newLivenessEngine(t, driver)
	spec := runningSpec("startup-budget", serverPort(t, server))
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(reports.states(proto.InstRunning)) == 0 && len(reports.states(proto.InstFailed)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 4 || len(reports.states(proto.InstRunning)) != 1 || len(reports.states(proto.InstFailed)) != 0 {
		t.Fatalf("startup inherited the running retry limit: calls=%d reports=%+v", calls.Load(), reports.snapshot())
	}
	waitWatched(t, e, spec.UUID)
}
