package testenv

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
	fakehandler "gpuworks.ai/gpuworks/test/fake-engine/handler"
)

// RunHook is the simulator replacement primitive.  A handled result avoids
// starting the default in-process HTTP engine.
type RunHook func(runtime.WorkloadSpec) (string, error, bool)

// FakeDriver is a deterministic runtime fake.  It deliberately starts a real
// HTTP listener for Run, so health checks and the OpenAI proxy still cross the
// same network boundaries as a real engine.  ImageArchiver makes it suitable
// for testing the server's yamux image relay as well.
type FakeDriver struct {
	name                 string
	mu                   sync.Mutex
	seq                  int
	servers              map[string]*http.Server
	ln                   map[string]net.Listener
	names                map[string]string // workload name -> runtime id (adoption)
	stopped              map[string]bool   // container exists but is not running
	exitCodes            map[string]int    // exit code of a container whose engine exited
	OnRunHook            RunHook
	AfterRunHook         func(runtime.WorkloadSpec)
	OnStopHook           func(string) (error, bool)
	AfterStopHook        func(string)
	OnRemoveHook         func(string) (error, bool)
	AfterRemoveHook      func(string)
	OnStatusHook         func(string) (runtime.Status, error, bool)
	AfterStatusHook      func(string, runtime.Status)
	OnPullImageHook      func(string, func(string)) (error, bool)
	AfterPullImageHook   func(string)
	OnImageExistsHook    func(string) (bool, error, bool)
	AfterImageExistsHook func(string, bool)
	OnPrepareHook        func(runtime.WorkloadSpec) (error, bool)
	AfterPrepareHook     func(runtime.WorkloadSpec)
	OnLogsHook           func(string, bool, int) (io.ReadCloser, error, bool)
	AfterLogsHook        func(string, bool, int)
	OnExecHook           func(string, []string) ([]byte, error, bool)
	AfterExecHook        func(string, []string)
	failures             map[string]error
	archiveSize          int64
	loadedImages         map[string]bool
	loadedAny            bool
	imageAbsent          bool
	engineHandler        func(EngineInstance) http.Handler
	sharedPorts          bool
	sharedPortOf         map[string]sharedClaim // runtime id -> its claim on a shared engine port
	// sideChannels stops each PD instance's KV side channel listener.
	sideChannels map[string]func()
}

func NewFakeDriver(name string) *FakeDriver {
	if name == "" {
		name = "fake"
	}
	return &FakeDriver{name: name, servers: map[string]*http.Server{}, ln: map[string]net.Listener{}, names: map[string]string{}, stopped: map[string]bool{}, exitCodes: map[string]int{}, failures: map[string]error{}, loadedImages: map[string]bool{}, sharedPortOf: map[string]sharedClaim{}, archiveSize: 8 << 20}
}

func (d *FakeDriver) Name() string                         { return d.name }
func (d *FakeDriver) Detect(context.Context) (bool, error) { return true, nil }

// SetImageAbsent is useful for the negative image-load regression: loading an
// archive succeeds, but the runtime then claims the image is not present.
func (d *FakeDriver) SetImageAbsent(v bool) { d.mu.Lock(); d.imageAbsent = v; d.mu.Unlock() }

// EngineInstance names the instance a custom engine handler serves: its
// container, and the model name its launch serves (--served-model-name), which
// the agent checks on /v1/models after the health check, as with a real engine.
type EngineInstance struct {
	Container string
	Model     string
}

// SetEngineHandler overrides the HTTP handler served by Run's in-process engine
// listener. When nil (the default) the standard fake-engine handler is used.
// The listener, container-id tracking, Status and cleanup behaviour are
// unchanged, so a test can serve large or custom responses through the real
// proxy tunnel while the deploy path stays identical to production. The handler
// must still answer the engine health path (/health) so the instance reaches
// running.
func (d *FakeDriver) SetEngineHandler(fn func(EngineInstance) http.Handler) {
	d.mu.Lock()
	d.engineHandler = fn
	d.mu.Unlock()
}

// ShareEnginePorts makes this fake attach to a process-wide listener per port
// instead of owning one. See fake_runtime_shared_port.go: it is what lets many
// in-process nodes each run an instance on the port the scheduler gives them,
// which on a real fleet would be as many different hosts. Instances sharing a
// port are not distinguishable in their engine responses, so this belongs to
// fleet-level cases only.
func (d *FakeDriver) ShareEnginePorts() {
	d.mu.Lock()
	d.sharedPorts = true
	d.mu.Unlock()
}

func (d *FakeDriver) Prepare(_ context.Context, spec runtime.WorkloadSpec) error {
	d.mu.Lock()
	hook, after := d.OnPrepareHook, d.AfterPrepareHook
	d.mu.Unlock()
	if hook != nil {
		if err, handled := hook(spec); handled {
			if after != nil {
				after(spec)
			}
			return err
		}
	}
	err := d.fail("Prepare")
	if after != nil {
		after(spec)
	}
	return err
}
func (d *FakeDriver) FailNext(method string, err error) {
	d.mu.Lock()
	d.failures[method] = err
	d.mu.Unlock()
}

// ResetHooks releases simulator callbacks and one-shot failures at test
// cleanup. This prevents closures retaining a test's state after its node is
// gone, while the mutex keeps cleanup safe with an in-flight agent call.
func (d *FakeDriver) ResetHooks() {
	d.mu.Lock()
	d.OnRunHook = nil
	d.AfterRunHook = nil
	d.OnStopHook = nil
	d.AfterStopHook = nil
	d.OnRemoveHook = nil
	d.AfterRemoveHook = nil
	d.OnStatusHook = nil
	d.AfterStatusHook = nil
	d.OnPullImageHook = nil
	d.AfterPullImageHook = nil
	d.OnImageExistsHook = nil
	d.AfterImageExistsHook = nil
	d.OnPrepareHook = nil
	d.AfterPrepareHook = nil
	d.OnLogsHook = nil
	d.AfterLogsHook = nil
	d.OnExecHook = nil
	d.AfterExecHook = nil
	d.failures = map[string]error{}
	d.mu.Unlock()
}
func (d *FakeDriver) fail(method string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.failures[method]
	delete(d.failures, method)
	return err
}

func (d *FakeDriver) Run(ctx context.Context, s runtime.WorkloadSpec) (string, error) {
	if err := d.fail("Run"); err != nil {
		return "", err
	}
	d.mu.Lock()
	h, after := d.OnRunHook, d.AfterRunHook
	d.mu.Unlock()
	if h != nil {
		if id, err, ok := h(s); ok {
			if after != nil {
				after(s)
			}
			return id, err
		}
	}
	if s.Image == assets.PartitionProbeImage {
		d.mu.Lock()
		d.seq++
		id := fmt.Sprintf("fake-probe-%d", d.seq)
		d.names[s.Name] = id
		d.stopped[id] = true
		d.mu.Unlock()
		if after != nil {
			after(s)
		}
		return id, nil
	}
	// The first port is the engine's API port, except on a headless rank,
	// which serves no API: its ports belong to side channels (a multi-node
	// PD follower's), and it listens on an ephemeral one like a follower
	// that holds none.
	headless := slices.Contains(s.Cmd, "--headless")
	port := 0
	if len(s.Ports) > 0 && !headless {
		port = s.Ports[0]
	}
	d.mu.Lock()
	shared := d.sharedPorts
	d.mu.Unlock()
	if shared && port > 0 {
		claim := sharedClaim{port: port, model: fakehandler.ServedModel(append(append([]string(nil), s.Entrypoint...), s.Cmd...), s.Name)}
		if _, err := acquireSharedEnginePort(claim); err != nil {
			return "", err
		}
		d.mu.Lock()
		d.seq++
		id := fmt.Sprintf("fake-%d", d.seq)
		d.names[s.Name] = id
		d.stopped[id] = false
		d.sharedPortOf[id] = claim
		d.mu.Unlock()
		if after != nil {
			after(s)
		}
		return id, nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	d.seq++
	id := fmt.Sprintf("fake-%d", d.seq)
	d.ln[id] = ln
	d.names[s.Name] = id
	d.stopped[id] = false
	d.mu.Unlock()
	d.mu.Lock()
	engineHandler := d.engineHandler
	d.mu.Unlock()
	// A PD instance plays its connector role, read from the launch the way the
	// engine reads it, and serves the KV side channel its peers pull from.
	// The fake engine is one process per instance: KV moves between leaders,
	// so a headless rank serves no side channel of its own.
	pd := fakehandler.PDFromLaunch(append(append([]string(nil), s.Entrypoint...), s.Cmd...), s.Env)
	if pd != nil {
		pd.Dial = dialSameHost
	}
	// Like the real engine it stands in for, it answers under the launch's
	// --served-model-name; the agent checks that name after the health check.
	launch := append(append([]string(nil), s.Entrypoint...), s.Cmd...)
	engine := fakehandler.New(fakehandler.Options{Model: fakehandler.ServedModel(launch, s.Name), PD: pd})
	var handler http.Handler = engine
	if engineHandler != nil {
		handler = engineHandler(EngineInstance{Container: s.Name, Model: fakehandler.ServedModel(launch, s.Name)})
	} else if headless {
	} else if stopSide, sideErr := engine.ServeSideChannel(); sideErr != nil {
		_ = ln.Close()
		return "", fmt.Errorf("KV side channel: %w", sideErr)
	} else {
		d.mu.Lock()
		if d.sideChannels == nil {
			d.sideChannels = map[string]func(){}
		}
		d.sideChannels[id] = stopSide
		d.mu.Unlock()
	}
	srv := &http.Server{Handler: handler}
	d.mu.Lock()
	d.servers[id] = srv
	d.mu.Unlock()
	// Stop owns server shutdown; Serve terminates when its listener closes.
	go func() { _ = srv.Serve(ln) }()
	if after != nil {
		after(s)
	}
	return id, nil
}

func (d *FakeDriver) Stop(_ context.Context, id string) error {
	if err := d.fail("Stop"); err != nil {
		return err
	}
	d.mu.Lock()
	hook, after := d.OnStopHook, d.AfterStopHook
	d.mu.Unlock()
	if hook != nil {
		if err, handled := hook(id); handled {
			if after != nil {
				after(id)
			}
			return err
		}
	}
	d.halt(id)
	if after != nil {
		after(id)
	}
	return nil
}

// Exit ends a running container's engine with code, the way a crash, an OOM
// kill (137) or an engine returning 0 does without anyone calling Stop. Like a
// docker container, it stays inspectable with that code until it is removed.
func (d *FakeDriver) Exit(id string, code int) {
	resolved := d.halt(id)
	d.mu.Lock()
	if _, exists := d.stopped[resolved]; exists {
		d.exitCodes[resolved] = code
	}
	d.mu.Unlock()
}

// halt stops a container's engine and returns the runtime id it resolved to.
func (d *FakeDriver) halt(id string) string {
	d.mu.Lock()
	s := d.servers[id]
	resolved := id
	if s == nil {
		if mapped := d.names[id]; mapped != "" {
			resolved = mapped
			s = d.servers[resolved]
		}
	}
	wasStopped := d.stopped[resolved]
	claim, isShared := d.sharedPortOf[resolved]
	d.mu.Unlock()
	if isShared && !wasStopped {
		// The listener only closes when the last instance on that port lets go,
		// exactly as the port would free up on a real host.
		releaseSharedEnginePort(claim)
	}
	if s != nil && !wasStopped {
		_ = s.Close()
		d.stopSideChannel(resolved)
	}
	d.mu.Lock()
	if s != nil || isShared || d.stopped[resolved] {
		d.stopped[resolved] = true
	}
	d.mu.Unlock()
	return resolved
}
func (d *FakeDriver) Remove(_ context.Context, id string) error {
	if err := d.fail("Remove"); err != nil {
		return err
	}
	d.mu.Lock()
	hook, after := d.OnRemoveHook, d.AfterRemoveHook
	d.mu.Unlock()
	if hook != nil {
		if err, handled := hook(id); handled {
			if after != nil {
				after(id)
			}
			return err
		}
	}
	d.mu.Lock()
	s := d.servers[id]
	resolved := id
	if s == nil {
		if _, tracked := d.sharedPortOf[resolved]; !tracked {
			resolved = d.names[id]
		}
		s = d.servers[resolved]
	}
	delete(d.servers, resolved)
	ln := d.ln[resolved]
	delete(d.ln, resolved)
	claim, isShared := d.sharedPortOf[resolved]
	stillHeld := isShared && !d.stopped[resolved]
	delete(d.sharedPortOf, resolved)
	for name, mapped := range d.names {
		if mapped == resolved || name == id {
			delete(d.names, name)
		}
	}
	delete(d.stopped, resolved)
	delete(d.exitCodes, resolved)
	d.mu.Unlock()
	if stillHeld {
		releaseSharedEnginePort(claim)
	}
	if s != nil {
		_ = s.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
	d.stopSideChannel(resolved)
	if after != nil {
		after(id)
	}
	return nil
}

func (d *FakeDriver) stopSideChannel(id string) {
	d.mu.Lock()
	stop := d.sideChannels[id]
	delete(d.sideChannels, id)
	d.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// CloseAll releases listeners owned by this fake at test cleanup.  A cancelled
// agent intentionally leaves desired containers for adoption; the harness must
// still reclaim their host sockets so repeated tests cannot collide on ports.
func (d *FakeDriver) CloseAll() {
	d.mu.Lock()
	servers := make([]*http.Server, 0, len(d.servers))
	for id, srv := range d.servers {
		servers = append(servers, srv)
		delete(d.servers, id)
	}
	listeners := make([]net.Listener, 0, len(d.ln))
	for id, ln := range d.ln {
		listeners = append(listeners, ln)
		delete(d.ln, id)
	}
	held := make([]sharedClaim, 0, len(d.sharedPortOf))
	for id, claim := range d.sharedPortOf {
		if !d.stopped[id] {
			held = append(held, claim)
		}
		delete(d.sharedPortOf, id)
	}
	d.stopped = map[string]bool{}
	d.exitCodes = map[string]int{}
	sides := d.sideChannels
	d.sideChannels = nil
	d.mu.Unlock()
	for _, stop := range sides {
		stop()
	}
	for _, claim := range held {
		releaseSharedEnginePort(claim)
	}
	for _, srv := range servers {
		_ = srv.Close()
	}
	for _, ln := range listeners {
		_ = ln.Close()
	}
}
func (d *FakeDriver) Status(_ context.Context, id string) (runtime.Status, error) {
	d.mu.Lock()
	hook, after := d.OnStatusHook, d.AfterStatusHook
	d.mu.Unlock()
	if hook != nil {
		if status, err, handled := hook(id); handled {
			if after != nil {
				after(id, status)
			}
			return status, err
		}
	}
	d.mu.Lock()
	resolved := id
	if _, serverExists := d.servers[resolved]; !serverExists {
		if _, stoppedExists := d.stopped[resolved]; !stoppedExists {
			resolved = d.names[id]
		}
	}
	_, exists := d.servers[resolved]
	if !exists {
		_, exists = d.stopped[resolved]
	}
	running := exists && !d.stopped[resolved]
	exitCode := d.exitCodes[resolved]
	d.mu.Unlock()
	if err := d.fail("Status"); err != nil {
		return runtime.Status{}, err
	}
	status := runtime.Status{Exists: exists, Running: running}
	if exists && !running {
		status.ExitCode = exitCode
	}
	if running {
		status.Health = "healthy"
	}
	if after != nil {
		after(id, status)
	}
	return status, nil
}

// ListContainers reports the workload names this fake knows about, which is
// what the orphan sweeper compares against the desired set. Names are tracked
// from Run and dropped by Remove, so a container that only had Stop called on
// it is still listed, exactly like a stopped docker container.
func (d *FakeDriver) ListContainers(_ context.Context) ([]string, error) {
	if err := d.fail("ListContainers"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	names := make([]string, 0, len(d.names))
	for name := range d.names {
		names = append(names, name)
	}
	d.mu.Unlock()
	sort.Strings(names)
	return names, nil
}

// RunningContainers reports the names whose container is up, the listing
// `docker ps` gives without -a. It answers from the same state Status reads,
// status hook included, so the two can never disagree about a container.
func (d *FakeDriver) RunningContainers(_ context.Context) ([]string, error) {
	if err := d.fail("RunningContainers"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	hook := d.OnStatusHook
	up := make(map[string]bool, len(d.names))
	for name, id := range d.names {
		_, exists := d.servers[id]
		if !exists {
			_, exists = d.stopped[id]
		}
		up[name] = exists && !d.stopped[id]
	}
	d.mu.Unlock()
	var names []string
	for name, running := range up {
		if hook != nil {
			if status, err, handled := hook(name); handled {
				running = err == nil && status.Exists && status.Running
			}
		}
		if running {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (d *FakeDriver) Logs(_ context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	d.mu.Lock()
	hook, after := d.OnLogsHook, d.AfterLogsHook
	d.mu.Unlock()
	if hook != nil {
		if reader, err, handled := hook(id, follow, tail); handled {
			if after != nil {
				after(id, follow, tail)
			}
			return reader, err
		}
	}
	if err := d.fail("Logs"); err != nil {
		return nil, err
	}
	reader := io.NopCloser(strings.NewReader("INFO Application startup complete\nERROR torch.cuda.OutOfMemoryError: CUDA out of memory\n"))
	if after != nil {
		after(id, follow, tail)
	}
	return reader, nil
}
func (d *FakeDriver) Exec(_ context.Context, id string, cmd []string) ([]byte, error) {
	d.mu.Lock()
	hook, after := d.OnExecHook, d.AfterExecHook
	d.mu.Unlock()
	if hook != nil {
		if out, err, handled := hook(id, cmd); handled {
			if after != nil {
				after(id, cmd)
			}
			return out, err
		}
	}
	err := d.fail("Exec")
	if after != nil {
		after(id, cmd)
	}
	return nil, err
}

var _ runtime.Driver = (*FakeDriver)(nil)
var _ runtime.ImageArchiver = (*FakeDriver)(nil)

// dialSameHost reaches a peer engine of the fake cluster: every fake node
// runs on this host, whatever address the node advertises, so a peer's side
// channel is dialled on loopback at its port.
func dialSameHost(ctx context.Context, network, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
}
