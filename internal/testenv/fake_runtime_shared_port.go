package testenv

import (
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"syscall"

	fakehandler "gpuworks.ai/gpuworks/test/fake-engine/handler"
)

// Every in-process node in this harness runs on one host, but the product
// allocates engine ports per node: two nodes are two machines, so both are
// entitled to the same port number. On one host the second bind fails, which
// caps the harness at one running instance across the whole fleet - not a
// product limit, an artefact of standing N hosts up inside one process.
//
// A shared port lets the fakes model that: the first instance to claim a port
// opens the listener, later ones attach to it, and it closes when the last one
// lets go. Each instance still gets a real socket, a real health endpoint and a
// real proxy path; what it does not get is an engine response distinguishable
// from its port-mates, so this is only for cases that assert fleet behaviour
// rather than per-instance engine output. It is opt-in for exactly that reason.
type sharedEnginePort struct {
	server   *http.Server
	listener net.Listener
	refs     int
	// models counts the instances on the port per served model: the shared
	// engine lists them all on /v1/models, since the agent checks after the
	// health check that its own model is served there, as on a real host
	// where the port would be its engine's alone.
	models map[string]int
}

// sharedClaim is one instance's hold on a shared port, under its model.
type sharedClaim struct {
	port  int
	model string
}

var (
	sharedEnginePortsMu sync.Mutex
	sharedEnginePorts   = map[int]*sharedEnginePort{}
	// sharedEngineOptions shapes what every shared-port engine serves (for a
	// gateway load case: first-token delay, per-token delay, deltas per
	// stream). Model is always the instance's own name.
	sharedEngineOptions fakehandler.Options
)

// SetSharedEngineOptions sets the response shape of shared-port fake engines
// opened after the call. It is process-wide because the shared listeners are.
func SetSharedEngineOptions(opts fakehandler.Options) {
	sharedEnginePortsMu.Lock()
	sharedEngineOptions = opts
	sharedEnginePortsMu.Unlock()
}

func acquireSharedEnginePort(claim sharedClaim) (net.Listener, error) {
	sharedEnginePortsMu.Lock()
	defer sharedEnginePortsMu.Unlock()
	if entry := sharedEnginePorts[claim.port]; entry != nil {
		entry.refs++
		entry.models[claim.model]++
		return entry.listener, nil
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(claim.port)))
	if err != nil {
		return nil, err
	}
	entry := &sharedEnginePort{listener: listener, refs: 1, models: map[string]int{claim.model: 1}}
	opts := sharedEngineOptions
	opts.Model = claim.model
	opts.Models = entry.servedModels
	// One handler answers for every instance on this port, so its live queue
	// is all of theirs together; reporting it as each instance's queue would
	// feed the router a load signal no real engine produces.
	opts.StaticGauges = true
	entry.server = &http.Server{Handler: fakehandler.New(opts)}
	sharedEnginePorts[claim.port] = entry
	// Releasing the last port claim owns server/listener shutdown.
	go func() { _ = entry.server.Serve(listener) }()
	return listener, nil
}

// servedModels lists the models of the instances on the port.
func (e *sharedEnginePort) servedModels() []string {
	sharedEnginePortsMu.Lock()
	defer sharedEnginePortsMu.Unlock()
	out := make([]string, 0, len(e.models))
	for model := range e.models {
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}

func releaseSharedEnginePort(claim sharedClaim) {
	sharedEnginePortsMu.Lock()
	entry := sharedEnginePorts[claim.port]
	if entry == nil {
		sharedEnginePortsMu.Unlock()
		return
	}
	if entry.models[claim.model]--; entry.models[claim.model] <= 0 {
		delete(entry.models, claim.model)
	}
	entry.refs--
	if entry.refs > 0 {
		sharedEnginePortsMu.Unlock()
		return
	}
	delete(sharedEnginePorts, claim.port)
	sharedEnginePortsMu.Unlock()
	_ = entry.server.Close()
}

// hostPortCheckMu serialises the real binds of BindHostPort across every fake
// node in the process. Two nodes are two machines, so both may be given the
// same port and check it at the same moment; on one host each check's probe
// socket would then refuse the other's bind, failing a launch with
// ENGINE_PORT_REFUSED on a port neither machine uses.
var hostPortCheckMu sync.Mutex

// BindHostPort is the engine's pre-create port check (runtime.HostPortBinder).
// Without shared ports it binds for real, as the engine would, and closes the
// probe before another node's check can see it. With them the fakes stand
// separate machines up on one host, so another node's instance on the same
// port number is not this node's conflict: only an instance of this driver's
// own node still holding the port is. FailNext("BindHostPort", err) makes the
// next check fail with err, as a port another process took would.
func (d *FakeDriver) BindHostPort(port int) (io.Closer, error) {
	if err := d.fail("BindHostPort"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	shared := d.sharedPorts
	held := false
	for id, claim := range d.sharedPortOf {
		if claim.port == port && !d.stopped[id] {
			held = true
			break
		}
	}
	d.mu.Unlock()
	if !shared {
		hostPortCheckMu.Lock()
		defer hostPortCheckMu.Unlock()
		ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		_ = ln.Close()
		return io.NopCloser(nil), nil
	}
	if held {
		return nil, &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	}
	return io.NopCloser(nil), nil
}
