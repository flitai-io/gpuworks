package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/kubeboot"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
)

// kubeAcquireTokenOrStandBy prepares a Kubernetes driver's run
// (prepareKubeRun) and, when conf has no token yet, trades the cluster's
// enrollment token for one — storing it and the cluster's idle-return grace
// period onto conf and cfg — or stands by instead (kubeStandBy): no
// accelerator this build can manage on Kubernetes is on the bus yet.
// stoodBy true means run must return now, with err exactly as is (possibly
// nil, from ctx ending or a failed restart).
func kubeAcquireTokenOrStandBy(log *slog.Logger, conf *agentconf.Config, cfg *agentapp.Config, stateDir string) (stoodBy bool, err error) {
	if err := prepareKubeRun(stateDir); err != nil {
		return true, err
	}
	if strings.TrimSpace(conf.Token) != "" {
		return false, nil
	}
	ectx, estop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer estop()
	// An injected prober (GPUWORKS_FAKE_GPU) stands for the node's cards:
	// the bus is not what the agent will probe.
	if cfg.Prober == nil {
		if stoodBy, err := kubeStandBy(ectx, log, stateDir, kubeCensusFor(cfg.AcceleratorHost)); stoodBy {
			return true, err
		}
	}
	id, err := enrollKubeNode(ectx, log, agentapp.Config{ServerURL: conf.Server, StateDir: stateDir})
	if err != nil {
		return true, err
	}
	conf.Token, cfg.KubeIdleReturn = id.NodeToken, id.IdleReturn()
	return false, nil
}

// kubeEnrollRetryDelay is the first wait between enrollment attempts (it
// doubles up to a minute); a test shortens it.
var kubeEnrollRetryDelay = 2 * time.Second

// kubeEnrollRefusalEventInterval is how often a persisting refusal is
// written again as an event on the agent's pod; a test shortens it.
var kubeEnrollRefusalEventInterval = kube.EnrollRefusalEventInterval

// enrollKubeNode makes an agent of a cluster's DaemonSet a node: it trades
// the cluster's enrollment token for this node's token, retrying until the
// server answers, since the DaemonSet may be applied before the server can
// be reached.
func enrollKubeNode(ctx context.Context, log *slog.Logger, cfg agentapp.Config) (kube.Identity, error) {
	api, err := kubeapi.InCluster()
	if err != nil {
		return kube.Identity{}, err
	}
	return enrollKubeNodeWith(ctx, log, cfg, api)
}

// enrollKubeNodeWith is enrollKubeNode with its Kubernetes API client
// injected, so a test can supply kubefake's instead of a real in-cluster
// one. When the identity that comes back names a different node UUID than
// the one this state directory last saved (a reinstall under another
// cluster entry, reusing the same hostPath), the node's saved accelerator
// preference is cleared: it was the old entry's choice, not this one's.
func enrollKubeNodeWith(ctx context.Context, log *slog.Logger, cfg agentapp.Config, api *kubeapi.Client) (kube.Identity, error) {
	enrollToken := strings.TrimSpace(os.Getenv("GPUWORKS_KUBE_ENROLL_TOKEN"))
	nodeName := strings.TrimSpace(os.Getenv("GPUWORKS_KUBE_NODE_NAME"))
	if enrollToken == "" || nodeName == "" {
		return kube.Identity{}, fmt.Errorf("--driver=kube without --token needs GPUWORKS_KUBE_ENROLL_TOKEN and GPUWORKS_KUBE_NODE_NAME (the DaemonSet manifest sets them)")
	}
	previous := kube.ReadIdentity(cfg.StateDir)
	httpc := &http.Client{Timeout: 30 * time.Second}
	delay := kubeEnrollRetryDelay
	fallback := true
	reporter := &kube.RefusalReporter{
		API: api, Namespace: strings.TrimSpace(os.Getenv("GPUWORKS_KUBE_NAMESPACE")),
		PodName: strings.TrimSpace(os.Getenv("GPUWORKS_KUBE_POD_NAME")), NodeName: nodeName, Log: log,
		Interval: kubeEnrollRefusalEventInterval,
	}
	for {
		id, err := kube.Enroll(ctx, api, httpc, cfg.ServerURL, enrollToken, nodeName, cfg.StateDir, fallback)
		var refusal *kube.EnrollRefusal
		if errors.As(err, &refusal) {
			fallback = false // a refusal of this node's identity is not undone by a later outage
			// Also show it on `kubectl describe pod`; never affects the retry.
			reporter.Report(ctx, refusal)
		}
		if err == nil {
			log.Info("enrolled as a Kubernetes node", "node", nodeName, "uuid", id.NodeUUID)
			if previous.NodeUUID != "" && previous.NodeUUID != id.NodeUUID {
				if clearErr := agentapp.ClearAcceleratorPreference(cfg.StateDir); clearErr != nil {
					log.Warn("could not clear the saved accelerator preference for the new node identity", "err", clearErr)
				} else {
					log.Info("cleared the saved accelerator preference: this state directory now belongs to a different node identity", "previous_uuid", previous.NodeUUID, "uuid", id.NodeUUID)
				}
			}
			return id, nil
		}
		log.Warn("enrollment failed; retrying", "err", err, "in", delay.String())
		select {
		case <-ctx.Done():
			return kube.Identity{}, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, time.Minute)
	}
}

// alignKubeAgentTimeout bounds taking the server's binary on a first start:
// the agent registers with the binary it has rather than wait longer.
const alignKubeAgentTimeout = 3 * time.Minute

// alignKubeAgent runs before a Kubernetes agent registers. An agent the
// image's binary handed over to (kubeboot) takes the server's own binary
// once after the state directory was seeded, so a new node runs the
// server's version as an installed agent does. One that kept running the
// image's binary says why: its upgrades then change the DaemonSet's image.
func alignKubeAgent(log *slog.Logger, serverURL, token string) {
	if reason := os.Getenv(kubeboot.UnavailableEnv); reason != "" {
		log.Warn("running the agent image's own binary; upgrading this node changes the DaemonSet image", "reason", reason)
		return
	}
	if !kubeboot.TakeAlign() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), alignKubeAgentTimeout)
	defer cancel()
	// Returns only when the binaries are the same or the replacement failed.
	if err := agentconn.AlignWithServer(ctx, serverURL, token); err != nil {
		log.Warn("could not take the server's agent binary; running the image's", "err", err)
	}
}

// prepareKubeRun does what the Kubernetes driver's run must do before
// anything else, enrollment included: every vendor's CLIs run on the host
// (the manifest grants no vendor mounts), and only one gpuworks-agent
// process runs this node's workload at a time.
func prepareKubeRun(stateDir string) error {
	// A Kubernetes DaemonSet pod gets no vendor mounts: every vendor's CLIs
	// and driver libraries are the host's, found by walking the host's own
	// file system (internal/agent/gpu's hostexec.go).
	gpu.RunCLIsOnHost()
	// Only one gpuworks agent may run this node's workload at a time; see
	// acquireKubeAgentLock. The lock is released only by this process
	// exiting or an in-place re-exec (close-on-exec), never by Go's own
	// garbage collector finalizing an unreachable local.
	lock, err := acquireKubeAgentLock(stateDir)
	if err != nil {
		writeTerminationLog(err.Error())
		return err
	}
	goruntime.SetFinalizer(lock, nil)
	return nil
}

// kubeAcceleratorCensus is the host's accelerator vendors, independent of
// any CLI: *gpu.VendorCensus reads the PCI bus for real; a test, or the
// GPUWORKS_FAKE_GPU_VENDORS process-test harness (cfg.AcceleratorHost),
// supplies another.
type kubeAcceleratorCensus interface {
	Vendors() []proto.AcceleratorPresence
}

// kubeCensusFor is the real PCI bus census, unless host (a process test's
// GPUWORKS_FAKE_GPU_VENDORS substitute, cfg.AcceleratorHost) overrides it.
// It reads the node's bus through the host's root: the agent pod's own view
// of /sys counts as a container's, which the census leaves out (it would
// be another host's cards for an agent given part of a machine), while the
// agent pod runs with hostPID, so PID 1's root is the node's.
func kubeCensusFor(host kubeAcceleratorCensus) kubeAcceleratorCensus {
	if host != nil {
		return host
	}
	return &gpu.VendorCensus{HostRoot: kubeHostRoot}
}

// kubeHostRoot is the node's root as the agent pod sees it (hostPID).
var kubeHostRoot = "/proc/1/root"

// kubeStandByPoll is how often a node with no supported accelerator yet
// re-checks the PCI bus, before its first enrollment.
var kubeStandByPoll = 60 * time.Second

// kubeReexec replaces this process the way accelerator_choice.go's
// reexecAgent does; a test overrides it so a successful stand-by does not
// actually exec.
var kubeReexec = reexecKubeAgent

// kubeSupportedAccelerator is a census's first vendor this build can run on
// Kubernetes (accelerator.RunsOnKube), or "", false when census is nil
// or shows none. It does not touch any CLI: a card whose CLI does not
// answer is still this vendor's, and must still enroll and report the
// failure, not stand by forever.
func kubeSupportedAccelerator(census kubeAcceleratorCensus) (vendor string, ok bool) {
	if census == nil {
		return "", false
	}
	for _, v := range census.Vendors() {
		if accelerator.RunsOnKube(v.Vendor) {
			return v.Vendor, true
		}
	}
	return "", false
}

// kubeStandBy decides, before a node's first enrollment, whether to stand by
// instead of enrolling. A node that already has an identity of its own
// entry on disk behaves
// as it always did — (false, nil), whatever the bus shows right now: a
// card going missing, or a vendor this build no longer supports, must not
// revert an already-enrolled node to stand-by. A fresh node with no
// accelerator this build can run on Kubernetes on the bus yet logs once and
// waits, re-checking every kubeStandByPoll, until one appears (then
// kubeReexec restarts the process to enroll clean, as a self-upgrade or an
// accelerator-preference switch does) or ctx ends.
func kubeStandBy(ctx context.Context, log *slog.Logger, stateDir string, census kubeAcceleratorCensus) (stoodBy bool, err error) {
	if kube.ReadOwnIdentity(stateDir, os.Getenv("GPUWORKS_KUBE_ENROLL_TOKEN")).NodeToken != "" {
		return false, nil
	}
	if vendor, ok := kubeSupportedAccelerator(census); ok {
		log.Info("an accelerator this build can run on Kubernetes is already on the bus; enrolling", "vendor", vendor)
		return false, nil
	}
	log.Warn("standing by: no accelerator this build can run on Kubernetes is on the bus yet; staying up and re-checking the bus until one appears",
		"every", kubeStandByPoll.String())
	t := time.NewTicker(kubeStandByPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return true, nil // the pod is going away: a clean exit, not a failure
		case <-t.C:
			if vendor, ok := kubeSupportedAccelerator(census); ok {
				log.Info("a supported accelerator appeared; restarting to enroll", "vendor", vendor)
				if err := kubeReexec(); err != nil {
					log.Error("restarting to enroll a newly-appeared accelerator failed", "err", err)
					return true, err
				}
				return true, nil
			}
		}
	}
}

// reexecKubeAgent replaces this process with a fresh start of the same
// binary and arguments, as accelerator_choice.go's reexecAgent does: a node
// that stood by restarts clean once a supported accelerator appears,
// instead of falling into enrollment with whatever an idle wait left
// behind.
func reexecKubeAgent() error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	for strings.HasSuffix(bin, ".old") {
		bin = strings.TrimSuffix(bin, ".old")
	}
	return syscall.Exec(bin, os.Args, os.Environ())
}

// kubeAgentLockFile is the kube agent's exclusive lock, under the state
// directory (a hostPath, so any other gpuworks-agent process sharing this
// node's state directory sees it too).
const kubeAgentLockFile = "agent.lock"

// terminationLogPath is where Kubernetes reads a failed container's last
// words (kubectl describe shows it); a test overrides it so
// writeTerminationLog does not touch the real special file.
var terminationLogPath = "/dev/termination-log"

// writeTerminationLog records why run is about to exit non-zero. A host
// without that path (anywhere but inside a Kubernetes container) simply
// does not get one; the error is not this process's problem to report.
func writeTerminationLog(reason string) {
	_ = os.WriteFile(terminationLogPath, []byte(reason+"\n"), 0o644)
}

// acquireKubeAgentLock takes an exclusive, non-blocking lock on this node's
// state directory for the life of this process: two gpuworks-agent
// processes must never run this node's DaemonSet pod's workload at once.
// Go opens the file close-on-exec, so an in-place re-exec — a self-upgrade,
// or kubeReexec's/accelerator_choice.go's own restart, both syscall.Exec —
// releases it on the old process image automatically, and the new one
// retakes it itself on its own first line of run. prepareKubeRun disables
// the returned file's finalizer (so Go's GC never auto-closes it once its
// local variable is unreachable) and otherwise never touches it again;
// only an explicit Close (as a test does, to prove release) or the process
// itself exiting lets another holder in.
func acquireKubeAgentLock(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, kubeAgentLockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another gpuworks agent already runs on this node with state directory %s", stateDir)
	}
	return f, nil
}
