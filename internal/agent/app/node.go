package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/collector"
	"gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/dockerproxy"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/agent/hostsim"
	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/perftest"
	partitionprobe "gpuworks.ai/gpuworks/internal/agent/probe"
	"gpuworks.ai/gpuworks/internal/agent/registryprobe"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/selflog"
)

// NodeRuntime is the persistent agent-side controller: it owns the container
// engine and model downloads, and implements conn.Handlers. One instance lives
// for the whole agent process; Bind/Unbind track the current connection so
// status reports route to the live link (and are dropped when offline — the
// server re-syncs and the agent re-reports on the next connect).
type NodeRuntime struct {
	// kubeCordon is the last cordon state a kube node read from its API
	// server, restated when a round cannot ask.
	kubeCordon struct {
		mu   sync.Mutex
		last bool
	}
	log            *slog.Logger
	switchMu       sync.Mutex
	engine         *engine.Engine
	bench          *benchEngine
	prober         gpu.Prober
	partitionProbe *partitionprobe.Runner
	// cardSmoke asks each card whether the runtime can use it, on its own
	// schedule (see card_smoke.go). Nil where there is no check to run.
	cardSmoke *cardsmoke.Runner
	// doctorRunning lets one diagnosis run at a time. A second one would
	// only queue more checks on a driver the first may be stuck in.
	doctorRunning atomic.Bool
	// pcie follows each card's PCIe links and error counters (card_pcie.go).
	pcie pcieWatch
	// nvlink follows each card's active NVLinks (card_nvlink.go).
	nvlink nvlinkWatch
	// xid follows the kernel log for NVIDIA Xid events; nil off NVIDIA.
	xid atomic.Pointer[gpu.XidWatcher]
	// checks is each card's check states and last runs (card_checks.go).
	checks cardCheckMemory
	// cardSmokeCtx is the runner's own lifetime, so a check asked for
	// outside the schedule ends when the agent does.
	cardSmokeCtx context.Context
	// instanceCardIdents is a seam beside heldCards: the cards one
	// instance was given, whatever state it reached.
	instanceCardIdents func(uuid string) []string
	// heldCards overrides where cardsInUse reads this agent's own
	// placements, so the card occupancy rules can be tested without an
	// engine starting containers. Nil means ask the engine.
	heldCards func() engine.HeldCards
	// sleepingCards overrides where sleepingCardPIDs reads the cards of ours
	// whose instances all sleep, for the same tests. Nil means ask the engine.
	sleepingCards                 func() map[string]map[int]bool
	partitionProbeStateMu         sync.Mutex
	partitionProbeInputsSeen      bool
	partitionProbeFingerprintSeen string
	// modelsDirMu guards modelsDir: the server can move the models root at any
	// time through a state sync, while readers (downloads, doctor, path usage)
	// run on other goroutines.
	modelsDirMu sync.RWMutex
	modelsDir   string // absolute root for downloaded models
	dcgmMode    string // "" (== off, the default) | "auto" (opt in) | "off"; see gpu.NewDCGMProbe
	imageRefsMu sync.RWMutex
	imageRefs   []string
	// imageSeen is what the inventory last concluded per image, so only a
	// change is logged (image_presence_log.go).
	imageSeen   imagePresenceLog
	imagePullMu sync.Mutex
	imageOps    imageOperations
	netBench    *netBenchManager

	sendMu sync.Mutex
	send   conn.SendFunc // nil when offline
	// observationConnectionEpoch changes on every Bind and Unbind. Runtime
	// inspection captures it before doing I/O, then may report only through the
	// sender belonging to that same epoch.
	observationConnectionEpoch uint64
	benchPending               []proto.BenchmarkStatus

	// progress bounds outbound status traffic to one report per second per
	// object. Milestones and terminal states bypass it; see progressGate.
	progress *progressGate

	// model-file download bookkeeping
	dlMu          sync.Mutex
	downloading   map[string]*modelDownload
	stoppedCopies map[string]string
	// stoppedDownloads fences canceled attempts across retries and stale replays.
	stoppedDownloads map[string]map[string]bool
	syncMu           sync.Mutex
	modelSyncs       map[string]*activeModelSync
	imageSyncs       map[string]*activeImageSync
	imageSyncMu      sync.Mutex
	imageSyncSends   map[string]context.CancelFunc // source runtime calls, keyed by transfer id

	// netcfg is the effective network/accelerator configuration from the
	// server. The node owns it - downloads, model sync and image pulls all read
	// the same value - and the engine receives a reader for its own pulls.
	netMu  sync.RWMutex
	netcfg proto.NetworkConfig

	// registryAuth is the registry logins from the last state sync; see
	// registryLogins for why it has a type of its own.
	authMu       sync.RWMutex
	registryAuth registryLogins
	// puller pulls images from candidate sources for the engine and for the
	// image.pull RPC alike, so a registry that failed one is remembered by both.
	puller *imagepull.Puller
	// registries remembers which registries and model sites just failed on
	// this node; image pulls and model downloads try them last.
	registries *registryprobe.Prober
	// serverURL is the control plane's address; the Docker daemon's proxy must
	// never be applied to it.
	serverURL string
	// runtimeProxy manages the Docker daemon's proxy. Nil where the agent has
	// no business doing so (not Linux, tests that inject a driver).
	runtimeProxy runtimeProxyManager

	// collectorRunner is nil in production. Tests replace it with a one-shot
	// runner so StartBackground can be verified without waiting ten seconds.
	collectorRunner func(context.Context, *collector.Collector)

	// logFollowerRestarts counts container-restart-style stream ends of the
	// per-instance log followers; every restart is a short shipping gap.
	logFollowerRestarts atomic.Int64

	// logNoiseDropped counts scraper access-log lines dropped before shipping.
	logNoiseDropped atomic.Int64

	// instanceLogs is the per-instance log shipping state that container
	// removals drain (logshipper.go).
	instanceLogs instanceLogs

	// runtimeGPU and occupancy carry the last real answer across a round in
	// which the question could not be asked (runtime_gpu.go,
	// card_occupancy.go).
	runtimeGPU runtimeGPUVerdict
	occupancy  cardOccupancy

	// runtimeChoice is whether auto-detection picks the runtime and what
	// it passed over (runtime_select.go).
	runtimeChoice runtimeChoice
	// kubeNode is set when the agent runs as a Kubernetes node (--driver=kube):
	// its instances are pods, so the runtime is fixed (refuseRuntimeSwitch).
	kubeNode bool
	// accelerator is which vendor's cards this process manages and how
	// that is changed (accelerator_choice.go).
	accelerator acceleratorChoice

	// journal streams node infrastructure logs (journald); nil disables the
	// node-log follower (non-systemd nodes, tests).
	journal journalStreamer

	// debugLog opens debug sessions (debug_session.go); nil answers them as
	// unsupported.
	debugLog *debuglog.Tee
	// logDir is where agent.log is written (the configured state directory,
	// which a separate models directory does not move).
	logDir string
	// selfLog captures the agent's own warn+ slog records; nil disables the
	// self-log follower.
	selfLog *selflog.Collector

	// logNodeDropped counts node log lines dropped over the rate cap.
	logNodeDropped atomic.Int64

	nodeToken      atomic.Value // string
	servingForward *servingForwarder
	perftest       perftest.Runner

	// host is the machine a simulated node stands for (hostsim); nil on a
	// real node.
	host *hostsim.Host

	// stateDir is the agent's own state directory (udocker, runtime assets,
	// caches). It is fixed for the process: the models root can move with a
	// storage change and must not drag the agent's state along.
	stateDir string
	// storage makes the server's storages usable on this node.
	storage *storageRuntime
}

func (n *NodeRuntime) setNetwork(cfg proto.NetworkConfig) {
	n.netMu.Lock()
	n.netcfg = cfg
	n.netMu.Unlock()
	if n.servingForward != nil {
		n.servingForward.apply(cfg.ServingIP)
	}
}

// SetNodeToken records the credential the serving forwarder derives its TLS
// key and instance tokens from. The server may reissue it on registration.
func (n *NodeRuntime) SetNodeToken(token string) {
	n.nodeToken.Store(token)
	if n.servingForward != nil {
		n.servingForward.apply(n.network().ServingIP)
	}
}

func (n *NodeRuntime) currentNodeToken() string {
	token, _ := n.nodeToken.Load().(string)
	return token
}

// instanceAPIPort is the loopback port of a running instance that serves HTTP.
func (n *NodeRuntime) instanceAPIPort(instanceUUID string) (int, bool) {
	if n.engine == nil {
		return 0, false
	}
	for _, spec := range n.engine.RunningSpecs() {
		if spec.UUID == instanceUUID && spec.APIPort > 0 {
			return spec.APIPort, true
		}
	}
	return 0, false
}

// configuredNetwork is the network configuration as the server sent it.
func (n *NodeRuntime) configuredNetwork() proto.NetworkConfig {
	n.netMu.RLock()
	defer n.netMu.RUnlock()
	return n.netcfg
}

// network is the node's network configuration as everything on the node
// applies it. With a proxy set, NoProxy is the effective list - the
// operator's, then loopback and the server's own host
// (dockerproxy.NoProxyFor) - so the agent's own requests, the CLI pulls and
// the Docker daemon's drop-in skip the same hosts.
func (n *NodeRuntime) network() proto.NetworkConfig {
	cfg := n.configuredNetwork()
	if cfg.HTTPProxy != "" || cfg.HTTPSProxy != "" {
		cfg.NoProxy = dockerproxy.NoProxyFor(cfg.NoProxy, n.serverURL)
	}
	return cfg
}

// NewNodeRuntime builds the node runtime. If driver is nil it auto-detects
// docker; tests inject a mock driver. modelsDir is the absolute root for
// downloaded model weights.
func NewNodeRuntime(log *slog.Logger, driver runtime.Driver, modelsDir string, forceDriver ...string) *NodeRuntime {
	fd := ""
	if len(forceDriver) > 0 {
		fd = forceDriver[0]
	}
	return newNodeRuntime(log, driver, modelsDir, fd, nil, nil)
}

// registries is the registry prober every pull on the node shares (newPuller).
func newNodeRuntime(log *slog.Logger, driver runtime.Driver, modelsDir, forceDriver string, prober gpu.Prober, registries *registryprobe.Prober, engineOptions ...engine.EngineOption) *NodeRuntime {
	if prober == nil {
		prober = gpu.New()
	}
	stateDir := filepath.Dir(modelsDir)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		log.Warn("agent state directory unavailable", "path", stateDir, "err", err)
	}
	n := &NodeRuntime{log: log, modelsDir: modelsDir, stateDir: stateDir, prober: prober, downloading: map[string]*modelDownload{}, modelSyncs: map[string]*activeModelSync{}, imageSyncs: map[string]*activeImageSync{}, netBench: newNetBenchManager(), progress: newProgressGate()}
	var mountRunner storagemount.Runner
	if forceDriver == kube.DriverName {
		// The agent's pod mounts NFS on the host, with the host's
		// mount.nfs, where instance pods see it.
		mountRunner = kube.HostExec
	}
	n.storage = newStorageRuntime(stateDir, n.mountBusy, mountRunner)
	n.servingForward = newServingForwarder(log, n.currentNodeToken, n.instanceAPIPort)
	n.perftest = perftest.Host(stateDir)
	if accelerator.HasRuntimeAssetBundles(prober.Vendor()) {
		if installed, err := assets.InstallDomesticRuntimeAssets(filepath.Dir(modelsDir), prober.Vendor()); err != nil {
			log.Warn("domestic runtime asset package unavailable", "vendor", prober.Vendor(), "err", err)
		} else if len(installed) > 0 {
			log.Info("domestic runtime assets installed", "vendor", prober.Vendor(), "version", assets.DomesticRuntimeAssetVersion, "bundles", len(installed))
		}
	}
	// An injected driver is a test harness, whose pulls probe no registry; a
	// detected one is a real node's. Decide before detection replaces nil.
	injected := driver != nil
	if driver == nil {
		selection := detectDriverWaiting(log, filepath.Dir(modelsDir), forceDriver, prober, daemonStartWait)
		driver = selection.driver
		n.runtimeChoice.reset(strings.TrimSpace(forceDriver) == "", selection.fallback)
	}
	n.kubeNode = forceDriver == kube.DriverName || (driver != nil && driver.Name() == kube.DriverName)
	n.puller = n.newPuller(log, injected, registries)
	if driver != nil {
		// The node's puller comes first so a caller's own option can replace it.
		options := append([]engine.EngineOption{engine.WithImagePuller(n.puller)}, engineOptions...)
		options = append(options, engine.WithRuntimeObservationReporter(n.captureObservationConnection, n.reportRuntimeObservation))
		options = append(options, engine.WithNetworkConfig(n.network))
		options = append(options, engine.WithLogDrain(n.drainInstanceLogs))
		if !injected {
			// A test harness's runtime is not the host's: the host's docker
			// images are no source for it.
			options = append(options, engine.WithOtherRuntimes(n.otherRuntimes))
		}
		n.engine = engine.NewWithProber(driver, n.reportInstance, log, filepath.Dir(modelsDir), prober, options...)
		n.partitionProbe = partitionprobe.New(driver, n.engine, prober.Vendor())
	} else if hasPartitionBackendForVendor(prober.Vendor()) {
		// A runner without a driver reports an explicit skipped result, which is
		// distinct from an old agent that did not know probes at all.
		n.partitionProbe = partitionprobe.New(nil, nil, prober.Vendor())
	}
	n.bench = newBenchEngine(n.reportBenchmark, log)
	return n
}

// resolveModelPath turns a (possibly relative) target path into an absolute one
// under the models root. Absolute paths are used as-is (already-resolved).
func (n *NodeRuntime) resolveModelPath(target string) string {
	if filepath.IsAbs(target) {
		return target
	}
	return filepath.Join(n.ModelsRoot(), target)
}

// RuntimeDriverName returns the active driver name ("" if none).
func (n *NodeRuntime) RuntimeDriverName() string {
	d := n.runtimeDriver()
	if d == nil {
		return ""
	}
	return d.Name()
}

func (n *NodeRuntime) runtimeDriver() runtime.Driver {
	if n.engine == nil {
		return nil
	}
	return n.engine.Driver()
}

// ModelsRoot returns the absolute root for downloaded models. The server may
// move it through a state sync, so callers must not assume it never changes.
func (n *NodeRuntime) ModelsRoot() string {
	n.modelsDirMu.RLock()
	defer n.modelsDirMu.RUnlock()
	return n.modelsDir
}

// setModelsRoot is the only writer; the state sync handlers own it.
func (n *NodeRuntime) setModelsRoot(root string) {
	n.modelsDirMu.Lock()
	n.modelsDir = root
	n.modelsDirMu.Unlock()
}

// CachedRuntimeAssetDigests exposes the engine's validated content-addressed
// cache to the connection handshake without coupling conn to engine internals.
func (n *NodeRuntime) CachedRuntimeAssetDigests() []string {
	if n.engine == nil {
		return nil
	}
	return n.engine.CachedRuntimeAssetDigests()
}

// ContainerTerminalArgv exposes the runtime-specific PTY command to conn.
// Docker returns a true exec argv; udocker returns a controlled shell argv
// rebuilt from the tracked workload spec (it is a new proot process, not an
// attach to the model process).
func (n *NodeRuntime) ContainerTerminalArgv(ctx context.Context, id string, cmd []string) ([]string, error) {
	d := n.runtimeDriver()
	if d == nil {
		return nil, errors.New("container terminal runtime is unavailable")
	}
	execer, ok := d.(runtime.PTYExecer)
	if !ok {
		return nil, fmt.Errorf("runtime %s does not support container terminals", d.Name())
	}
	return execer.ExecPTY(ctx, id, cmd)
}

// ContainerTerminalSession binds runtime-specific remote teardown when needed.
// Other drivers retain the existing locally owned PTY/process-tree lifecycle.
func (n *NodeRuntime) ContainerTerminalSession(ctx context.Context, id string, cmd []string) ([]string, func(context.Context) error, error) {
	if driver, ok := n.runtimeDriver().(runtime.PTYSessionExecer); ok {
		return driver.ExecPTYSession(ctx, id, cmd)
	}
	argv, err := n.ContainerTerminalArgv(ctx, id, cmd)
	return argv, nil, err
}

// ContainerTerminalCapabilities reports only capabilities backed by a runtime
// PTY implementation. udocker has its own bit because its shell is simulated
// through a fresh proot invocation with explicit workload context.
func (n *NodeRuntime) ContainerTerminalCapabilities() []string {
	d := n.runtimeDriver()
	if d == nil {
		return nil
	}
	if _, ok := d.(runtime.PTYExecer); !ok {
		return nil
	}
	if strings.EqualFold(d.Name(), "udocker") {
		return []string{proto.CapWebtermContainerUdocker}
	}
	return []string{proto.CapWebtermContainer}
}

// PartitionAssets reports observed files only. Server-side policy decides
// whether those facts make a backend ready for a particular GPU.
func (n *NodeRuntime) PartitionAssets(ctx context.Context) []proto.AssetReport {
	if n.prober == nil {
		return nil
	}
	vendor := accelerator.NormalizeVendor(n.prober.Vendor())
	if vendor == accelerator.VendorNVIDIA {
		if assets.LibvgpuAvailable() || n.host.LibvgpuExtractor() != nil {
			return []proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "embedded://dgpu", Files: []string{"libvgpu.so"}}}
		}
		return []proto.AssetReport{}
	}
	if hasPartitionBackendForVendor(vendor) {
		return engine.ObservedPartitionAssets(ctx, n.stateDir, n.prober)
	}
	return []proto.AssetReport{}
}

// Asset and probe reporting is vendor-wide; SKU admission happens later for
// each card. A strict backend match must not hide observations for that vendor.
func hasPartitionBackendForVendor(vendor string) bool {
	vendor = accelerator.NormalizeVendor(vendor)
	for _, backend := range accelerator.All() {
		if backend.Metadata().Vendor == vendor {
			return true
		}
	}
	return false
}

func (n *NodeRuntime) PartitionProbes(ctx context.Context, reports []proto.AssetReport, gpus []proto.GPUInfo) []proto.PartitionProbeResult {
	if n.partitionProbe == nil {
		return []proto.PartitionProbeResult{}
	}
	if n.partitionProbeNeedsRefresh(reports, gpus) {
		return n.partitionProbe.Refresh(ctx, reports, gpus)
	}
	return n.partitionProbe.Results(ctx, reports, gpus)
}

// CachedPartitionProbes reports the last partition probe results while the
// GPU inventory is unreadable (a stalled vendor CLI). Feeding the missing
// inventory into PartitionProbes instead would count as a GPU change: the
// results would be rerun against no GPUs, wiping them, and rerun again on
// every GPU once the CLI answers.
func (n *NodeRuntime) CachedPartitionProbes() []proto.PartitionProbeResult {
	if n.partitionProbe == nil {
		return []proto.PartitionProbeResult{}
	}
	return n.partitionProbe.Cached()
}

func (n *NodeRuntime) partitionProbeNeedsRefresh(reports []proto.AssetReport, gpus []proto.GPUInfo) bool {
	fingerprint := partitionProbeFingerprint(reports, gpus, n.RuntimeDriverName())
	n.partitionProbeStateMu.Lock()
	defer n.partitionProbeStateMu.Unlock()
	changed := n.partitionProbeInputsSeen && n.partitionProbeFingerprintSeen != fingerprint
	n.partitionProbeInputsSeen = true
	n.partitionProbeFingerprintSeen = fingerprint
	return changed
}

// partitionProbeFingerprint covers what a partition probe's verdict depends
// on: which cards there are, where, and of what kind. Not the facts that
// change between reports without changing that — a card's health finding,
// the processes on it — or every minute's report would rerun a probe
// container on every card.
func partitionProbeFingerprint(reports []proto.AssetReport, gpus []proto.GPUInfo, runtimeName string) string {
	type probedCard struct {
		Index                         int
		UUID, BDF, Vendor, Arch, Name string
		Driver                        string
		MemoryTotalBytes              int64
		Ownership                     string
	}
	normalized := make([]probedCard, 0, len(gpus))
	for _, g := range gpus {
		normalized = append(normalized, probedCard{
			Index: g.Index, UUID: g.UUID, BDF: g.PCIeBDF, Vendor: g.Vendor, Arch: g.ArchID, Name: g.Name,
			Driver: g.DriverVersion, MemoryTotalBytes: g.MemoryTotalBytes, Ownership: g.Ownership,
		})
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Index != normalized[j].Index {
			return normalized[i].Index < normalized[j].Index
		}
		return normalized[i].BDF < normalized[j].BDF
	})
	gpuData, _ := json.Marshal(normalized)
	return partitionAssetFingerprint(reports) + "\x00" + runtimeName + "\x00" + string(gpuData)
}

func partitionAssetFingerprint(reports []proto.AssetReport) string {
	normalized := append([]proto.AssetReport(nil), reports...)
	for i := range normalized {
		normalized[i].Files = append([]string(nil), normalized[i].Files...)
		sort.Strings(normalized[i].Files)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Backend != normalized[j].Backend {
			return normalized[i].Backend < normalized[j].Backend
		}
		if normalized[i].Root != normalized[j].Root {
			return normalized[i].Root < normalized[j].Root
		}
		return strings.Join(normalized[i].Files, "\x00") < strings.Join(normalized[j].Files, "\x00")
	})
	data, _ := json.Marshal(normalized)
	return string(data)
}

func (n *NodeRuntime) StreamLogs(ctx context.Context, ref string, follow bool, w io.Writer) error {
	if n.engine == nil {
		return errUnknownRPC("no runtime")
	}
	name := n.engine.ContainerNameFor(ref)
	rc, err := n.engine.Driver().Logs(ctx, name, follow, 200)
	if err != nil {
		return err
	}
	defer rc.Close()
	// Close the log reader when the server hangs up so io.Copy unblocks.
	go func() {
		<-ctx.Done()
		_ = rc.Close()
	}()
	_, err = io.Copy(w, rc)
	return err
}

// listContainers lists the active runtime's container names (for orphan
// cleanup). It must go through the driver: the hardcoded `docker ps` it used to
// run made every udocker node fail with "executable file not found", which the
// orphan sweeper discarded, so containers whose deployment was already gone
// were never reclaimed.
func (n *NodeRuntime) listContainers(ctx context.Context) ([]string, error) {
	driver := n.runtimeDriver()
	if driver == nil {
		return nil, errors.New("no container runtime is configured")
	}
	return driver.ListContainers(ctx)
}

type rpcError string

func (e rpcError) Error() string   { return string(e) }
func errUnknownRPC(m string) error { return rpcError("unknown rpc method: " + m) }
