// Package app is the agent assembly root: reconnect lifecycle and the doctor
// self-check. Connection identity is supplied by the service command line;
// UUIDs issued by the server are held only in memory.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/backupexclude"
	"gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/dockerproxy"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/hostsim"
	"gpuworks.ai/gpuworks/internal/agent/registryprobe"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/selflog"
)

// Config for the agent process.
type Config struct {
	ServerURL   string         // https://server
	Token       string         // per-node token (required for normal runs)
	StateDir    string         // local state/log/model directory
	ModelsDir   string         // absolute root for downloaded models (default StateDir/models)
	Driver      runtime.Driver // nil = auto-detect (tests inject a mock)
	ForceDriver string         // "docker" | "udocker" | "nerdctl" — override auto-detection
	Prober      gpu.Prober     // nil = nvidia-smi prober (tests inject a mock)
	// AcceleratorHost is the host's cards by vendor, which the prober is
	// chosen from when Prober is nil; nil means the real host. A process
	// test sets a fake one (GPUWORKS_FAKE_GPU_VENDORS).
	AcceleratorHost AcceleratorHost
	ConnectMode     string // agent_dial (default) | server_dial (reserved)
	ListenAddr      string // server_dial listen address (default agentconf.DefaultListen)
	// DataLane enables the dedicated high-throughput data lane on agent_dial
	// connections (default true via the CLI flag). server_dial and smoke never
	// open one.
	DataLane bool
	// DCGM controls the optional per-tick "dcgmi dmon" probe (plan §4.1). It
	// is opt-in and defaults to "off" (also what "" means): only the explicit
	// value "auto" enables it, and only when dcgmi is also found on PATH.
	DCGM string
	// NetworkInterfaceResolver maps an advertised node IP to its host-network
	// interface. Nil uses the real host interfaces; hermetic agents inject their
	// simulated interface so distributed runtime environment assembly stays real.
	NetworkInterfaceResolver func(string) (string, error)
	// NetworkInventory replaces the host network inventory the agent reports.
	// Nil reports the real host; hermetic agents simulate their own addresses.
	NetworkInventory func() proto.NetInventory
	// SimulatedHost makes the agent stand in for another machine (the
	// experience environment's simulated node); nil is a real node.
	SimulatedHost *hostsim.Host
	// SelfLog captures the agent's own warn+ slog records so they ship to the
	// log center under source=agent. Nil disables that follower.
	SelfLog *selflog.Collector
	// DebugLog opens the time- and size-bounded debug sessions the server
	// asks for (debug.start); nil when the process logs without one.
	DebugLog *debuglog.Tee
	// RegistryProbe ranks an image's registries for every pull. Nil probes
	// the real registries, or none when Driver is injected (a test harness).
	RegistryProbe *registryprobe.Prober
	// EnginePorts is this node's own engine port range (--engine-ports); nil
	// uses the server's.
	EnginePorts *proto.PortRange
	// KubeIdleReturn is how long a card borrowed from Kubernetes outlives its
	// last instance on a kube node (the cluster's setting, from enrollment);
	// negative keeps borrowed cards.
	KubeIdleReturn time.Duration
}

// Agent owns the reconnect lifecycle.
type Agent struct {
	cfg Config
	log *slog.Logger
}

// New builds an Agent.
func New(cfg Config, log *slog.Logger) *Agent {
	return &Agent{cfg: cfg, log: log}
}

// Run connects and keeps the session alive, reconnecting with backoff until
// ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	serverURL := a.cfg.ServerURL
	nodeToken := a.cfg.Token
	if strings.TrimSpace(nodeToken) == "" {
		return fmt.Errorf("node token is required")
	}
	// A server_dial agent never contacts the server, so it has no address.
	if serverURL == "" && connectMode(a.cfg.ConnectMode) != "server_dial" {
		return fmt.Errorf("server URL is required unless the server dials the agent")
	}
	// A binary installed by an in-place upgrade stays on probation until the
	// first registered handshake; see conn.StartUpgradeGuard.
	conn.StartUpgradeGuard(a.log)

	// The node runtime (engine + downloads) persists across reconnects; only the
	// transport is rebuilt each attempt.
	modelsDir := a.cfg.ModelsDir
	if modelsDir == "" {
		modelsDir = filepath.Join(a.cfg.StateDir, "models")
	}
	// Model paths become docker bind-mount sources, which must be absolute.
	if abs, err := filepath.Abs(modelsDir); err == nil {
		modelsDir = abs
	}
	stateDir := filepath.Dir(modelsDir)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create agent state directory %s: %w", stateDir, err)
	}
	// On a Mac, keep models and environment packages out of Time Machine;
	// tmutil can take seconds, which registration does not wait for.
	go backupexclude.Exclude(a.log, stateDir, modelsDir)
	// Inventory reporting and background metrics must share one prober. Besides
	// keeping vendor selection consistent, this lets backends such as Ascend
	// reuse the same npu-smi snapshot when the periodic inventory and metrics
	// loops run close together.
	prober := a.cfg.Prober
	acceleratorPreference := readAcceleratorPreference(stateDir)
	var host AcceleratorHost
	if prober == nil {
		host = a.cfg.AcceleratorHost
		if host == nil {
			host = realAcceleratorHost{census: &gpu.VendorCensus{}}
		}
		prober = host.ProberFor(acceleratorPreference)
		if acceleratorPreference != proto.AcceleratorPreferenceAuto {
			a.log.Info("managing the chosen accelerator", "vendor", prober.Vendor(), "preference", acceleratorPreference)
		}
	}
	var engineOptions []engine.EngineOption
	if a.cfg.NetworkInterfaceResolver != nil {
		engineOptions = append(engineOptions, engine.WithNetworkInterfaceResolver(a.cfg.NetworkInterfaceResolver))
	}
	if h := a.cfg.SimulatedHost; h != nil {
		h.Dir = modelsDir
	}
	if extract := a.cfg.SimulatedHost.LibvgpuExtractor(); extract != nil {
		engineOptions = append(engineOptions, engine.WithLibvgpuExtractor(extract))
	}
	node := newNodeRuntime(a.log, a.cfg.Driver, modelsDir, a.cfg.ForceDriver, prober, a.cfg.RegistryProbe, engineOptions...)
	node.host = a.cfg.SimulatedHost
	node.selfLog = a.cfg.SelfLog
	node.debugLog = a.cfg.DebugLog
	node.logDir = a.cfg.StateDir
	if kd, ok := node.runtimeDriver().(*kube.Driver); ok {
		kd.SetIdleReturn(a.cfg.KubeIdleReturn)
		go kd.Start(ctx)
	}
	node.accelerator.preference = acceleratorPreference
	node.accelerator.census = host
	node.serverURL = serverURL
	if a.cfg.Driver == nil && goruntime.GOOS == "linux" {
		// An injected driver means a test; the real host's Docker daemon is
		// none of its business.
		node.runtimeProxy = dockerproxy.New(a.log)
	}
	node.SetNodeToken(nodeToken)
	defer node.servingForward.close()
	node.dcgmMode = a.cfg.DCGM

	// Start metrics/log background loops once the node identity is known.
	var bgOnce sync.Once
	startBG := func(uuid string) {
		if uuid != "" {
			bgOnce.Do(func() { node.StartBackground(ctx, uuid) })
		}
	}
	var nodeUUID string

	if connectMode(a.cfg.ConnectMode) == "server_dial" {
		return a.runServerDial(ctx, serverURL, nodeToken, &nodeUUID, node, prober, startBG)
	}

	backoff := newBackoff(1*time.Second, 30*time.Second)
	connectionLog := connectionDiagnosticLogger(a.log)
	for {
		if ctx.Err() != nil {
			return nil
		}
		creds := conn.Credentials{
			ServerURL:   serverURL,
			NodeUUID:    nodeUUID,
			NodeToken:   nodeToken,
			ConnectMode: connectMode(a.cfg.ConnectMode),
			DataLane:    a.cfg.DataLane,
			Simulated:   a.cfg.SimulatedHost,
			EnginePorts: a.cfg.EnginePorts,
		}
		onReg := func(uuid, issuedToken string) error {
			nodeUUID = uuid
			if strings.TrimSpace(issuedToken) != "" {
				nodeToken = issuedToken
				node.SetNodeToken(issuedToken)
			}
			startBG(uuid)
			return nil
		}

		c := conn.New(creds, onReg, a.handlers(node), prober, connectionLog)
		start := time.Now()
		err := c.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			var reject *conn.RejectError
			if errors.As(err, &reject) {
				logConnectionFailure(a.log, c, true)
				d := 5 * time.Minute
				c.RecordReconnect(d)
				a.log.Info("reconnecting", "in", d.String())
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(d):
				}
				continue
			}
			logConnectionFailure(a.log, c, false)
		} else {
			a.log.Info("connection closed")
		}
		// A session that lasted a while resets backoff — transient drops recover
		// fast, but a server that rejects us immediately backs off.
		if time.Since(start) > 60*time.Second {
			backoff.reset()
		}
		d := backoff.next()
		c.RecordReconnect(d)
		a.log.Info("reconnecting", "in", d.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d):
		}
	}
}

func (a *Agent) runServerDial(ctx context.Context, serverURL, nodeToken string, nodeUUID *string, node *NodeRuntime, prober gpu.Prober, startBG func(string)) error {
	listenAddr := a.cfg.ListenAddr
	if listenAddr == "" {
		listenAddr = agentconf.DefaultListen
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}
	defer ln.Close()
	a.log.Info("agent listening for server_dial", "addr", listenAddr)
	connectionLog := connectionDiagnosticLogger(a.log)
	var identityMu sync.Mutex
	factory := func() *conn.Conn {
		identityMu.Lock()
		creds := conn.Credentials{
			ServerURL:   serverURL,
			NodeUUID:    *nodeUUID,
			NodeToken:   nodeToken,
			ConnectMode: "server_dial",
			Simulated:   a.cfg.SimulatedHost,
			EnginePorts: a.cfg.EnginePorts,
			DataLane:    a.cfg.DataLane,
		}
		identityMu.Unlock()
		onReg := func(uuid, issuedToken string) error {
			identityMu.Lock()
			*nodeUUID = uuid
			if strings.TrimSpace(issuedToken) != "" {
				nodeToken = issuedToken
				node.SetNodeToken(issuedToken)
			}
			identityMu.Unlock()
			startBG(uuid)
			return nil
		}
		return conn.New(creds, onReg, a.handlers(node), prober, connectionLog)
	}
	return conn.ServeServerDial(ctx, ln, factory, func(c *conn.Conn, _ error) {
		if ctx.Err() == nil {
			logConnectionFailure(a.log, c, false)
		}
	})
}

func connectMode(mode string) string {
	if mode == "server_dial" {
		return "server_dial"
	}
	return "agent_dial"
}

// backoff is a simple jittered exponential backoff.
type backoff struct {
	base, max, cur time.Duration
}

func newBackoff(base, max time.Duration) *backoff {
	return &backoff{base: base, max: max, cur: base}
}

func (b *backoff) next() time.Duration {
	d := b.cur
	b.cur *= 2
	if b.cur > b.max {
		b.cur = b.max
	}
	return d
}

func (b *backoff) reset() { b.cur = b.base }

// simulatedNetwork reports a configured network inventory instead of the
// host's; every other handler is the node runtime's.
type simulatedNetwork struct {
	*NodeRuntime
	inventory func() proto.NetInventory
}

func (s simulatedNetwork) NetworkInventory() proto.NetInventory { return s.inventory() }

// handlers is what the connection dispatches to.
func (a *Agent) handlers(node *NodeRuntime) conn.Handlers {
	if a.cfg.NetworkInventory != nil {
		return simulatedNetwork{NodeRuntime: node, inventory: a.cfg.NetworkInventory}
	}
	return node
}
