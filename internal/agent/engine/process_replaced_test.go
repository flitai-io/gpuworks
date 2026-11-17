package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// processEngine is a vLLM sleep endpoint that remembers whether its process
// sleeps. restart is a new process under the same port: it starts awake.
type processEngine struct {
	mu     sync.Mutex
	asleep bool
}

func (p *processEngine) restart() { p.mu.Lock(); p.asleep = false; p.mu.Unlock() }

func (p *processEngine) sleeping() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.asleep }

func (p *processEngine) serve(t *testing.T) int {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		switch {
		case r.URL.Path == "/is_sleeping":
			if p.asleep {
				_, _ = w.Write([]byte("true"))
			} else {
				_, _ = w.Write([]byte("false"))
			}
		case r.URL.Path == "/sleep":
			p.asleep = true
		case strings.HasPrefix(r.URL.Path, "/wake_up"):
			p.asleep = false
		}
	}))
	t.Cleanup(server.Close)
	return (&sleepHTTPServer{server: server}).port(t)
}

// An instance desired asleep whose engine process is replaced - the container
// died and the next sync started a new one before the liveness monitor saw it,
// or the runtime was switched - gets a new process that starts awake. The
// agent must put that process to sleep, not take the old process's sleep for
// it: otherwise the server is told the instance sleeps (and holds its L1 host
// buffer) while the new engine keeps every card's memory.
func TestAReplacedEngineProcessIsPutToSleepAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace func(t *testing.T, e *Engine, driver *livenessDriver, proc *processEngine, spec proto.InstanceDesiredState) *livenessDriver
	}{
		{"died and restarted by the next sync", func(t *testing.T, e *Engine, driver *livenessDriver, proc *processEngine, spec proto.InstanceDesiredState) *livenessDriver {
			driver.exit(containerName(spec.UUID), 137, false)
			proc.restart()
			e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
			return driver
		}},
		{"moved to another runtime", func(t *testing.T, e *Engine, _ *livenessDriver, proc *processEngine, _ proto.InstanceDesiredState) *livenessDriver {
			next := newLivenessDriver()
			next.name = "udocker"
			proc.restart()
			if err := e.SwitchDriver(context.Background(), next); err != nil {
				t.Fatal(err)
			}
			return next
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := newLivenessDriver()
			e, log := newLivenessEngine(t, driver)
			proc := &processEngine{}
			spec := runningSpec("replaced-process", proc.serve(t))
			spec.Sleepable = true
			serve(t, e, log, spec)
			waitIdle(t, e, spec.UUID)
			spec.DesiredSleep = 1
			e.ApplyUpdate(context.Background(), []proto.InstanceDesiredState{spec}, nil)
			deadline := time.Now().Add(5 * time.Second)
			for !(proc.sleeping() && e.holdsL1HostBuffer(spec.UUID)) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			waitIdle(t, e, spec.UUID)
			if !proc.sleeping() {
				t.Fatal("fixture: the first process did not sleep")
			}

			now := tc.replace(t, e, driver, proc, spec)
			deadline = time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				now.mu.Lock()
				_, up := now.containers[containerName(spec.UUID)]
				running := up && now.containers[containerName(spec.UUID)].running
				now.mu.Unlock()
				e.mu.Lock()
				busy := e.inFlight[spec.UUID]
				e.mu.Unlock()
				if running && !busy && proc.sleeping() {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			waitIdle(t, e, spec.UUID)
			if !proc.sleeping() {
				t.Error("the new engine process was never put to sleep: the agent took the old process's sleep for it, so the server is told it sleeps while it keeps its cards")
			}
		})
	}
}
