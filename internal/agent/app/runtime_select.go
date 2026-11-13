package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/agent/runtime/hostproc"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	nerdctldrv "gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	udockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Which container runtime a node uses.
//
// An operator can name one (--driver, or a runtime preference other than
// auto) and gets exactly that, whatever it can do. Otherwise the agent
// picks: the first runtime that answers, in the order host → docker →
// nerdctl → runveil — except that on an NVIDIA node a runtime that answers
// and cannot give containers the cards (no Container Toolkit, or a daemon
// never configured for it) is passed over for runveil, which binds the
// device nodes itself. Taking it would only cordon the node
// (runtime_gpu.go) while a runtime that can serve it sits unused. What was
// passed over, and why, is reported so the operator can see the node runs
// on its fallback and how to get the runtime it would have preferred.
//
// The fallback is watched: once the passed-over runtime can give containers
// the cards, the agent switches back to it as soon as no instance would be
// disturbed, and says it is ready until then.

// runtimeFallback is a runtime auto-detection passed over, and why: a code
// from accelerator.RuntimeGPUBlocked*, or accelerator.RuntimeGPUReady once
// it can serve after all. The zero value is no fallback.
type runtimeFallback struct {
	from, reason string
}

// runtimeSelection is what detectDriver chose. unanswered is set when the
// chosen runtime needed asking about the accelerators and its daemon could
// not be asked: taken on trust at startup, but no reason to leave a
// runtime that is known to work.
type runtimeSelection struct {
	driver     runtime.Driver
	fallback   runtimeFallback
	unanswered bool
}

// runtimeChoice is whether the agent picks the runtime itself, and what its
// last pick passed over.
type runtimeChoice struct {
	mu       sync.Mutex
	auto     bool
	fallback runtimeFallback
}

func (c *runtimeChoice) reset(auto bool, fallback runtimeFallback) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auto = auto
	c.fallback = fallback
}

func (c *runtimeChoice) get() (bool, runtimeFallback) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auto, c.fallback
}

// RuntimeFallback is the runtime auto-detection passed over and the code
// saying why, both "" when it passed over none. The status report carries
// it; the minute-level watch (watchRuntimeFallback) keeps it current.
func (n *NodeRuntime) RuntimeFallback() (from, reason string) {
	_, fallback := n.runtimeChoice.get()
	return fallback.from, fallback.reason
}

// driverUnavailableOn says why a forced driver cannot run on goos, or "".
// The host driver runs macOS environment packages, which exist only for
// darwin; runveil (udocker) is PRoot on Linux ptrace, which macOS lacks.
func driverUnavailableOn(goos, driver string) string {
	switch {
	case driver == hostproc.DriverName && !hostproc.SupportedOn(goos):
		return "the host runtime runs macOS environment packages and is only available on macOS"
	case driver == "udocker" && goos == "darwin":
		return "the runveil runtime needs Linux; a Mac uses the host runtime"
	}
	return ""
}

// detectDriver returns the forced driver, or auto-selects one when force is
// empty: the macOS host runtime, docker (with a usable daemon), nerdctl, then
// udocker for no-root/no-daemon nodes (autodl/compshare containers). A nil
// driver means none is usable. On macOS the native driver comes first even
// when Docker Desktop answers: its containers run in a Linux VM that has no
// access to the Metal GPU. prober decides which runtimes can serve the
// node's accelerators; nil skips that question.
func detectDriver(log *slog.Logger, baseDir, force string, prober gpu.Prober) runtimeSelection {
	return detectDriverWaiting(log, baseDir, force, prober, 0)
}

// daemonStartWait is how long the agent's first detection waits for a
// container daemon that is installed but not answering (runtime.
// ErrDaemonNotReady) before passing it over. It covers dockerd still
// starting at boot and dockerd busy pulling a large image; taking the next
// runtime at once would start the node's instances there, where they stay.
// The minute-level recheck covers a daemon slower than this. A variable so
// a test need not wait.
var daemonStartWait = 2 * time.Minute

// daemonRetryDelay is the first pause between asks while waiting; it doubles
// up to 15 seconds. A variable for the same reason.
var daemonRetryDelay = 2 * time.Second

// detectDriverWaiting is detectDriver that waits up to wait for an
// installed daemon to answer. Only startup waits; the recheck asks once.
func detectDriverWaiting(log *slog.Logger, baseDir, force string, prober gpu.Prober, wait time.Duration) runtimeSelection {
	ctx := context.Background()
	force = strings.ToLower(strings.TrimSpace(force))
	if reason := driverUnavailableOn(goruntime.GOOS, force); reason != "" {
		log.Error("runtime driver not available on this OS", "driver", force, "os", goruntime.GOOS, "reason", reason)
		return runtimeSelection{}
	}
	switch force {
	case "none":
		log.Info("runtime driver disabled", "driver", "none")
		return runtimeSelection{}
	case "udocker":
		log.Info("runtime driver forced", "driver", "udocker")
		return runtimeSelection{driver: udockerdrv.New(baseDir)}
	case "docker":
		return runtimeSelection{driver: dockerdrv.New()}
	case "nerdctl":
		log.Info("runtime driver forced", "driver", "nerdctl")
		return runtimeSelection{driver: nerdctldrv.New()}
	case hostproc.DriverName:
		log.Info("runtime driver forced", "driver", hostproc.DriverName)
		return runtimeSelection{driver: hostproc.New(baseDir)}
	case kube.DriverName:
		d, err := kube.FromEnvironment(baseDir, cardUUIDs(prober), cardBDFs(prober), cardDevices(prober, log), log)
		if err == nil {
			_, err = d.Detect(ctx)
		}
		if err != nil {
			log.Error("kube runtime unavailable", "err", err)
			return runtimeSelection{}
		}
		log.Info("runtime driver forced", "driver", kube.DriverName)
		return runtimeSelection{driver: d}
	case "":
		// auto-detect below
	default:
		log.Error("invalid runtime driver", "driver", force, "allowed", "docker|udocker|nerdctl|host|kube|none")
		return runtimeSelection{}
	}
	candidates := []struct {
		build func() runtime.Driver
		label string
	}{
		{func() runtime.Driver { return hostproc.New(baseDir) }, hostproc.DriverName + " (macOS host processes)"},
		{func() runtime.Driver { return dockerdrv.New() }, "docker"},
		{func() runtime.Driver { return nerdctldrv.New() }, "nerdctl (no docker daemon)"},
		{func() runtime.Driver { return udockerdrv.New(baseDir) }, "udocker (no docker daemon)"},
	}
	var fallback runtimeFallback
	for _, candidate := range candidates {
		driver := candidate.build()
		ok, err := driver.Detect(ctx)
		if !ok && errors.Is(err, runtime.ErrDaemonNotReady) && wait > 0 {
			ok, err = waitForDaemon(ctx, log, driver, err, wait)
		}
		if !ok {
			if errors.Is(err, runtime.ErrDaemonNotReady) {
				log.Warn("runtime driver passed over: its daemon is installed but not answering; rechecking every minute",
					"driver", driver.Name(), "err", err)
				if fallback.from == "" {
					fallback = runtimeFallback{from: driver.Name(), reason: proto.RuntimeFallbackDaemonNotReady}
				}
			}
			continue
		}
		blocked, answered := passOver(ctx, log, prober, driver)
		if blocked != "" {
			log.Warn("runtime driver passed over: it cannot give containers this node's accelerators",
				"driver", driver.Name(), "reason", blocked)
			if fallback.from == "" {
				fallback = runtimeFallback{from: driver.Name(), reason: blocked}
			}
			continue
		}
		label := candidate.label
		switch {
		case fallback.reason == proto.RuntimeFallbackDaemonNotReady:
			label = fmt.Sprintf("%s (%s daemon not answering)", driver.Name(), fallback.from)
		case fallback.from != "":
			label = fmt.Sprintf("%s (%s cannot use the accelerators: %s)", driver.Name(), fallback.from, fallback.reason)
		}
		log.Info("runtime driver detected", "driver", label)
		return runtimeSelection{driver: driver, fallback: fallback, unanswered: !answered}
	}
	log.Warn("no usable container runtime (docker/nerdctl/udocker); deployments disabled on this node")
	return runtimeSelection{}
}

// waitForDaemon asks driver again, backing off from daemonRetryDelay to 15
// seconds, until
// its daemon answers, it stops being merely not ready, or wait has passed.
func waitForDaemon(ctx context.Context, log *slog.Logger, driver runtime.Driver, err error, wait time.Duration) (bool, error) {
	log.Info("runtime daemon installed but not answering; waiting for it", "driver", driver.Name(), "wait", wait, "err", err)
	deadline := time.Now().Add(wait)
	delay := min(daemonRetryDelay, wait)
	for time.Now().Before(deadline) {
		time.Sleep(min(delay, time.Until(deadline)))
		ok, detectErr := driver.Detect(ctx)
		if ok || !errors.Is(detectErr, runtime.ErrDaemonNotReady) {
			return ok, detectErr
		}
		err = detectErr
		delay = min(2*delay, 15*time.Second)
	}
	return false, err
}

// passOver is why auto-detection should not take driver on this node, or ""
// to take it. Only a definite answer passes a runtime over: a daemon that
// could not be asked (answered=false) is taken, as the status report keeps
// its last answer rather than guess.
func passOver(ctx context.Context, log *slog.Logger, prober gpu.Prober, driver runtime.Driver) (blocked string, answered bool) {
	if prober == nil || !accelerator.RuntimeGPUFallback(prober.Vendor()) {
		return "", true
	}
	if !accelerator.RuntimeNeedsGPUPlugin(prober.Vendor(), driver.Name()) {
		return "", true
	}
	return judgeRuntimeGPU(ctx, log, prober, driver, runtime.DetectPlugin(ctx, prober, driver.Name()))
}

// refuseRuntimeSwitch says why a Kubernetes node's runtime cannot be replaced
// by target: its instances are pods of the cluster, and the nerdctl, docker
// or udocker the host may also have would run them outside Kubernetes, with
// cards the kubelet can hand to other pods. nil when the node is no
// Kubernetes node, or target is its own runtime.
func (n *NodeRuntime) refuseRuntimeSwitch(target string) error {
	if !n.kubeNode || target == kube.DriverName {
		return nil
	}
	return fmt.Errorf("this node is a Kubernetes node: its runtime is %s and cannot be switched to %s (instances run as pods of the cluster)", kube.DriverName, target)
}

// SwitchRuntime changes the active runtime for subsequent reconciles. Existing
// managed containers are stopped by the engine and recreated with the target
// driver. The operation is intentionally available even when the target's
// vendor plugin is absent; Doctor will expose that fact separately.
func (n *NodeRuntime) SwitchRuntime(ctx context.Context, name string) error {
	n.switchMu.Lock()
	defer n.switchMu.Unlock()
	name = strings.ToLower(strings.TrimSpace(name))
	if err := n.refuseRuntimeSwitch(name); err != nil {
		return err
	}
	if name != "docker" && name != "nerdctl" && name != "udocker" {
		return fmt.Errorf("unsupported runtime %q", name)
	}
	driver := detectDriver(n.log, n.stateDir, name, nil).driver
	if driver == nil {
		return fmt.Errorf("runtime %s is unavailable", name)
	}
	if ok, err := driver.Detect(ctx); err != nil || !ok {
		if err != nil {
			return fmt.Errorf("detect runtime %s: %w", name, err)
		}
		return fmt.Errorf("runtime %s is unavailable", name)
	}
	if err := n.switchToDriver(ctx, driver); err != nil {
		return err
	}
	n.runtimeChoice.reset(false, runtimeFallback{})
	return nil
}

func (n *NodeRuntime) switchToDriver(ctx context.Context, driver runtime.Driver) error {
	if old := n.runtimeDriver(); old != nil && strings.EqualFold(old.Name(), driver.Name()) {
		return nil
	}
	if n.engine == nil {
		// No engine was built at startup, so no container can run on this node;
		// there is nothing to switch and nothing to report as active.
		return nil
	}
	if err := n.engine.SwitchDriver(ctx, driver); err != nil {
		return err
	}
	if n.partitionProbe != nil {
		n.partitionProbe.SetDriver(driver)
	}
	return nil
}

// ApplyRuntimePreference applies an operator's runtime choice now. Empty and
// "auto" both hand the choice back to auto-detection, which takes effect at
// once even if instances have to be recreated: the operator asked for it.
func (n *NodeRuntime) ApplyRuntimePreference(ctx context.Context, preference string) error {
	return n.applyRuntimePreference(ctx, preference, false)
}

// applyRuntimePreference is ApplyRuntimePreference; with idleOnly, an
// auto-detected runtime other than the current one is switched to only when
// that disturbs no instance. The server replays the preference on every
// connect, and a replay is not a request to recreate what runs.
func (n *NodeRuntime) applyRuntimePreference(ctx context.Context, preference string, idleOnly bool) error {
	preference = strings.ToLower(strings.TrimSpace(preference))
	if preference == "" || preference == proto.RuntimePreferenceAuto {
		return n.applyAutoRuntime(ctx, idleOnly)
	}
	if !proto.ValidRuntimePreference(preference) {
		return fmt.Errorf("unsupported runtime preference %q", preference)
	}
	return n.SwitchRuntime(ctx, preference)
}

// applyAutoRuntime runs auto-detection and moves to its pick. With idleOnly
// and instances on the node, the current runtime stays; if the pick is the
// runtime the node fell back from, it is reported ready instead.
func (n *NodeRuntime) applyAutoRuntime(ctx context.Context, idleOnly bool) error {
	n.switchMu.Lock()
	defer n.switchMu.Unlock()
	if n.kubeNode {
		// Auto-detection would find the node's nerdctl (the host wrapper the
		// kube runtime installed) and move the instances out of their pods.
		n.runtimeChoice.reset(false, runtimeFallback{})
		return nil
	}
	// Detection runs again every minute on a node with something to watch;
	// what it finds is logged below only when it changes, not each round.
	selection := detectDriver(slog.New(slog.DiscardHandler), n.stateDir, "", n.prober)
	if selection.driver == nil {
		return errors.New("no usable container runtime")
	}
	current := n.runtimeDriver()
	if current != nil && !strings.EqualFold(current.Name(), selection.driver.Name()) {
		// The current runtime may stay below, but the choice is the agent's
		// from now on, so the recheck keeps watching it.
		_, fallback := n.runtimeChoice.get()
		if selection.unanswered {
			// Nothing is known about the pick; the current runtime stays.
			n.runtimeChoice.reset(true, fallback)
			return nil
		}
		if idleOnly && n.engine != nil && !n.engine.Idle() {
			if fallback.from != "" && strings.EqualFold(fallback.from, selection.driver.Name()) && fallback.reason != accelerator.RuntimeGPUReady {
				n.log.Info("runtime driver can use the accelerators now; switching once no instance runs",
					"driver", selection.driver.Name(), "current", current.Name())
				fallback.reason = accelerator.RuntimeGPUReady
			}
			n.runtimeChoice.reset(true, fallback)
			return nil
		}
	}
	if err := n.switchToDriver(ctx, selection.driver); err != nil {
		return err
	}
	_, before := n.runtimeChoice.get()
	n.runtimeChoice.reset(true, selection.fallback)
	if current != nil && !strings.EqualFold(current.Name(), selection.driver.Name()) {
		n.log.Info("runtime driver switched by auto-detection", "from", current.Name(), "to", selection.driver.Name())
	}
	if selection.fallback != before && selection.fallback.from != "" {
		n.log.Warn("runtime driver passed over: it cannot give containers this node's accelerators",
			"driver", selection.fallback.from, "reason", selection.fallback.reason, "using", selection.driver.Name())
	}
	return nil
}

// runtimeFallbackInterval is how often a fallback is rechecked. It matches
// the status report, which is when anyone would see the result, and costs
// one `docker info` on the few nodes that have something to watch. A
// variable so a test need not wait a minute.
var runtimeFallbackInterval = time.Minute

// watchRuntimeFallback rechecks auto-detection while there is something to
// watch, for the life of the agent.
func (n *NodeRuntime) watchRuntimeFallback(ctx context.Context) {
	ticker := time.NewTicker(runtimeFallbackInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.recheckRuntimeFallback(ctx)
		}
	}
}

// recheckRuntimeFallback reruns auto-detection when the node picks its own
// runtime and either runs on a fallback or its runtime cannot use the cards
// (it lost them after detection, or could not be asked then). A runtime
// that serves the node needs no second look.
func (n *NodeRuntime) recheckRuntimeFallback(ctx context.Context) {
	auto, fallback := n.runtimeChoice.get()
	if !auto || n.engine == nil {
		return
	}
	if fallback.from == "" && n.runtimeGPU.get() == "" {
		return
	}
	if err := n.applyAutoRuntime(ctx, true); err != nil {
		n.log.Warn("runtime fallback recheck failed", "err", err)
	}
}
