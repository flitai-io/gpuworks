package engine

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// containerName is the deterministic docker name for an instance (enables
// adoption after an agent restart and orphan cleanup).
func containerName(uuid string) string {
	short := uuid
	if len(short) > 12 {
		short = short[:12]
	}
	return "gpuworks-" + short
}

// SetDesiredFull replaces the entire desired set (state.sync). Instances no
// longer present are torn down.
func (e *Engine) SetDesiredFull(ctx context.Context, specs []proto.InstanceDesiredState) {
	e.mu.Lock()
	old := e.desired
	e.desired = map[string]proto.InstanceDesiredState{}
	e.desiredKnown = true
	for _, s := range specs {
		if s.DesiredState == "stopped" {
			previous, known := old[s.UUID]
			if !known {
				previous = s
			}
			e.requireCleanupLocked(previous)
			e.cancelEnsureLocked(s.UUID)
		}
		e.desired[s.UUID] = s
	}
	// Find removed.
	var removed []proto.InstanceDesiredState
	for uuid, spec := range old {
		if _, ok := e.desired[uuid]; !ok {
			e.requireCleanupLocked(spec)
			if !e.inFlight[uuid] {
				e.inFlight[uuid] = true // serialize teardown with a same-UUID re-add
				removed = append(removed, spec)
			} else {
				e.cancelEnsureLocked(uuid)
			}
			e.invalidateRuntimeEpochLocked(uuid)
			delete(e.pending, uuid)
		}
	}
	// A previous teardown may have failed after its desired entry was removed.
	// A later full sync retries that cleanup even though the UUID is absent
	// from both the old and new desired maps.
	for uuid, spec := range e.cleanupRequired {
		if _, desired := e.desired[uuid]; !desired && !e.inFlight[uuid] {
			e.inFlight[uuid] = true
			removed = append(removed, spec)
		}
	}
	e.mu.Unlock()

	for _, spec := range removed {
		e.finishEnsure(spec)
	}
	e.reconcileAll(ctx)
}

// ApplyUpdate merges an incremental update (state.update).
func (e *Engine) ApplyUpdate(ctx context.Context, specs []proto.InstanceDesiredState, removed []string) {
	e.mu.Lock()
	for _, s := range specs {
		if s.DesiredState == "stopped" {
			previous, known := e.desired[s.UUID]
			if !known {
				previous = s
			}
			e.requireCleanupLocked(previous)
			e.cancelEnsureLocked(s.UUID)
		}
		e.desired[s.UUID] = s
	}
	// The specs are kept, not just the uuids: releasing a deployment's managed
	// directories needs to know what the instance was. An uuid this agent never
	// knew is still torn down, in case a container outlived the desired state.
	removedSpecs := make([]proto.InstanceDesiredState, 0, len(removed))
	for _, uuid := range removed {
		spec, known := e.desired[uuid]
		if !known {
			spec = proto.InstanceDesiredState{UUID: uuid}
		}
		e.requireCleanupLocked(spec)
		if !e.inFlight[uuid] {
			e.inFlight[uuid] = true // serialize teardown with a same-UUID re-add
			removedSpecs = append(removedSpecs, spec)
		} else {
			e.cancelEnsureLocked(uuid)
		}
		delete(e.desired, uuid)
		e.invalidateRuntimeEpochLocked(uuid)
		delete(e.pending, uuid)
	}
	e.mu.Unlock()

	for _, spec := range removedSpecs {
		e.finishEnsure(spec)
	}
	for _, s := range specs {
		e.ensure(ctx, s)
	}
}

// reconcileAll ensures every desired instance.
func (e *Engine) reconcileAll(ctx context.Context) {
	e.mu.Lock()
	specs := make([]proto.InstanceDesiredState, 0, len(e.desired))
	for _, s := range e.desired {
		specs = append(specs, s)
	}
	e.mu.Unlock()
	for _, s := range specs {
		e.ensure(ctx, s)
	}
}

// ensure brings one instance to its desired state (async; guarded per-uuid).
type pendingEnsure struct {
	ctx  context.Context
	spec proto.InstanceDesiredState
}

func (e *Engine) requireCleanupLocked(spec proto.InstanceDesiredState) {
	if _, exists := e.cleanupRequired[spec.UUID]; !exists {
		e.cleanupRequired[spec.UUID] = spec
	}
}

func (e *Engine) cancelEnsureLocked(uuid string) {
	if cancel := e.inFlightCancel[uuid]; cancel != nil {
		cancel()
	}
}

func (e *Engine) ensure(ctx context.Context, spec proto.InstanceDesiredState) {
	e.mu.Lock()
	current, desired := e.desired[spec.UUID]
	if !desired {
		e.mu.Unlock()
		return
	}
	// The desired map is authoritative. A queued call may have waited behind a
	// prior reconcile while a later sync replaced its spec.
	spec = current
	if e.inFlight[spec.UUID] {
		// A state sync on a replacement connection must not disappear behind an
		// inspect started for the old connection. Keep only the newest desired
		// request and run it as soon as the serialized attempt finishes.
		e.pending[spec.UUID] = pendingEnsure{ctx: ctx, spec: spec}
		e.mu.Unlock()
		return
	}
	e.inFlight[spec.UUID] = true
	e.cancelHealthProbeLocked(spec.UUID)
	e.nextRuntimeEpoch++
	epoch := e.nextRuntimeEpoch
	e.runtimeEpoch[spec.UUID] = epoch
	attemptCtx, cancel := context.WithCancel(ctx)
	e.inFlightCancel[spec.UUID] = cancel
	e.mu.Unlock()
	e.runEnsure(attemptCtx, spec, epoch)
}

func (e *Engine) runEnsure(ctx context.Context, spec proto.InstanceDesiredState, epoch uint64) {
	go func() {
		defer e.finishEnsure(spec)
		e.mu.Lock()
		_, desired := e.desired[spec.UUID]
		currentEpoch := e.runtimeEpoch[spec.UUID] == epoch
		_, cleanupRequired := e.cleanupRequired[spec.UUID]
		e.mu.Unlock()
		if !desired || !currentEpoch || ctx.Err() != nil {
			return
		}
		if cleanupRequired {
			return // finishEnsure reclaims the prior generation before any start.
		}
		if spec.DesiredState == "stopped" {
			e.mu.Lock()
			e.requireCleanupLocked(spec)
			e.mu.Unlock()
			return
		}
		e.startOrAdoptEpoch(ctx, spec, epoch)
		e.reconcilePartitionLimit(spec)
		// Headless distributed workers have no HTTP sleep endpoint and must
		// remain awake for the lifetime of their leader.
		if !spec.Headless {
			e.reconcileSleep(ctx, spec)
		}
	}()
}

func (e *Engine) finishEnsure(spec proto.InstanceDesiredState) {
	uuid := spec.UUID
	e.mu.Lock()
	e.cancelEnsureLocked(uuid)
	delete(e.inFlightCancel, uuid)
	cleanupSpec, cleanup := e.cleanupRequired[uuid]
	if _, desired := e.desired[uuid]; !desired && !cleanup {
		cleanupSpec, cleanup = spec, true
		e.requireCleanupLocked(spec)
	}
	e.mu.Unlock()

	if cleanup {
		// The old attempt has exited. A removal followed by a same-UUID add
		// still has to reclaim its container and partition before replacement.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cleanupErr := e.reclaimInstance(cleanupCtx, uuid)
		cancel()
		if cleanupErr != nil {
			// A later sync/update can retry cleanup. Never hand the queued
			// replacement to a runtime that may still hold the old quota.
			// Release the attempt before reporting: a sync sent in reaction to
			// the report must find the teardown retryable, not still in flight.
			e.mu.Lock()
			delete(e.inFlight, uuid)
			delete(e.pending, uuid)
			e.mu.Unlock()
			e.log.Error("instance teardown pending; resources may still be held", "uuid", uuid, "err", cleanupErr)
			e.report(proto.InstanceStatus{UUID: uuid, State: proto.InstRemoving, Message: "teardown pending: " + cleanupErr.Error()})
			return
		}
		e.releaseKVDiskDir(cleanupSpec)
		e.mu.Lock()
		delete(e.cleanupRequired, uuid)
		current, desired := e.desired[uuid]
		if !desired || current.DesiredState == "stopped" {
			e.mu.Unlock()
			e.report(proto.InstanceStatus{UUID: uuid, State: proto.InstStopped})
			e.log.Info("instance torn down", "uuid", uuid)
			e.mu.Lock()
		}
	} else {
		e.mu.Lock()
	}
	pending, queued := e.pending[uuid]
	delete(e.pending, uuid)
	current, desired := e.desired[uuid]
	if !desired || (cleanup && current.DesiredState == "stopped") || (!cleanup && !queued) {
		delete(e.inFlight, uuid)
		e.mu.Unlock()
		return
	}
	// A queued update or a re-add during cleanup starts only after the old
	// generation is absent. Keep the serialized ownership across the handoff.
	if !queued {
		pending.ctx = context.Background()
	}
	e.nextRuntimeEpoch++
	epoch := e.nextRuntimeEpoch
	e.runtimeEpoch[uuid] = epoch
	attemptCtx, cancel := context.WithCancel(pending.ctx)
	e.inFlightCancel[uuid] = cancel
	e.mu.Unlock()
	e.runEnsure(attemptCtx, current, epoch)
}

// startOrAdopt ensures a running, healthy container for the spec.
// startOrAdopt is retained as the synchronous test seam. Reconciliation uses
// startOrAdoptEpoch with the epoch allocated by ensure.
func (e *Engine) startOrAdopt(ctx context.Context, spec proto.InstanceDesiredState) {
	e.mu.Lock()
	e.nextRuntimeEpoch++
	epoch := e.nextRuntimeEpoch
	e.runtimeEpoch[spec.UUID] = epoch
	if _, ok := e.desired[spec.UUID]; !ok {
		e.desired[spec.UUID] = spec
	}
	e.mu.Unlock()
	e.startOrAdoptEpoch(ctx, spec, epoch)
}

func (e *Engine) startOrAdoptEpoch(ctx context.Context, spec proto.InstanceDesiredState, epoch uint64) {
	name := containerName(spec.UUID)
	// One driver for the whole start/adopt attempt: a runtime switch mid-attempt
	// must not hand earlier steps and later steps two different runtimes.
	driver := e.Driver()
	// The cards this instance is bound to may carry different device indexes
	// than when it was placed; everything below works in current indexes.
	if err := e.resolveBoundCards(ctx, &spec); err != nil {
		e.reportState(spec, proto.InstFailed, "", "", err.Error())
		return
	}
	ws := toWorkloadSpec(spec, name)
	if err := applyDistributedNetworkInterface(&ws, spec, e.proberVendor(), e.networkInterfaceName); err != nil {
		e.reportState(spec, proto.InstFailed, "", "", "distributed network interface selection failed: "+err.Error())
		return
	}

	// An inspection error does not prove the container is absent. Keep the
	// claim and retry locally; InstRemoving would make the controller retire a
	// healthy desired-running instance on a transient runtime CLI failure.
	var st runtime.Status
	inspectionFailures := 0
	for {
		if ctx.Err() != nil {
			return
		}
		var err error
		st, err = driver.Status(ctx, name)
		if err == nil && st.Exists && !st.Running {
			err = driver.Remove(ctx, name)
			if err == nil {
				st.Exists = false
			}
		}
		if err == nil {
			break
		}
		inspectionFailures++
		if inspectionFailures == 1 {
			e.log.Warn("container inspection or stale-container cleanup pending", "instance", spec.UUID, "container", name, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	if st.Exists {
		if st.Running {
			// A running deterministic container is being adopted after an agent
			// restart. It has already served traffic, so repeating the cold
			// sleep/wake prewarm would create an avoidable 503 window.
			e.mu.Lock()
			e.prewarmed[spec.UUID] = true
			e.mu.Unlock()
			if spec.Sleepable {
				// Keep an exact level already applied by this agent. The engine
				// probe only reports a boolean, so overwriting a known L2 value
				// with 1 would make the next wake use the unsafe one-step path.
				if err := e.adoptSleepLevel(ctx, spec); err != nil {
					e.reportState(spec, proto.InstOptimizing, name, "", "sleep state probe failed: "+err.Error())
					return
				}
			}
			e.collectAndPublishRuntimeObservation(ctx, spec, name, "", epoch)
			e.probeAndWatch(ctx, spec, name, epoch)
			return
		}
	}

	// Admission governs creation. A running container is adopted above using
	// its existing runtime contract, even if a newer policy rejects new launches.
	if err := e.validateTargetDesiredForDriver(spec, driver.Name()); err != nil {
		e.reportInjectionFailure(spec, err)
		return
	}
	// A model path that is not there must fail the launch, not reach the
	// runtime: docker creates a missing bind-mount source as an empty
	// directory, so an unmounted NFS would otherwise start the engine on an
	// empty /model and leave a stray directory under the mount point.
	if err := checkModelPaths(spec); err != nil {
		e.reportState(spec, proto.InstFailed, "", "", err.Error())
		return
	}
	// A port taken since the server last saw this node's listeners fails
	// now, so the server picks another, instead of after the engine loaded
	// its weights and could not bind.
	if err := e.checkPorts(spec); err != nil {
		e.reportState(spec, proto.InstFailed, "", "", err.Error())
		return
	}

	// Pull the image unless it is already present locally (fast re-deploy; also
	// supports locally-built images that are not in any registry). The launch
	// reference is spec.Image except when a digest-pinned image was satisfied by
	// a mirror that pins the same digest.
	launchImage, ok := e.prepareImage(ctx, driver, spec, name)
	if !ok {
		return
	}
	ws.Image = launchImage

	if err := e.applyRuntimeAssets(&ws, spec); err != nil {
		e.reportState(spec, proto.InstFailed, "", "", "runtime asset preparation failed: "+err.Error())
		return
	}
	if len(spec.RuntimeAssets) > 0 {
		e.report(proto.InstanceStatus{
			UUID: spec.UUID, State: proto.InstStarting, Phase: proto.PhaseAssetPrepare, Progress: 1,
			Message: "runtime assets are ready", RuntimeAssetDigests: runtimeAssetDigests(spec.RuntimeAssets),
		})
	}

	if err := e.applyPreStart(&ws, spec); err != nil {
		e.reportState(spec, proto.InstFailed, "", "", "pre-start preparation failed: "+err.Error())
		return
	}

	if err := e.applyKVDiskMount(&ws, spec); err != nil {
		e.reportState(spec, proto.InstFailed, "", "", "KV disk tier preparation failed: "+err.Error())
		return
	}

	if err := e.applyDeviceInjectionForLaunch(ctx, &ws, spec.GPUIndexes, driver.Name(), spec.NNodes); err != nil {
		e.reportInjectionFailure(spec, err)
		return
	}
	if ok, reason := e.applyPartition(ctx, &ws, spec); !ok {
		if err := e.cleanupPartitionState(spec.UUID); err != nil {
			e.report(proto.InstanceStatus{UUID: spec.UUID, State: proto.InstRemoving, Message: reason + "; partition cleanup pending: " + err.Error()})
		} else {
			e.reportState(spec, proto.InstFailed, "", "", reason)
		}
		return
	}
	e.mu.Lock()
	current, desired := e.desired[spec.UUID]
	tracked := e.inFlight[spec.UUID]
	currentEpoch := e.runtimeEpoch[spec.UUID] == epoch
	e.mu.Unlock()
	if tracked && (!desired || current.DesiredState == "stopped" || !currentEpoch) {
		return // The in-flight goroutine will acknowledge teardown after it exits.
	}
	e.waitForCardChecks(ctx, spec.UUID, spec.GPUIdents)
	for _, note := range ws.PreparationNotes {
		e.reportStateProgress(spec, proto.InstStarting, name, "", note, proto.PhaseContainerPrepare, 0, 0, 0, 0)
	}
	e.reportStateProgress(spec, proto.InstStarting, name, "", "preparing container", proto.PhaseContainerPrepare, 0, 0, 0, 0)
	runDone := make(chan struct {
		id  string
		err error
	}, 1)
	var runProgressMu sync.Mutex
	runPhase, runMessage := "container_start", "starting container"
	go func() {
		var id string
		var err error
		if progressDriver, ok := driver.(runtime.RunProgressDriver); ok {
			id, err = progressDriver.RunWithProgress(ctx, ws, func(phase, message string) {
				runProgressMu.Lock()
				runPhase, runMessage = phase, message
				runProgressMu.Unlock()
				e.reportStateProgress(spec, proto.InstStarting, name, "", message, phase, 0, 0, 0, 0)
			})
		} else {
			id, err = driver.Run(ctx, ws)
		}
		runDone <- struct {
			id  string
			err error
		}{id: id, err: err}
	}()
	ticker := time.NewTicker(5 * time.Second)
	var runResult struct {
		id  string
		err error
	}
	for {
		select {
		case runResult = <-runDone:
			ticker.Stop()
			goto runFinished
		case <-ticker.C:
			runProgressMu.Lock()
			phase, message := runPhase, runMessage
			runProgressMu.Unlock()
			// The heartbeat repeats the current phase verbatim. Stamping the
			// elapsed time into it would make every tick a different message,
			// which is what turned a quiet container start into an event row
			// per tick; how long a phase has lasted is derivable from the
			// timestamps the control plane already stores.
			e.reportStateProgress(spec, proto.InstStarting, name, "", message, phase, 0, 0, 0, 0)
		}
	}
runFinished:
	id, err := runResult.id, runResult.err
	if err != nil {
		e.failAfterContainer(ctx, spec, "run failed: "+err.Error())
		return
	}
	e.collectAndPublishRuntimeObservation(ctx, spec, id, id, epoch)
	e.probeAndWatchStartup(ctx, spec, id, epoch, true)
}

func (e *Engine) invalidateRuntimeEpochLocked(uuid string) {
	e.cancelHealthProbeLocked(uuid)
	e.nextRuntimeEpoch++
	e.runtimeEpoch[uuid] = e.nextRuntimeEpoch
	delete(e.servingEpoch, uuid)
}

func runtimeAssetDigests(assets []proto.RuntimeAsset) []string {
	seen := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		if digest := strings.ToLower(strings.TrimSpace(asset.SHA256)); digest != "" {
			seen[digest] = struct{}{}
		}
	}
	digests := make([]string, 0, len(seen))
	for digest := range seen {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	return digests
}
