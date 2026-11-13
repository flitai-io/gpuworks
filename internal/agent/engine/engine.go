// Package engine owns the lifecycle of inference instances on a node: it turns
// InstanceDesiredState objects into running containers via the runtime driver, probes
// health, reports status back to the server, and reconciles orphans. It is the
// agent-side consumer of the desired-state protocol.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Reporter sends an instance status back to the server (via the ctrl stream).
type Reporter func(proto.InstanceStatus)

// Engine reconciles desired instances against running containers.
type Engine struct {
	driver           runtime.Driver
	hostRuntimeFacts func() accelerator.HostRuntime
	prober           gpu.Prober
	report           Reporter
	log              *slog.Logger
	stateDir         string // for extracted assets (libvgpu), the dGPU usage cache
	runtimeMounts    *RuntimeMountManager
	extractLibvgpu   func(string) (string, error)
	// checkPorts runs before a container is created (checkEnginePorts).
	checkPorts func(proto.InstanceDesiredState) error

	mu          sync.Mutex
	assetMu     sync.Mutex // serializes content-addressed cache publication
	partitionMu sync.Mutex // serializes per-card virtual-id allocation
	desired     map[string]proto.InstanceDesiredState
	// desiredKnown is false until the control plane has sent the full
	// desired set once. Before that an empty `desired` means "not told
	// yet", not "this node runs nothing" — the two look identical and
	// reading the second is how a card the platform itself is using gets
	// called somebody else's (see NodeRuntime.cardsInUse).
	desiredKnown    bool
	inFlight        map[string]bool                       // uuid currently being reconciled
	inFlightCancel  map[string]context.CancelFunc         // cancels startup work before removal waits for it
	cleanupRequired map[string]proto.InstanceDesiredState // old generation must be reclaimed before a replacement starts
	pending         map[string]pendingEnsure
	sleepLevel      map[string]int  // uuid → actual sleep level
	prewarmed       map[string]bool // uuid → sleep prewarm done
	// l1HostHeld names the instances whose engine completed an L1 sleep since
	// its container started (holdsL1HostBuffer).
	l1HostHeld map[string]bool
	// sleepPIDs is, per sleeping instance, the process its engine keeps on
	// each of its cards (by card identity), learned when its residue was
	// measured: the card then held that one process only. It lets the node
	// report a process somebody else starts on a sleeper's card.
	sleepPIDs map[string]map[string]int
	// runtimeEpoch identifies the one serialized start/adopt attempt allowed to
	// publish a runtime observation for an instance. Removal invalidates it
	// before teardown, so a slow inspect cannot publish after replacement.
	runtimeEpoch     map[string]uint64
	nextRuntimeEpoch uint64
	// servingEpoch names, per instance, the start/adopt attempt that reported
	// it running; only such an instance is watched for its container exiting.
	// liveness.go explains why the other maps cannot tell this.
	servingEpoch map[string]uint64
	// Read-only HTTP probes do not occupy inFlight: container exits must still
	// be observed while an engine is slow to answer. One task per instance.
	healthProbes map[string]*healthProbeTask
	// Runtime observations are tied to the ctrl connection that was current
	// before runtime inspection began. The reporter validates that connection
	// epoch again without holding Engine.mu before doing any network I/O.
	captureObservationConnection func() uint64
	reportRuntimeObservation     func(uint64, proto.InstanceStatus) bool

	// residueSettle is the wait between a sleep order and the card memory
	// reading taken as the instance's residue; 0 reads at once.
	residueSettle time.Duration

	headlessProbeInterval time.Duration
	headlessStableChecks  int
	livenessInterval      time.Duration
	healthProbeAttempts   int
	healthProbeInterval   time.Duration
	healthProbeTimeout    time.Duration
	networkInterfaceName  func(string) (string, error)

	// networkConfig reads the node's effective network configuration. The node
	// owns the value (the server can replace it on any state sync); the engine
	// only consumes it, so the two cannot drift apart.
	networkConfig func() proto.NetworkConfig

	// logDrain ships a container's remaining output before removal deletes it.
	logDrain LogDrain

	// puller pulls images from an instance's candidate sources; the node's
	// image.pull RPC shares it (see WithImagePuller).
	puller *imagepull.Puller
	// otherRuntimes lists the node's runtimes besides the active one, to copy
	// a missing image from (WithOtherRuntimes).
	otherRuntimes func(ctx context.Context, active string) []runtime.LocalImageSource
	// cardGate is what a start waits on before launching (SetCardGate).
	cardGate func(ctx context.Context, idents []string, limit time.Duration) bool
}

// WithNetworkConfig injects the node's network configuration reader. Its URL
// rewrite rules cover image pulls from an older server's flat reference list
// (a current server plans the candidates itself) as well as model downloads.
func WithNetworkConfig(read func() proto.NetworkConfig) EngineOption {
	return func(engine *Engine) {
		engine.networkConfig = read
	}
}

// WithLibvgpuExtractor replaces where the NVIDIA dGPU library comes from
// (the agent's embedded copy by default): a simulated node writes a stand-in
// its runtime mounts but never loads.
func WithLibvgpuExtractor(extract func(path string) (string, error)) EngineOption {
	return func(engine *Engine) {
		engine.extractLibvgpu = extract
	}
}

func (e *Engine) network() proto.NetworkConfig {
	if e.networkConfig == nil {
		return proto.NetworkConfig{}
	}
	return e.networkConfig()
}

// New builds an Engine. stateDir holds extracted assets (libvgpu) + HAMi cache.
func New(driver runtime.Driver, report Reporter, log *slog.Logger, stateDir string) *Engine {
	return NewWithProber(driver, report, log, stateDir, nil)
}

// NewWithProber builds an Engine with the GPU prober that owns local device
// injection semantics. A nil prober selects the default host prober.
func NewWithProber(driver runtime.Driver, report Reporter, log *slog.Logger, stateDir string, prober gpu.Prober, options ...EngineOption) *Engine {
	if prober == nil {
		prober = gpu.New()
	}
	if log == nil {
		log = slog.Default()
	}
	result := &Engine{
		driver:                driver,
		prober:                prober,
		report:                report,
		log:                   log,
		stateDir:              stateDir,
		runtimeMounts:         newRuntimeMountManager(stateDir, driver),
		extractLibvgpu:        assets.ExtractLibvgpu,
		checkPorts:            portCheckFor(driver),
		desired:               map[string]proto.InstanceDesiredState{},
		inFlight:              map[string]bool{},
		inFlightCancel:        map[string]context.CancelFunc{},
		cleanupRequired:       map[string]proto.InstanceDesiredState{},
		pending:               map[string]pendingEnsure{},
		sleepLevel:            map[string]int{},
		prewarmed:             map[string]bool{},
		l1HostHeld:            map[string]bool{},
		sleepPIDs:             map[string]map[string]int{},
		runtimeEpoch:          map[string]uint64{},
		servingEpoch:          map[string]uint64{},
		healthProbes:          map[string]*healthProbeTask{},
		residueSettle:         defaultResidueSettle,
		headlessProbeInterval: time.Second,
		headlessStableChecks:  3,
		livenessInterval:      defaultLivenessInterval,
		healthProbeAttempts:   runningHealthAttempts,
		healthProbeInterval:   engineHealthRetryInterval,
		healthProbeTimeout:    engineHealthTimeout,
		networkInterfaceName:  networkInterfaceNameForIP,
	}
	for _, option := range options {
		if option != nil {
			option(result)
		}
	}
	if result.puller == nil {
		result.puller = imagepull.New(imagepull.Config{Log: log, Network: result.network})
	}
	return result
}

// teardown stops and removes an instance's container.
//
// A failed Remove leaves the container rootfs behind and, on udocker, leaves the
// proot process tree holding the GPU while the control plane has already
// forgotten the deployment, so it must reach the control plane rather than only
// the agent log: the instance is reported as failed instead of stopped, and the
// deployment delete path can retry the removal it is waiting on.
func (e *Engine) teardown(ctx context.Context, uuid string) bool {
	if err := e.reclaimInstance(ctx, uuid); err != nil {
		e.log.Error("instance teardown pending; resources may still be held", "uuid", uuid, "err", err)
		e.report(proto.InstanceStatus{UUID: uuid, State: proto.InstRemoving, Message: "teardown pending: " + err.Error()})
		return false
	}
	e.report(proto.InstanceStatus{UUID: uuid, State: proto.InstStopped})
	e.log.Info("instance torn down", "uuid", uuid)
	return true
}

// reclaimInstance leaves partition state intact until the container has been
// confirmed absent. Both teardown and failed startup use this ordering.
func (e *Engine) reclaimInstance(ctx context.Context, uuid string) error {
	if err := e.removeContainer(ctx, uuid); err != nil {
		return err
	}
	if err := e.cleanupPartitionState(uuid); err != nil {
		return fmt.Errorf("remove partition state: %w", err)
	}
	if err := os.RemoveAll(e.preStartDir(uuid)); err != nil {
		return fmt.Errorf("remove pre-start state: %w", err)
	}
	return nil
}

func (e *Engine) failAfterContainer(ctx context.Context, spec proto.InstanceDesiredState, message string, measured ...json.RawMessage) {
	var observation json.RawMessage
	if len(measured) > 0 {
		observation = measured[0]
	}
	if err := e.reclaimInstance(ctx, spec.UUID); err != nil {
		e.log.Error("failed instance cleanup pending", "uuid", spec.UUID, "err", err)
		e.report(proto.InstanceStatus{UUID: spec.UUID, State: proto.InstRemoving, Message: message + "; cleanup pending: " + err.Error(), Measured: observation})
		return
	}
	e.report(proto.InstanceStatus{UUID: spec.UUID, State: proto.InstFailed, Message: message, Measured: observation})
}

// removeContainer stops and deletes the instance's container, reporting the
// removal error (nil when there was nothing to remove).
func (e *Engine) removeContainer(ctx context.Context, uuid string) error {
	name := containerName(uuid)
	d := e.Driver()
	st, err := d.Status(ctx, name)
	if err != nil {
		return fmt.Errorf("inspect container %s: %w", name, err)
	}
	if !st.Exists {
		return nil
	}
	e.log.Info("stopping instance container", "uuid", uuid, "container", name)
	if err := d.Stop(ctx, name); err != nil {
		// Remove is forced, so a stop that merely timed out is still reclaimed
		// below; the failure only loses the graceful shutdown window.
		e.log.Warn("instance container stop failed; continuing with forced removal", "uuid", uuid, "container", name, "err", err)
	}
	// Removal deletes the container's output with it. Draining after Stop
	// also catches the engine's own shutdown lines.
	release := e.drainLogs(ctx, uuid, !st.Running)
	defer release()
	if err := d.Remove(ctx, name); err != nil {
		return fmt.Errorf("remove container %s: %w", name, err)
	}
	return nil
}

const (
	maxPreStartScriptBytes = 64 << 10
	preStartMountPath      = "/gpuworks/pre-start.sh"
)

func (e *Engine) preStartDir(uuid string) string {
	manager := e.runtimeMountManager()
	return filepath.Join(manager.purposeRoot("pre-start"), sanitizeID(uuid))
}

func (e *Engine) runtimeMountManager() *RuntimeMountManager {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.runtimeMounts == nil {
		e.runtimeMounts = newRuntimeMountManager(e.stateDir, e.driver)
	}
	return e.runtimeMounts
}

// applyPreStart installs a per-instance wrapper inside the container. The
// wrapper runs on every container start and then execs the engine command with
// the original argument boundaries intact.
func (e *Engine) applyPreStart(ws *runtime.WorkloadSpec, spec proto.InstanceDesiredState) error {
	hasArgv := len(spec.PreStartArgv) > 0
	hasScript := spec.PreStartScript != ""
	if !hasArgv && !hasScript {
		return nil
	}
	if hasArgv && hasScript {
		return fmt.Errorf("pre_start_argv and pre_start_script are mutually exclusive")
	}
	if len(spec.PreStartScript) > maxPreStartScriptBytes {
		return fmt.Errorf("pre_start_script exceeds 64 KiB")
	}
	if len(ws.Entrypoint) == 0 {
		return fmt.Errorf("pre-start requires an explicit engine entrypoint")
	}
	for index, arg := range spec.PreStartArgv {
		if strings.TrimSpace(arg) == "" {
			return fmt.Errorf("pre_start_argv[%d] is empty", index)
		}
	}

	dir, err := e.runtimeMountManager().PrepareInstanceDir("pre-start", sanitizeID(spec.UUID))
	if err != nil {
		return fmt.Errorf("create wrapper directory: %w", err)
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\nset -eu\n")
	if hasArgv {
		for index, arg := range spec.PreStartArgv {
			if index > 0 {
				script.WriteByte(' ')
			}
			script.WriteString(shellSingleQuote(arg))
		}
		script.WriteByte('\n')
	} else {
		script.WriteString(spec.PreStartScript)
		if !strings.HasSuffix(spec.PreStartScript, "\n") {
			script.WriteByte('\n')
		}
	}
	script.WriteString("exec \"$@\"\n")
	owner, err := e.runtimeMountManager().mountOwner()
	if err != nil {
		return fmt.Errorf("resolve wrapper owner: %w", err)
	}
	hostPath, err := e.runtimeMountManager().writeReadOnlyFileAtomic(dir, "pre-start.sh", []byte(script.String()), owner)
	if err != nil {
		return fmt.Errorf("write wrapper: %w", err)
	}
	e.log.Info("installed pre-start wrapper", "uuid", spec.UUID, "path", hostPath)

	engineCommand := append([]string(nil), ws.Entrypoint...)
	engineCommand = append(engineCommand, ws.Cmd...)
	ws.Entrypoint = []string{"/bin/sh", preStartMountPath}
	ws.Cmd = engineCommand
	ws.Mounts = append(ws.Mounts, runtime.Mount{Source: hostPath, Target: preStartMountPath, ReadOnly: true})
	return nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (e *Engine) reportState(spec proto.InstanceDesiredState, state, id, addr, msg string) {
	e.report(proto.InstanceStatus{
		UUID: spec.UUID, State: state, ContainerID: id, Message: msg, DirectAddr: addr,
	})
}

func (e *Engine) reportStateProgress(spec proto.InstanceDesiredState, state, id, addr, msg, phase string, progress float64, done, total, rate int64) {
	e.report(proto.InstanceStatus{
		UUID: spec.UUID, State: state, ContainerID: id, Message: msg, DirectAddr: addr,
		Phase: phase, Progress: progress, BytesDone: done, BytesTotal: total, RateBPS: rate,
	})
}

// toWorkloadSpec converts a desired InstanceDesiredState into a driver WorkloadSpec.
func toWorkloadSpec(spec proto.InstanceDesiredState, name string) runtime.WorkloadSpec {
	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = v
	}
	ws := runtime.WorkloadSpec{
		Name:        name,
		Image:       spec.Image,
		Entrypoint:  spec.Entrypoint,
		Cmd:         spec.Argv,
		Env:         env,
		GPUIndexes:  spec.GPUIndexes,
		HostNetwork: spec.HostNetwork,
		ShmBytes:    spec.ShmBytes,
		Ports:       spec.Ports,
		DockerArgs:  spec.DockerArgs,
	}
	for _, m := range spec.Mounts {
		ws.Mounts = append(ws.Mounts, runtime.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	if spec.ModelPath != "" && spec.ModelMount != "" {
		ws.Mounts = append(ws.Mounts, runtime.Mount{
			Source: spec.ModelPath, Target: spec.ModelMount, ReadOnly: true,
		})
	}
	return ws
}

// CleanupOrphans removes gpuworks-managed containers not in the desired set,
// then the KV disk tier directories of deployments with no desired instance.
// Called on connect after SetDesiredFull to reconcile leftovers.
//
// This is the only reclamation path for containers whose deployment is already
// gone from the control plane, so nothing here may fail silently: a listing that
// errors out (which is what an udocker node used to produce, when the sweep was
// hardcoded to `docker ps`) reclaims nothing while looking like a clean node.
// The error is therefore returned to the caller instead of being dropped, along
// with the per-container failures that leave a rootfs behind.
func (e *Engine) CleanupOrphans(ctx context.Context, listContainers func(ctx context.Context) ([]string, error)) error {
	if listContainers == nil {
		return errors.New("orphan cleanup requires a container lister")
	}
	names, err := listContainers(ctx)
	if err != nil {
		return fmt.Errorf("list containers for orphan cleanup: %w", err)
	}
	e.mu.Lock()
	keep := map[string]bool{}
	for uuid := range e.desired {
		keep[containerName(uuid)] = true
	}
	e.mu.Unlock()
	var failures []string
	d := e.Driver()
	for _, n := range names {
		if !strings.HasPrefix(n, "gpuworks-") || keep[n] {
			continue
		}
		e.log.Info("removing orphan container", "name", n)
		if err := d.Stop(ctx, n); err != nil {
			e.log.Warn("orphan container stop failed", "name", n, "err", err)
			failures = append(failures, fmt.Sprintf("%s: stop: %v", n, err))
			continue
		}
		if err := d.Remove(ctx, n); err != nil {
			e.log.Warn("orphan container removal failed; rootfs may remain", "name", n, "err", err)
			failures = append(failures, fmt.Sprintf("%s: remove: %v", n, err))
		}
	}
	if len(failures) > 0 {
		// A container that survived may still have its disk tier mounted: the
		// directories wait for a sweep that removed every orphan.
		return fmt.Errorf("orphan container cleanup failed for %d container(s): %s", len(failures), strings.Join(failures, "; "))
	}
	return e.reclaimOrphanKVDiskDirs()
}

// Driver exposes the runtime driver (for logtail).
func (e *Engine) Driver() runtime.Driver {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.driver
}

// RunningSpecs returns desired-running, awake instance specs for log shipping.
// Headless workers are included because their startup logs are essential for
// diagnosing distributed failures.
func (e *Engine) RunningSpecs() []proto.InstanceDesiredState {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []proto.InstanceDesiredState
	for _, s := range e.desired {
		if s.DesiredState == "running" && e.sleepLevel[s.UUID] == 0 {
			out = append(out, s)
		}
	}
	return out
}

// DesiredKnown reports whether the control plane has told this agent what
// it should be running. Until it has, this agent knows nothing about its
// own instances, which is not the same as having none.
func (e *Engine) DesiredKnown() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.desiredKnown
}

// MetricSpecs returns only instances that expose an HTTP metrics endpoint.
// Distributed headless workers deliberately have no API port.
func (e *Engine) MetricSpecs() []proto.InstanceDesiredState {
	all := e.RunningSpecs()
	out := make([]proto.InstanceDesiredState, 0, len(all))
	for _, spec := range all {
		if !spec.Headless && spec.APIPort > 0 {
			out = append(out, spec)
		}
	}
	return out
}

// ContainerNameFor returns the container name for an instance uuid (logtail).
func (e *Engine) ContainerNameFor(uuid string) string { return containerName(uuid) }
