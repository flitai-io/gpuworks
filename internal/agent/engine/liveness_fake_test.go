package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// livenessDriver keeps containers the way docker does: a stopped or crashed
// container stays inspectable with its exit code until it is removed, Stop on
// it is harmless, `rm -f` deletes it whether it runs or not, and an absent
// container is Exists=false without an error. `docker ps` lists the running
// ones only. Listing, inspect and remove failures are injected per call, a
// listing can be made to leave containers out, and inspect or remove can be
// held open to stage a race.
type livenessDriver struct {
	*headlessTestDriver
	mu         sync.Mutex
	containers map[string]*livenessContainer
	logs       string
	listErrs   int
	unlisted   map[string]bool // running containers the listing leaves out
	statusErrs int
	removeErrs int
	runs       int
	lists      int
	inspects   map[string]int // Status calls per container name
	holdStatus chan struct{}  // when set, the next Status waits on it
	statusHeld chan struct{}
	holdRemove chan struct{} // when set, the next Remove waits on it
	removeHeld chan struct{}
}

type livenessContainer struct {
	running  bool
	exitCode int
	oom      bool
}

func newLivenessDriver() *livenessDriver {
	return &livenessDriver{headlessTestDriver: &headlessTestDriver{name: "docker"}, containers: map[string]*livenessContainer{}, unlisted: map[string]bool{}, inspects: map[string]int{}}
}

func (d *livenessDriver) Run(_ context.Context, ws runtime.WorkloadSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.containers[ws.Name]; exists {
		return "", fmt.Errorf("Conflict. The container name %q is already in use", ws.Name)
	}
	d.containers[ws.Name] = &livenessContainer{running: true}
	d.runs++
	return ws.Name, nil
}

func (d *livenessDriver) RunningContainers(context.Context) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lists++
	if d.listErrs > 0 {
		d.listErrs--
		return nil, errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	}
	var names []string
	for name, c := range d.containers {
		if c.running && !d.unlisted[name] {
			names = append(names, name)
		}
	}
	return names, nil
}

func (d *livenessDriver) Status(_ context.Context, name string) (runtime.Status, error) {
	d.mu.Lock()
	hold, held := d.holdStatus, d.statusHeld
	d.holdStatus, d.statusHeld = nil, nil
	d.inspects[name]++
	d.mu.Unlock()
	if hold != nil {
		close(held)
		<-hold
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.statusErrs > 0 {
		d.statusErrs--
		return runtime.Status{}, errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	}
	c, ok := d.containers[name]
	if !ok {
		return runtime.Status{Exists: false}, nil
	}
	return runtime.Status{Exists: true, Running: c.running, ExitCode: c.exitCode, OOMKilled: c.oom}, nil
}

func (d *livenessDriver) Stop(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[name]
	if !ok {
		return fmt.Errorf("No such container: %s", name)
	}
	if c.running {
		c.running, c.exitCode = false, 0
	}
	return nil
}

func (d *livenessDriver) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	hold, held := d.holdRemove, d.removeHeld
	d.holdRemove, d.removeHeld = nil, nil
	d.mu.Unlock()
	if hold != nil {
		close(held)
		<-hold
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.removeErrs > 0 {
		d.removeErrs--
		return fmt.Errorf("Error response from daemon: removal of container %s is already in progress", name)
	}
	delete(d.containers, name)
	return nil
}

func (d *livenessDriver) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return io.NopCloser(strings.NewReader(d.logs)), nil
}

// exit ends a container's process the way the kernel or `docker stop` would.
func (d *livenessDriver) exit(name string, code int, oom bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.containers[name] = &livenessContainer{exitCode: code, oom: oom}
}

func (d *livenessDriver) deleteContainer(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.containers, name)
}

func (d *livenessDriver) exists(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.containers[name]
	return ok
}

// resetCalls forgets the listing and inspect calls made so far, so a test
// counts only the rounds it runs after startup.
func (d *livenessDriver) resetCalls() {
	d.set(func(d *livenessDriver) { d.lists, d.inspects = 0, map[string]int{} })
}

func (d *livenessDriver) calls() (lists int, inspects map[string]int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	inspects = make(map[string]int, len(d.inspects))
	for name, n := range d.inspects {
		inspects[name] = n
	}
	return d.lists, inspects
}

func (d *livenessDriver) set(fn func(*livenessDriver)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d)
}

// holdNextStatus makes the next inspect wait; the returned channel closes once
// that inspect has started and release lets it answer.
func (d *livenessDriver) holdNextStatus() (started <-chan struct{}, release func()) {
	hold, held := make(chan struct{}), make(chan struct{})
	d.set(func(d *livenessDriver) { d.holdStatus, d.statusHeld = hold, held })
	return held, func() { close(hold) }
}

func (d *livenessDriver) holdNextRemove() (started <-chan struct{}, release func()) {
	hold, held := make(chan struct{}), make(chan struct{})
	d.set(func(d *livenessDriver) { d.holdRemove, d.removeHeld = hold, held })
	return held, func() { close(hold) }
}

type statusLog struct {
	mu  sync.Mutex
	all []proto.InstanceStatus
}

func (l *statusLog) report(st proto.InstanceStatus) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, st)
}

func (l *statusLog) states(state string) []proto.InstanceStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []proto.InstanceStatus
	for _, st := range l.all {
		if st.State == state {
			out = append(out, st)
		}
	}
	return out
}

func (l *statusLog) waitFor(t *testing.T, state string, count int) []proto.InstanceStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := l.states(state); len(got) >= count {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %d %q report(s); got %+v", count, state, l.snapshot())
	return nil
}

func (l *statusLog) snapshot() []proto.InstanceStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]proto.InstanceStatus(nil), l.all...)
}

// healthyEngine answers the startup health check and the vLLM sleep protocol.
func healthyEngine(t *testing.T) int {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/is_sleeping" {
			_, _ = w.Write([]byte("false"))
		}
	}))
	t.Cleanup(server.Close)
	return (&sleepHTTPServer{server: server}).port(t)
}

func newLivenessEngine(t *testing.T, driver runtime.Driver) (*Engine, *statusLog) {
	t.Helper()
	log := &statusLog{}
	e := New(driver, log.report, slog.Default(), t.TempDir())
	e.headlessProbeInterval = time.Millisecond
	e.headlessStableChecks = 1
	// One failed HTTP check retires; tests that exercise the failure window
	// set their own.
	e.healthFailureWindow = 0
	t.Cleanup(e.stopHealthProbes)
	return e, log
}

// serve brings spec up through the real ensure path and waits until the
// attempt has ended with the instance handed to the liveness monitor.
func serve(t *testing.T, e *Engine, log *statusLog, spec proto.InstanceDesiredState) {
	t.Helper()
	before := len(log.states(proto.InstRunning))
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	log.waitFor(t, proto.InstRunning, before+1)
	waitWatched(t, e, spec.UUID)
}

func waitWatched(t *testing.T, e *Engine, uuid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		_, watched := e.watchableLocked(uuid, e.servingEpoch[uuid])
		e.mu.Unlock()
		if watched {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s was not handed to the liveness monitor", uuid)
}

func waitIdle(t *testing.T, e *Engine, uuid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		busy := e.inFlight[uuid]
		e.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s stayed in flight", uuid)
}

func runningSpec(uuid string, port int) proto.InstanceDesiredState {
	return proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", APIPort: port, StartTimeout: 30}
}
