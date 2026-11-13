// Package controller runs the server-side reconcile loops. Each resource kind
// gets its own reconcile function (no god file). The loops cover deployments →
// instances (schedule + desired-state push) and model-file readiness; GPU
// selection is topology-aware rather than a plain count.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/placewait"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/servingdial"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// HubPusher is the controller's view of the agent hub.
type HubPusher interface {
	Online(uuid string) bool
	PushToNode(uuid string, su proto.StateUpdate) error
	SyncNode(uuid string) error
	CopyModelPath(ctx context.Context, req proto.ModelSyncCopyRequest) (proto.ModelSyncCopyResult, error)
	StopModelSync(ctx context.Context, nodeUUID, key, transferID string) error
}

// Controller reconciles deployments and instances.
type Controller struct {
	cacheCopies   cacheCopyClaims
	downloadUsers downloadDependencyCache
	// placements and evictions are what the deployment view shows of
	// preparation the controller itself drives (preparation_view.go).
	placements preparationPlacements
	evictions  *prepobs.Phases[store.FileRef]
	store      *store.Store
	networks   Networks
	hub        HubPusher
	log        *slog.Logger
	// servingDial reaches serving forwarders (checkServingPath); nil dials
	// with the default dialer. Tests replace it.
	servingDial servingdial.DialFunc
	q           *workq
	cacheQ      *workq
	// waits are the deployments whose replica waits for others to give up
	// cards (placement_wait.go), read by the residency arbiter.
	waits        *placewait.Registry
	retryBackoff time.Duration
	offlineGrace time.Duration
	// watchingSince is when this server began watching its nodes (Run). A
	// node last seen before then counts as seen then: how long it was gone
	// while the server itself was down is unknown, and its agent has not had
	// the chance to reconnect yet.
	watchingSince time.Time
	maxAttempts   int
	// enginePorts is the host-port range engine instances are allocated
	// from. The hub reads it through EnginePorts to tell agents which
	// listeners to report, so it is atomic.
	enginePorts atomic.Pointer[proto.PortRange]
	// startTimeout is the harness override of the instance health budget
	// (SetStartTimeout); zero leaves it to the deployment and the policy.
	startTimeout int
	syncMu       sync.Mutex
	modelSyncs   map[string]*modelSyncRun
	// drainProbe and draining time the instances being drained (drain.go).
	drainProbe DrainProbe
	drainMu    sync.Mutex
	draining   map[uint]map[string]time.Time // by deployment, then instance
	// sleepPushed is the sleep intent last pushed per instance, by
	// deployment (sleep.go).
	sleepMu     sync.Mutex
	sleepPushed map[uint]map[string]sleepOrder
	// now is the controller's clock; tests replace it.
	now func() time.Time
	// poolRefused remembers, by deployment, the candidates whose trial plan
	// refused a replica recently (unit_pool.go).
	// nodeStatusDirty is set by routine node status reports and cleared by
	// the settle tick that acts on them (settleNodeStatus).
	nodeStatusDirty atomic.Bool
	poolMu          sync.Mutex
	poolRefused     map[uint]map[uint]time.Time
	// dgpuPending holds the deployments whose instances' memory limits were
	// rewritten in place and are not yet handed to their nodes
	// (dgpu_limit.go).
	dgpuMu      sync.Mutex
	dgpuPending map[uint]bool
	// poolWaiting is when each deployment's last pass found no node; see
	// poolSettleBackoff.
	poolWaiting          map[uint]time.Time
	inferenceNodeHandler provider.InferenceNodeLifecycleHandler
	// observation coalesces catalog changes into in-place refreshes of the
	// instances' observation (observation.go).
	observation *observationRefresh
	// peerCool and imageWaits are the automatic image copy's memory
	// (image_acquisition.go).
	peerCool   pairCooldown
	imageWaits imageWaits
	// failover is node replacement's memory (node_failover.go).
	failover nodeFailover
}

func (c *Controller) SetInferenceNodeLifecycleHandler(handler provider.InferenceNodeLifecycleHandler) {
	c.inferenceNodeHandler = handler
}

// defaultEnginePorts is the production engine host-port range.
var defaultEnginePorts = proto.PortRange{Start: 30000, End: 32767}

// SetEnginePorts gives hermetic harnesses disjoint engine-port ranges when
// several `go test` package processes run concurrently. Production keeps the
// historical 30000–32767 range.
func (c *Controller) SetEnginePorts(start, end int) {
	if start < 1 || end < start || end > 65535 {
		return
	}
	c.enginePorts.Store(&proto.PortRange{Start: start, End: end})
}

// EnginePorts is the host-port range engine instances are allocated from.
func (c *Controller) EnginePorts() proto.PortRange { return *c.enginePorts.Load() }

type modelSyncRun struct {
	transferID string
	cancel     context.CancelFunc
	// automatic marks a copy whose source the controller chose itself. Its
	// failure is no one's recorded choice, so the model falls back to the
	// public download rather than waiting on an operator's retry.
	automatic bool
	// canceled is set by an operator's cancel before it cancels the copy: the
	// copy's own error is then that cancel, not a failure to fall back from,
	// and CancelModelSync records the outcome.
	canceled atomic.Bool
}

const modelSyncConfirmationTimeout = 2 * time.Minute

// quotaChangeRemovalPending is what an instance retired for a partition
// change says while it waits for its node to confirm the teardown.
const quotaChangeRemovalPending = "quota change removal pending"

// New builds a Controller.
func New(st *store.Store, hub HubPusher, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	c := &Controller{
		store: st, networks: network.New(st, network.StoreCatalog{Store: st}), hub: hub, log: log, q: newWorkq(300 * time.Millisecond), cacheQ: newWorkq(300 * time.Millisecond),
		retryBackoff: 15 * time.Second, offlineGrace: 2 * time.Minute,
		maxAttempts: 3, modelSyncs: map[string]*modelSyncRun{}, sleepPushed: map[uint]map[string]sleepOrder{},
		now: time.Now, observation: newObservationRefresh(), evictions: prepobs.NewPhases[store.FileRef](),
		waits: placewait.New(),
	}
	c.SetEnginePorts(defaultEnginePorts.Start, defaultEnginePorts.End)
	return c
}

// SetRetryPolicy overrides the failed-instance retry backoff and attempt cap
// (used by tests to exercise the bound quickly).
func (c *Controller) SetRetryPolicy(backoff time.Duration, maxAttempts int) {
	c.retryBackoff = backoff
	c.maxAttempts = maxAttempts
}

// SetOfflineGrace overrides how long a distributed rank may remain
// disconnected before its whole collective is reset. It is primarily used by
// hermetic tests; production allows normal agent reconnect backoff to finish.
func (c *Controller) SetOfflineGrace(grace time.Duration) {
	if grace >= 0 {
		c.offlineGrace = grace
	}
}

// SetStartTimeout overrides the instance startup health budget for every
// deployment, ahead of a deployment's own value and the cluster default. It
// exists for hermetic harnesses that exercise timeout/failure paths without
// waiting for the production budget.
func (c *Controller) SetStartTimeout(seconds int) {
	if seconds > 0 {
		c.startTimeout = seconds
	}
}

// Run subscribes to change events and processes the reconcile queue until ctx
// is cancelled.
func (c *Controller) Run(ctx context.Context) {
	c.watchingSince = c.now()
	// A server App may be stopped and started again in-process (and embedders
	// commonly do this during graceful restarts). The previous worker closes its
	// queue on shutdown; replace that closed queue before priming persisted
	// deployments so updates after restart are not silently dropped.
	if c.q.isClosed() {
		c.q = newWorkq(300 * time.Millisecond)
	}
	if c.cacheQ.isClosed() {
		c.cacheQ = newWorkq(300 * time.Millisecond)
	}
	if err := c.store.RecoverCacheOperations(); err != nil {
		c.log.Warn("recover cache operations", "err", err)
	}
	cacheDone := make(chan struct{})
	go func() { defer close(cacheDone); c.cacheWorker(ctx) }()
	defer func() { c.cacheQ.close(); <-cacheDone }()
	c.cacheQ.add("waiting")
	if storages, err := c.store.ListStorages(); err == nil {
		for _, storage := range storages {
			if storage.Type == proto.StorageNFSCache {
				c.cacheQ.add("refresh-storage/" + strconv.FormatUint(uint64(storage.ID), 10))
			}
		}
	}
	sub := c.store.Bus.Subscribe(256, "storage", store.StorageCacheTopic, "deployment", "instance", "modelfile", "node", "image", "network_test", store.InferenceTemplateTopic, store.PullSettingsTopic)
	defer sub.Close()
	// Tests started by a previous process died with it.
	if err := c.store.InterruptRunningNetworkTests(); err != nil {
		c.log.Warn("could not close interrupted network tests", "err", err)
	}

	// Prime: reconcile all existing deployments on startup.
	if !c.primeCacheCopyClaims(ctx) {
		return
	}

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		c.worker(ctx)
	}()
	// The startup pass re-resolves every instance's observation once: a new
	// server version may have changed a built-in template, or how a mapping
	// resolves.
	refreshCtx, stopRefresh := context.WithCancel(ctx)
	refreshDone := make(chan struct{})
	c.observation.request(store.InferenceTemplatesAll)
	go func() {
		defer close(refreshDone)
		c.runObservationRefresh(refreshCtx)
	}()
	defer func() {
		stopRefresh()
		<-refreshDone
	}()

	settle := time.NewTicker(nodeStatusSettle)
	defer settle.Stop()
	for {
		select {
		case <-ctx.Done():
			c.cancelAllModelSyncs()
			c.q.close()
			// worker only returns once q.close() makes q.next() report !ok, so
			// it must be waited for after closing the queue, not before: Run
			// must not return (and the caller must not treat the controller as
			// stopped) while worker is still in flight.
			<-workerDone
			return
		case ev := <-sub.C():
			c.onEvent(ev)
		case <-settle.C:
			c.settleNodeStatus()
		}
	}
}

func (c *Controller) cancelAllModelSyncs() {
	c.syncMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.modelSyncs))
	for _, run := range c.modelSyncs {
		cancels = append(cancels, run.cancel)
	}
	c.syncMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (c *Controller) worker(ctx context.Context) {
	for {
		key, ok := c.q.next()
		if !ok {
			return
		}
		id, kind := parseKey(key)
		switch kind {
		case "dep":
			if err := c.reconcileDeploymentGuarded(ctx, id); err != nil {
				c.log.Warn("reconcile deployment failed", "id", id, "err", err)
			}
		}
	}
}

func depKey(id uint) string { return "dep/" + strconv.FormatUint(uint64(id), 10) }

// allInstancesTerminal reports whether every instance has reached a state the
// agent will not leave on its own, i.e. its teardown has been reported. An empty
// slice is terminal: with nothing to tear down there is nothing to wait for.
func allInstancesTerminal(insts []store.Instance) bool {
	for _, inst := range insts {
		switch inst.State {
		case proto.InstStopped:
		default:
			return false
		}
	}
	return true
}

func parseKey(k string) (uint, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			v, _ := strconv.ParseUint(k[i+1:], 10, 64)
			return uint(v), k[:i]
		}
	}
	return 0, ""
}

// ---- deployment reconcile ----

func (c *Controller) reconcileDeployment(ctx context.Context, id uint) error {
	dep, err := c.store.GetDeployment(id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := c.withdrawDeploymentDownloads(ctx, id); err != nil {
			return err
		}
		if err := c.withdrawCacheCopies(ctx, id); err != nil {
			return err
		}
		c.forgetDeployment(id) // deleted
		return nil
	}
	if _, err := c.store.NodeByID(dep.NodeID); err != nil {
		return fmt.Errorf("deployment %d: node %d not found", id, dep.NodeID)
	}

	insts, _ := c.store.InstancesForDeployment(id)
	// A deployment's partition - whole cards or a slice, its backend, its SM
	// share - is part of the instance's desired spec and cannot change
	// in place: dGPU cache files and GPU claims are keyed by instance UUID.
	// Tear down the old generation before scheduling a new one so the cache
	// path and runtime limits cannot be accidentally reused. A slice's memory
	// limit is the exception: it is rewritten on running instances in place
	// (store.SetDeploymentDGPUQuota, pushDGPULimits), and an instance the
	// rewrite left out takes the deployment's limit when it is next replaced.
	if dep.NNodes <= 1 && dep.State != store.DeployStopped && dep.State != store.DeployDeleting {
		var replace []string
		for _, inst := range insts {
			if inst.State == proto.InstFailed || inst.State == proto.InstPendingRemoval || inst.DesiredStateJSON == "" {
				continue
			}
			var spec proto.InstanceDesiredState
			if json.Unmarshal([]byte(inst.DesiredStateJSON), &spec) != nil {
				continue
			}
			if (spec.VRAMLimitBytes > 0) != (dep.VRAMLimitBytes > 0) || spec.SMLimitPct != dep.SMLimitPct || spec.PartitionBackend != dep.PartitionBackend {
				replace = append(replace, inst.UUID)
			}
		}
		if len(replace) > 0 {
			var waiting []string
			for _, inst := range insts {
				if slices.Contains(replace, inst.UUID) && inst.State != proto.InstStopped {
					waiting = append(waiting, inst.UUID)
				}
			}
			if len(waiting) == 0 {
				for _, inst := range insts {
					if !slices.Contains(replace, inst.UUID) {
						continue
					}
					if _, err := c.finalizeInstanceRemoval(ctx, inst, true); err != nil {
						return err
					}
				}
				c.q.add(depKey(id))
				return nil
			}
			for _, inst := range insts {
				// Each retry while the old generation exits finds it already
				// marked; writing it again only repeats the same timeline entry.
				if !slices.Contains(waiting, inst.UUID) || (inst.State == proto.InstRemoving && inst.Message == quotaChangeRemovalPending) {
					continue
				}
				if err := c.store.SetInstanceState(inst.UUID, proto.InstRemoving, quotaChangeRemovalPending); err != nil {
					return err
				}
			}
			if err := c.pushRemoveByNode(insts, waiting); err != nil {
				c.q.addAfter(depKey(id), c.retryBackoff)
				return err
			}
			c.q.addAfter(depKey(id), c.retryBackoff)
			return nil
		}
	}

	// Stopped/deleting deployments: tear down all their instances. A deleting
	// deployment is removed only after the agent confirms the teardown.
	if dep.State == store.DeployStopped || dep.State == store.DeployDeleting {
		if err := c.withdrawDeploymentDownloads(ctx, dep.ID); err != nil {
			return err
		}
		if err := c.withdrawCacheCopies(ctx, dep.ID); err != nil {
			return err
		}
		c.forgetDeployment(dep.ID)
		var pending []string
		for _, in := range insts {
			if in.State != proto.InstStopped {
				pending = append(pending, in.UUID)
				if in.State != proto.InstRemoving {
					if err := c.store.SetInstanceState(in.UUID, proto.InstRemoving, "deployment removal pending"); err != nil {
						return err
					}
				}
			}
		}
		if len(pending) > 0 {
			if err := c.pushRemoveByNode(insts, pending); err != nil {
				message := "remove pending: " + err.Error()
				for _, inst := range insts {
					if inst.State != proto.InstStopped && (inst.State != proto.InstRemoving || inst.Message != message) {
						_ = c.store.SetInstanceState(inst.UUID, proto.InstRemoving, message)
					}
				}
				c.q.addAfter(depKey(id), c.retryBackoff)
				return err
			}
		}
		if !allInstancesTerminal(insts) {
			c.q.addAfter(depKey(id), c.retryBackoff)
			return nil
		}
		for _, inst := range insts {
			done, err := c.finalizeInstanceRemoval(ctx, inst, dep.State == store.DeployStopped)
			if err != nil {
				c.q.addAfter(depKey(id), c.retryBackoff)
				return err
			}
			if !done {
				c.q.addAfter(depKey(id), c.retryBackoff)
				return nil
			}
		}
		if dep.State == store.DeployDeleting {
			// Wait for the agent to confirm, instead of treating an accepted
			// push as proof the container is gone. It is not: teardown is
			// asynchronous on the agent, and on udocker the ONLY code that can
			// actually delete the rootfs is Remove()'s os.RemoveAll - `udocker
			// rm` fails there by construction (it chmods a directory the
			// runtime user does not own). Dropping the deployment row first
			// discards the last handle on a container that may still be
			// running with its GPU allocated.
			//
			// No timeout here on purpose. A node that never confirms leaves the
			// deployment in `deleting`, which is the honest state; force-delete
			// is the explicit escape hatch for a node that is gone.
			if err := c.store.DeleteDeployment(dep.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			c.forgetDeployment(dep.ID)
		}
		return nil
	}

	// Sync desired sleep level to existing running instances (push updated specs).
	c.syncSleep(dep, insts)
	c.pushDGPULimits(dep, insts)

	pd, err := planning.PDOf(dep.Placement)
	if err != nil {
		c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": err.Error()}})
		return nil //nolint:nilerr // The placement error is persisted as an unschedulable reason instead of retried as an internal failure.
	}
	// Every shape - replicas, a multi-node group, PD roles - is a set of
	// units reconciled the same way (ADR 0029).
	return c.reconcileUnits(ctx, dep, insts, pd)
}
