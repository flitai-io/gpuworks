// Package testenv provides the in-process L1 integration harness described in
// artifacts/archive/2026-08-21-test-pyramid/test-pyramid-p0-p2.md. It runs the real server and agent
// assembly while replacing only GPU probing and the container runtime.
package testenv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	serverapp "gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type GPUSpec struct {
	Model             string
	VRAMMB            int
	Vendor            string
	ArchID            string
	ComputeCapability string
	Healthy           *bool
}

type nodeOptions struct {
	gpus                    []GPUSpec
	gpuIndexes              []int
	driverName              string
	driver                  runtime.Driver
	managementAddress       string
	vendor                  string
	framework               string
	version                 string
	gpuDriver               string
	disableRuntimeInjection bool
	serverURL               string
	dataLaneDisabled        bool
	probeGate               <-chan struct{}
}
type NodeOption func(*nodeOptions)

func WithGPUs(count int, spec GPUSpec) NodeOption {
	return func(o *nodeOptions) {
		o.gpus = make([]GPUSpec, count)
		for i := range o.gpus {
			o.gpus[i] = spec
		}
	}
}

// WithGPUList describes heterogeneous cards without mutating the persisted GPU
// rows behind the agent's back. It is used by scheduling integration tests to
// exercise health, memory, model and capability selection exactly as reported.
func WithGPUList(specs ...GPUSpec) NodeOption {
	return func(o *nodeOptions) { o.gpus = append([]GPUSpec(nil), specs...) }
}

// WithGPUIndexes preserves non-contiguous physical device identifiers in the
// fake prober. It is intentionally separate from WithGPUs so existing tests
// continue to get the conventional 0..N-1 sequence by default.
func WithGPUIndexes(indexes ...int) NodeOption {
	return func(o *nodeOptions) { o.gpuIndexes = append([]int(nil), indexes...) }
}
func WithDriverName(name string) NodeOption { return func(o *nodeOptions) { o.driverName = name } }
func WithDriver(driver runtime.Driver) NodeOption {
	return func(o *nodeOptions) {
		o.driver = driver
		if driver != nil {
			o.driverName = driver.Name()
		}
	}
}
func WithAccelerator(vendor, framework, version string) NodeOption {
	return func(o *nodeOptions) {
		o.vendor, o.framework, o.version = vendor, framework, version
	}
}

// WithGPUDriver reports a specific GPU driver string from the fake prober.
// The default "fake" does not parse as a version, which leaves driver
// compatibility unknown; resolution tests that depend on a real verdict pass
// the driver they mean, the way a real node reports it.
func WithGPUDriver(driver string) NodeOption {
	return func(o *nodeOptions) { o.gpuDriver = driver }
}
func WithoutAcceleratorRuntimeInjection() NodeOption {
	return func(o *nodeOptions) { o.disableRuntimeInjection = true }
}

// WithServerURL points an in-process agent at an alternate control-plane URL
// instead of the harness's direct base URL. It is used to insert a ShapedLink
// (bandwidth/RTT/queue) between the agent and the server.
func WithServerURL(url string) NodeOption {
	return func(o *nodeOptions) { o.serverURL = strings.TrimSpace(url) }
}

// WithShapedLink routes an in-process agent through a ShapedLink so its
// transport carries the link's bandwidth, latency, and bounded-queue
// characteristics. Equivalent to WithServerURL(link.URL()).
func WithShapedLink(link *ShapedLink) NodeOption {
	return func(o *nodeOptions) {
		if link != nil {
			o.serverURL = link.URL()
		}
	}
}

// WithDataLaneDisabled starts the in-process agent with --data-lane=false, so it
// dials a single adaptive lane (no ?lane param) exactly like a legacy agent.
func WithDataLaneDisabled() NodeOption {
	return func(o *nodeOptions) { o.dataLaneDisabled = true }
}

// WithProbeGate holds the node's GPU probe until gate is closed, so the first
// status report (the one that carries the GPU inventory) is withheld while the
// session itself registers and the node goes online.
func WithProbeGate(gate <-chan struct{}) NodeOption {
	return func(o *nodeOptions) { o.probeGate = gate }
}

// WithManagementAddress sets the address the node's simulated agent reports
// reaching the server from: its management network address, which carries its
// copies and multi-node traffic unless a network is attached for them.
func WithManagementAddress(address string) NodeOption {
	return func(o *nodeOptions) { o.managementAddress = strings.TrimSpace(address) }
}

type Env struct {
	t            testing.TB
	ctx          context.Context
	cancel       context.CancelFunc
	serverMu     sync.Mutex
	serverCtx    context.Context
	serverCancel context.CancelFunc
	serverDone   chan error
	app          *serverapp.App
	baseURL      string
	dbPath       string
	// dataDir is the private temp directory the server was assembled with.
	// Exposing it is what lets a case drive the subsystems that work through
	// the data dir rather than through the API (the public entry point does).
	dataDir string
	nodesMu sync.Mutex
	nodes   map[string]*Node
	// portStart is the first port of the engine-port range this harness
	// holds (0 when it fell back to the controller's default range).
	portStart int
}

// harnessLogger discards output by default: a hermetic case asserts on
// behaviour, not on log text, and 1000 in-process agents would drown the test
// output. Setting GPUWORKS_TESTENV_LOG to a path turns the real server and
// agent logs on, which is how a load case is diagnosed when it reports
// something the assertions alone cannot explain.
func harnessLogger(t testing.TB) *slog.Logger {
	path := strings.TrimSpace(os.Getenv("GPUWORKS_TESTENV_LOG"))
	if path == "" {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open harness log %s: %v", path, err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return slog.New(slog.NewTextHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func New(t testing.TB) *Env {
	return newEnv(t, "", "", false)
}

// NewWithPrivateHTTPHosts explicitly trusts only the private HTTP fixtures
// named by a case; ordinary harnesses retain production outbound restrictions.
func NewWithPrivateHTTPHosts(t testing.TB, hosts ...string) *Env {
	return newEnv(t, "", "", false, hosts...)
}

// NewWithModelEndpoint is the same real app wiring as New, with an explicit
// offline HF/ModelScope endpoint used by model registration tests.
func NewWithModelEndpoint(t testing.TB, endpoint string) *Env {
	return newEnv(t, endpoint, "", false)
}

// NewOnDisk puts the control-plane database on a real file instead of in
// memory. An in-memory database never fsyncs, so it cannot show what write
// volume costs; a test that measures durable-write pressure has to use this.
// The returned Env exposes the file path through DBPath.
func NewOnDisk(t testing.TB) *Env {
	t.Helper()
	return newEnv(t, "", filepath.Join(t.TempDir(), "gpuworks.db"), false)
}

// NewWithRecovery is New with out-of-band recovery switched on, as it is in a
// default server: an offline node with an SSH credential is probed and, if its
// agent is gone, reinstalled. Other harnesses leave it off so a node a test
// stops on purpose stays down.
func NewWithRecovery(t testing.TB) *Env {
	return newEnv(t, "", "", true)
}

// DBPath is the control-plane database file, empty for an in-memory harness.
func (e *Env) DBPath() string { return e.dbPath }

// DataDir is the server's data directory: where the file-based subsystems keep
// their state (the public entry point's configuration and observation, for
// instance), as opposed to the database the API exposes.
func (e *Env) DataDir() string { return e.dataDir }

func newEnv(t testing.TB, modelEndpoint, dbPath string, recovery bool, allowedPrivateHosts ...string) *Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	// Keep the L1 control-plane database in memory by default. DataDir remains
	// a private temp directory for observability stores, image blobs, and key
	// material.
	dsn := dbPath
	if dsn == "" {
		dsn = ":memory:"
	}
	if modelEndpoint != "" {
		if endpoint, err := url.Parse(modelEndpoint); err == nil {
			allowedPrivateHosts = append(append([]string(nil), allowedPrivateHosts...), endpoint.Hostname())
		}
	}
	a, err := serverapp.New(serverapp.Config{Addr: "127.0.0.1:0", DataDir: dir, DBPath: dsn, AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026", ModelEndpoint: modelEndpoint, OutboundAllowPrivateHosts: allowedPrivateHosts,
		// CI routes SSH to cloud nodes through a SOCKS proxy (.gitlab-ci.yml).
		SSHProxy: os.Getenv("GPUWORKS_SSH_PROXY"), RecoveryEnabled: recovery}, harnessLogger(t))
	if err != nil {
		cancel()
		t.Fatalf("server app: %v", err)
	}
	portStart, portEnd := chooseTestPortRange()
	if portStart != 0 {
		a.SetControllerEnginePorts(portStart, portEnd)
		// Registered before anything that binds in the range, so the cleanups
		// (last in, first out) stop the server and every node first. A range
		// that is not handed back is lost to the machine until this test
		// process exits, and a package of a few dozen harnesses used them all.
		t.Cleanup(func() { releasePortRange(portStart) })
	}
	serverCtx, serverCancel := context.WithCancel(context.Background())
	e := &Env{t: t, ctx: ctx, cancel: cancel, serverCtx: serverCtx, serverCancel: serverCancel, app: a, nodes: map[string]*Node{}, dbPath: dbPath, dataDir: dir, portStart: portStart}
	done := make(chan error, 1)
	e.serverDone = done
	go func() { done <- a.Run(serverCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for a.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if a.Addr() == "127.0.0.1:0" {
		cancel()
		var bindErr error
		select {
		case bindErr = <-done:
		case <-time.After(time.Second):
			t.Fatalf("server did not bind within cleanup timeout")
		}
		if bindErr != nil {
			t.Fatalf("server bind failed: %v", bindErr)
		}
		t.Fatal("server did not bind")
	}
	e.baseURL = "http://" + a.Addr()
	t.Cleanup(func() {
		cancel()
		e.serverMu.Lock()
		currentCancel, currentDone := e.serverCancel, e.serverDone
		e.serverMu.Unlock()
		if currentCancel == nil || currentDone == nil {
			return
		}
		currentCancel()
		select {
		case err := <-currentDone:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(serverShutdownBudget):
			reportShutdownTimeout(t, false)
		}
	})
	return e
}

// serverShutdownBudget is how long a stopped server may take to return. Run's
// own graceful path is bounded by 5s for HTTP, 2s for proxy accounting and 2s
// for the metrics drain, and then waits for its background loops and flushes
// the request sinks to SQLite; on a loaded CI runner (two pipelines share one)
// that is well past 9s, so the budget must not be below what Run itself may
// use. Closing the request sinks also checkpoints their SQLite WAL with an
// fsync, which has taken over 15s on a runner whose disk another job was
// loading. The embedded server is not latency-sensitive and the budget only
// tells a hang from a slow drain: pay the wait instead of failing whichever
// test tears down last.
const serverShutdownBudget = 60 * time.Second

// reportShutdownTimeout fails the test for a server that did not return, with
// the goroutines still running so a hang can be told from a slow drain.
func reportShutdownTimeout(t testing.TB, fatal bool) {
	t.Helper()
	var stacks bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&stacks, 1)
	const limit = 24 << 10
	dump := stacks.String()
	if len(dump) > limit {
		dump = dump[:limit] + "\n... (truncated)"
	}
	msg := fmt.Sprintf("server shutdown timeout after %s; goroutines:\n%s", serverShutdownBudget, dump)
	if fatal {
		t.Fatal(msg)
	}
	t.Error(msg)
}

// StopServer closes only the control-plane listener; in-process agents keep
// running and will mark themselves offline before reconnecting.
func (e *Env) StopServer(t testing.TB) {
	t.Helper()
	e.serverMu.Lock()
	cancel := e.serverCancel
	done := e.serverDone
	e.serverMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server shutdown: %v", err)
		}
	case <-time.After(serverShutdownBudget):
		reportShutdownTimeout(t, true)
	}
	e.serverMu.Lock()
	e.serverCancel = nil
	e.serverDone = nil
	e.serverMu.Unlock()
}

// RestartServer reuses the assembled app and its SQLite store, binding the
// listener to its previous concrete address so existing agents reconnect.
func (e *Env) RestartServer(t testing.TB) {
	t.Helper()
	e.StopServer(t)
	addr := strings.TrimPrefix(e.baseURL, "http://")
	if err := e.app.SetAddr(addr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	e.serverMu.Lock()
	e.serverCtx, e.serverCancel, e.serverDone = ctx, cancel, done
	e.serverMu.Unlock()
	go func() { done <- e.app.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && e.app.Addr() == addr {
		// Addr is concrete as soon as net.Listen succeeds; retain the small
		// loop to avoid racing the Run goroutine.
		if resp, err := http.Get(e.baseURL + "/api/v1/healthz"); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not restart")
}

func (e *Env) Store() *store.Store { return e.app.Store() }
func (e *Env) URL() string         { return e.baseURL }

// Hub exposes the server hub so lane-aware tests can assert data-lane state
// deterministically (Hub.DataLaneState).
func (e *Env) Hub() *hub.Hub { return e.app.Hub() }

// NewShapedLink starts a shaped relay in front of the harness server and
// registers it for cleanup. Point a node at it with WithShapedLink so the
// agent<->server transport carries the configured bandwidth, RTT, and queue.
func (e *Env) NewShapedLink(t testing.TB, cfg ShapedLinkConfig) *ShapedLink {
	t.Helper()
	link, err := NewShapedLink(strings.TrimPrefix(e.baseURL, "http://"), cfg)
	if err != nil {
		t.Fatalf("start shaped link: %v", err)
	}
	t.Cleanup(func() { _ = link.Close() })
	return link
}

func (e *Env) DialServerAgent(ctx context.Context, addr, token string) error {
	return e.app.DialServerAgent(ctx, addr, token)
}

// SetControllerRetryPolicy shortens reconcile retries for deterministic
// failure-injection tests; normal harnesses retain production defaults.
func (e *Env) SetControllerRetryPolicy(backoff time.Duration, maxAttempts int) {
	e.app.SetControllerRetryPolicy(backoff, maxAttempts)
}

func (e *Env) SetControllerStartTimeout(seconds int) {
	e.app.SetControllerStartTimeout(seconds)
}

// SetControllerEnginePorts overrides the controller's host-port allocator for
// hermetic L3 tests that need a deterministic engine port.
func (e *Env) SetControllerEnginePorts(start, end int) {
	e.app.SetControllerEnginePorts(start, end)
}

// APIClient is an authenticated management-plane client. Paths beginning with
// "/" are resolved against the harness server and receive the admin bearer
// token automatically.
type APIClient struct {
	Client         *http.Client
	BaseURL, Token string
}

func (e *Env) API(t testing.TB) *APIClient {
	t.Helper()
	payload := bytes.NewBufferString(`{"username":"admin","password":"gpuworks-test-password-2026"}`)
	req, err := http.NewRequest(http.MethodPost, e.baseURL+"/api/v1/auth/login", payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("management login: status %d", resp.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Token == "" {
		t.Fatal("management login returned an empty token")
	}
	return &APIClient{Client: &http.Client{}, BaseURL: e.baseURL, Token: body.Token}
}

func (c *APIClient) Do(req *http.Request) (*http.Response, error) {
	if !req.URL.IsAbs() {
		u := *req.URL
		u.Scheme = "http"
		u.Host = strings.TrimPrefix(c.BaseURL, "http://")
		req = req.Clone(req.Context())
		req.URL = &u
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return c.Client.Do(req)
}
func (c *APIClient) Get(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}
func (c *APIClient) PostJSON(path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.Do(req)
}

func (c *APIClient) Delete(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

type Node struct {
	Name     string
	UUID     string
	Token    string
	stateDir string
	Driver   *FakeDriver
	Runtime  runtime.Driver
	Prober   *FakeProber
	Agent    *agentapp.Agent
	cancel   context.CancelFunc

	// serverURL is the control-plane URL this node dials. It differs from the
	// harness base URL when the node is routed through a ShapedLink; RestartNode
	// preserves it.
	serverURL string
	// dataLane records whether the agent opens a data lane (default true).
	// RestartNode preserves it.
	dataLane bool
	// address is the node's simulated management address.
	address string
}

// networkInventory is the simulated inventory the node's agent reports: one
// interface holding its address, which is also its management address.
func (n *Node) networkInventory() proto.NetInventory {
	inv := proto.NetInventory{Interfaces: []proto.NetInterface{}, RDMAPorts: []proto.RDMAPort{}}
	if n.address != "" {
		inv.Interfaces = append(inv.Interfaces, proto.NetInterface{Name: "fake0", State: "up", Addrs: []string{n.address + "/24"}})
		inv.Management = &proto.ManagementAddress{Interface: "fake0", IP: n.address}
	}
	return inv
}

func (n *Node) StateDir() string { return n.stateDir }

func (e *Env) AddNode(name string, options ...NodeOption) *Node {
	e.t.Helper()
	pending, token, err := e.Store().CreateNodePending(name)
	if err != nil {
		e.t.Fatalf("create pending node %q: %v", name, err)
	}
	o := nodeOptions{driverName: "fake"}
	for _, f := range options {
		f(&o)
	}
	gpus := make([]proto.GPUInfo, len(o.gpus))
	for i, s := range o.gpus {
		mb := s.VRAMMB
		if mb == 0 {
			mb = 81920
		}
		model := s.Model
		if model == "" {
			model = "A100"
		}
		vendor := strings.TrimSpace(s.Vendor)
		if vendor == "" {
			vendor = strings.TrimSpace(o.vendor)
		}
		if vendor == "" {
			vendor = "nvidia"
		}
		healthy := true
		if s.Healthy != nil {
			healthy = *s.Healthy
		}
		index := i
		if i < len(o.gpuIndexes) {
			index = o.gpuIndexes[i]
		}
		gpus[i] = proto.GPUInfo{Index: index, UUID: fmt.Sprintf("GPU-%s-%d", name, index), Name: model, MemoryTotalBytes: int64(mb) << 20, Vendor: vendor, ArchID: s.ArchID, ComputeCapability: s.ComputeCapability, Healthy: healthy}
	}
	var d runtime.Driver = o.driver
	var fake *FakeDriver
	if d == nil {
		fake = NewFakeDriver(o.driverName)
		d = fake
	}
	p := NewFakeProber(gpus)
	if strings.TrimSpace(o.vendor) != "" {
		p.VendorName = o.vendor
	}
	p.FrameworkName, p.FrameworkVersion = o.framework, o.version
	p.GPUDriverName = o.gpuDriver
	p.DisableRuntimeInjection = o.disableRuntimeInjection
	if gate := o.probeGate; gate != nil {
		p.OnProbeHook = func(ctx context.Context) ([]proto.GPUInfo, error, bool) {
			select {
			case <-gate:
				return nil, nil, false
			case <-ctx.Done():
				return nil, ctx.Err(), true
			}
		}
	}
	state := filepath.Join(e.t.TempDir(), name)
	serverURL := e.baseURL
	if o.serverURL != "" {
		serverURL = o.serverURL
	}
	ctx, cancel := context.WithCancel(e.ctx)
	dataLane := !o.dataLaneDisabled
	// A real agent always reports the address it reaches the server from;
	// hermetic nodes default to loopback, which their copies can really bind.
	address := o.managementAddress
	if address == "" {
		address = "127.0.0.1"
	}
	n := &Node{Name: name, UUID: pending.UUID, Token: token, stateDir: state, Driver: fake, Runtime: d, Prober: p, cancel: cancel, serverURL: serverURL, dataLane: dataLane, address: address}
	ag := agentapp.New(agentapp.Config{
		ServerURL: serverURL, Token: token, StateDir: state, Driver: d,
		ForceDriver: o.driverName, Prober: p, DataLane: dataLane,
		NetworkInterfaceResolver: fakeNetworkInterfaceResolver,
		NetworkInventory:         n.networkInventory,
	}, harnessLogger(e.t))
	n.Agent = ag
	go func() { _ = ag.Run(ctx) }()
	if fake != nil {
		e.t.Cleanup(func() {
			n.cancel()
			fake.ResetHooks()
			fake.CloseAll()
		})
	}
	e.t.Cleanup(p.ResetHooks)
	e.nodesMu.Lock()
	e.nodes[name] = n
	e.nodesMu.Unlock()
	return n
}

func (e *Env) Eventually(t testing.TB, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

// EventuallyNoFail polls condition and reports whether it held before the
// timeout, leaving the failure message to the caller. Eventually's own message
// cannot say what was awaited, so a caller that can describe the state it saw
// should use this and report it.
func (e *Env) EventuallyNoFail(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return condition()
}
func (e *Env) WaitNodeOnline(t testing.TB, name string) {
	t.Helper()
	e.waitFor(t, "node "+name+" online", func() (bool, string) { return e.nodeReady(name) })
}

// WaitNodeGPUs waits for a node's GPU inventory to land. A node goes online
// the moment its session registers, but the GPUs travel in the periodic
// status.node report that follows. WaitNodeOnline waits for that report too;
// this waits for a count the caller names (a node reporting fewer GPUs than it
// was added with, or GPUs that changed later). Without the report a
// spec-matched deployment can meet a node with no GPU rows and be rejected
// with "no matching spec" before the inventory arrives.
func (e *Env) WaitNodeGPUs(t testing.TB, name string, minGPUs int) {
	t.Helper()
	e.waitFor(t, fmt.Sprintf("node %s reporting %d GPU(s)", name, minGPUs), func() (bool, string) {
		row, state := e.nodeState(name)
		if row.UUID == "" {
			return false, state
		}
		withGPUs, err := e.Store().NodeWithGPUs(row.UUID)
		if err != nil {
			return false, err.Error()
		}
		return len(withGPUs.GPUs) >= minGPUs, fmt.Sprintf("%d GPU row(s)", len(withGPUs.GPUs))
	})
}

// WaitNodeRuntime waits for the node's reported runtime driver. A node goes
// online when its session registers, while the runtime name arrives with the
// status report that follows, so a test asserting on it has to wait for the
// fact rather than for the node.
func (e *Env) WaitNodeRuntime(t testing.TB, name, want string) {
	t.Helper()
	e.waitFor(t, fmt.Sprintf("node %s reporting runtime %q", name, want), func() (bool, string) {
		row, state := e.nodeState(name)
		if row.UUID == "" {
			return false, state
		}
		return row.RuntimeDriver == want, fmt.Sprintf("runtime %q", row.RuntimeDriver)
	})
}

// StopNodes disconnects every in-process agent and waits for the control plane
// to let go of their sessions.
//
// Cleanup alone is not enough for a fleet: AddNode's cleanup cancels an agent's
// context but does not wait for it, so the server's own shutdown then races a
// crowd of sessions that are still tearing down, and a harness with hundreds of
// nodes runs out of its shutdown budget. A case that stands up a fleet calls
// this when it is done with it; a case with a handful of nodes never needs to.
func (e *Env) StopNodes(t testing.TB) {
	t.Helper()
	e.nodesMu.Lock()
	nodes := make([]*Node, 0, len(e.nodes))
	for _, n := range e.nodes {
		nodes = append(nodes, n)
	}
	e.nodesMu.Unlock()
	for _, n := range nodes {
		if n.cancel != nil {
			n.cancel()
		}
	}
	// Waiting on the session table is what makes this a drain rather than a
	// signal: the hub drops a session only once its reader has ended.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if e.app.Hub().SessionCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("%d agent session(s) were still registered 30s after the fleet was stopped", e.app.Hub().SessionCount())
}

// StopNode stops one in-process agent and waits until the server has dropped
// its session, so no status report of that agent can land afterwards. A case
// that writes node facts the agent would otherwise keep re-reporting stops the
// agent first; runtime-owned containers are left untouched.
func (e *Env) StopNode(t testing.TB, name string) *Node {
	t.Helper()
	e.nodesMu.Lock()
	n := e.nodes[name]
	e.nodesMu.Unlock()
	if n == nil {
		t.Fatalf("unknown test node %q", name)
		return nil
	}
	if n.cancel != nil {
		n.cancel()
	}
	if n.UUID != "" {
		e.waitFor(t, "node "+name+" offline", func() (bool, string) {
			row, state := e.nodeState(name)
			return row.UUID != "" && row.State == store.NodeStateOffline, state
		})
	}
	return n
}

// RestartNode stops the in-process agent transport and starts a fresh Agent
// with the same persisted identity, runtime driver, and GPU prober. It models
// an agent process restart while deliberately leaving runtime-owned containers
// untouched so adoption can be asserted by an integration test.
func (e *Env) RestartNode(t testing.TB, name string) {
	t.Helper()
	// Let the old websocket observe cancellation before opening the replacement
	// connection. Starting two sessions for one identity concurrently can cause
	// stale desired-state/status messages to win under the race detector.
	n := e.StopNode(t, name)
	ctx, cancel := context.WithCancel(e.ctx)
	force := ""
	if n.Runtime != nil {
		force = n.Runtime.Name()
	}
	serverURL := n.serverURL
	if serverURL == "" {
		serverURL = e.baseURL
	}
	ag := agentapp.New(agentapp.Config{
		ServerURL: serverURL, Token: n.Token, StateDir: n.stateDir,
		Driver: n.Runtime, ForceDriver: force, Prober: n.Prober, DataLane: n.dataLane,
		NetworkInterfaceResolver: fakeNetworkInterfaceResolver,
		NetworkInventory:         n.networkInventory,
	}, harnessLogger(t))
	n.cancel = cancel
	n.Agent = ag
	go func() { _ = ag.Run(ctx) }()
}

func fakeNetworkInterfaceResolver(address string) (string, error) {
	if strings.TrimSpace(address) == "" {
		return "", fmt.Errorf("fake network address is empty")
	}
	return "fake0", nil
}

func (e *Env) WaitInstanceState(t testing.TB, uuid, want string) {
	e.Eventually(t, 2*time.Minute, func() bool {
		instances, _ := e.Store().ListInstances()
		for _, inst := range instances {
			if inst.UUID == uuid && inst.State == want {
				return true
			}
		}
		return false
	})
}

// FakeProber is a deterministic, injectable GPU backend.
type FakeProber struct {
	mu                       sync.Mutex
	GPUs                     []proto.GPUInfo
	MetricsData              []gpu.Metric
	OnProbeHook              func(context.Context) ([]proto.GPUInfo, error, bool)
	AfterProbeHook           func([]proto.GPUInfo)
	OnMetricsHook            func(context.Context) ([]gpu.Metric, error, bool)
	AfterMetricsHook         func([]gpu.Metric)
	failures                 map[string]error
	VendorName               string
	FrameworkName            string
	FrameworkVersion         string
	GPUDriverName            string
	DisableRuntimeInjection  bool
	DeviceShareCalls         []int
	DeviceShareChanged       bool
	PartitionHardwareIDValue string
}

// Synthetic marks this prober as an in-process test backend. The agent must
// not run a host CUDA smoke check for its synthetic NVIDIA devices.
func (*FakeProber) Synthetic() bool { return true }

func NewFakeProber(g []proto.GPUInfo) *FakeProber {
	vendor := "nvidia"
	if len(g) > 0 && strings.TrimSpace(g[0].Vendor) != "" {
		vendor = g[0].Vendor
	}
	return &FakeProber{GPUs: g, VendorName: vendor, failures: map[string]error{}}
}
func (p *FakeProber) SetGPUs(g []proto.GPUInfo) {
	p.mu.Lock()
	p.GPUs = append([]proto.GPUInfo(nil), g...)
	p.mu.Unlock()
}
func (p *FakeProber) SetMetrics(m []gpu.Metric) {
	p.mu.Lock()
	p.MetricsData = append([]gpu.Metric(nil), m...)
	p.mu.Unlock()
}
func (p *FakeProber) Vendor() string                     { return p.VendorName }
func (p *FakeProber) Available() bool                    { return true }
func (p *FakeProber) RuntimeSmoke(context.Context) error { return nil }
func (p *FakeProber) Framework(context.Context) (string, string) {
	return p.FrameworkName, p.FrameworkVersion
}
func (p *FakeProber) EnsureDeviceShare(_ context.Context, index int) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.DeviceShareCalls = append(p.DeviceShareCalls, index)
	return p.DeviceShareChanged, nil
}
func (p *FakeProber) PartitionHardwareID(context.Context, int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.PartitionHardwareIDValue
}
func (p *FakeProber) ShareCalls() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.DeviceShareCalls...)
}
func (p *FakeProber) OnProbe(h func(context.Context) ([]proto.GPUInfo, error, bool)) {
	p.mu.Lock()
	p.OnProbeHook = h
	p.mu.Unlock()
}
func (p *FakeProber) AfterProbe(h func([]proto.GPUInfo)) {
	p.mu.Lock()
	p.AfterProbeHook = h
	p.mu.Unlock()
}
func (p *FakeProber) OnMetrics(h func(context.Context) ([]gpu.Metric, error, bool)) {
	p.mu.Lock()
	p.OnMetricsHook = h
	p.mu.Unlock()
}
func (p *FakeProber) AfterMetrics(h func([]gpu.Metric)) {
	p.mu.Lock()
	p.AfterMetricsHook = h
	p.mu.Unlock()
}
func (p *FakeProber) FailNext(method string, err error) {
	p.mu.Lock()
	p.failures[method] = err
	p.mu.Unlock()
}

// ResetHooks releases simulator callbacks and one-shot failures at test
// cleanup. The mutex also makes it safe if a collector call is finishing.
func (p *FakeProber) ResetHooks() {
	p.mu.Lock()
	p.OnProbeHook = nil
	p.AfterProbeHook = nil
	p.OnMetricsHook = nil
	p.AfterMetricsHook = nil
	p.failures = map[string]error{}
	p.mu.Unlock()
}
func (p *FakeProber) fail(method string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.failures[method]
	delete(p.failures, method)
	return err
}
func (p *FakeProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	if err := p.fail("Probe"); err != nil {
		return nil, err
	}
	p.mu.Lock()
	h, after, g := p.OnProbeHook, p.AfterProbeHook, append([]proto.GPUInfo(nil), p.GPUs...)
	p.mu.Unlock()
	if h != nil {
		if out, err, ok := h(ctx); ok {
			if after != nil {
				after(out)
			}
			return out, err
		}
	}
	if after != nil {
		after(g)
	}
	return g, nil
}
func (p *FakeProber) DriverVersion(context.Context) string {
	if p.GPUDriverName != "" {
		return p.GPUDriverName
	}
	return "fake"
}
func (p *FakeProber) Topology(context.Context) string { return "" }
func (p *FakeProber) Metrics(ctx context.Context) ([]gpu.Metric, error) {
	return p.metrics(ctx)
}
func (p *FakeProber) metrics(ctx context.Context) ([]gpu.Metric, error) {
	if err := p.fail("Metrics"); err != nil {
		return nil, err
	}
	p.mu.Lock()
	hook, after, data := p.OnMetricsHook, p.AfterMetricsHook, append([]gpu.Metric(nil), p.MetricsData...)
	p.mu.Unlock()
	if hook != nil {
		if out, err, handled := hook(ctx); handled {
			if after != nil {
				after(out)
			}
			return out, err
		}
	}
	if after != nil {
		after(data)
	}
	return data, nil
}
func (p *FakeProber) RuntimeInjection(indexes []int) gpu.RuntimeInjection {
	if p.DisableRuntimeInjection {
		return gpu.RuntimeInjection{}
	}
	visible := make([]string, len(indexes))
	for i, index := range indexes {
		visible[i] = fmt.Sprint(index)
	}
	switch strings.ToLower(strings.TrimSpace(p.VendorName)) {
	case "ascend", "huawei":
		compact := make([]string, len(indexes))
		for i := range compact {
			compact[i] = fmt.Sprint(i)
		}
		return gpu.RuntimeInjection{Env: map[string]string{
			"ASCEND_VISIBLE_DEVICES": strings.Join(visible, ","), "ASCEND_RT_VISIBLE_DEVICES": strings.Join(compact, ","), "NPU_VISIBLE_DEVICES": strings.Join(compact, ","),
		}, DockerArgs: []string{"--runtime", "ascend"}}
	case "ppu", "t-head", "thead":
		args := []string{"--device", "/dev/alixpu:/dev/alixpu:rwm", "--device", "/dev/alixpu_ctl:/dev/alixpu_ctl:rwm"}
		compact := make([]string, len(indexes))
		for i := range compact {
			compact[i] = fmt.Sprint(i)
			args = append(args, "--device", fmt.Sprintf("/dev/alixpu_ppu%d:/dev/alixpu_ppu%d:rwm", indexes[i], indexes[i]))
		}
		return gpu.RuntimeInjection{Env: map[string]string{"NVIDIA_VISIBLE_DEVICES": "void", "CUDA_VISIBLE_DEVICES": strings.Join(compact, ",")}, DockerArgs: args}
	default:
		return gpu.RuntimeInjection{}
	}
}

// FakeDriver is implemented in fake_runtime.go.  Keeping the runtime fake in
// its own file leaves this harness entry point small enough to audit.

// ModelDir creates an empty directory standing in for a model's weights on a
// node. The agent refuses to launch an instance whose model directory does not
// exist, so a fixture model file must point at a real one.
func ModelDir(t testing.TB, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
