package engine

import (
	"context"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// defaultLivenessInterval is how often running instances' containers are
// checked after startup. A round costs one runtime listing however many
// instances the node packs, plus one inspect for each container the listing
// does not show as running; a rebuild takes minutes to load a model, which
// makes detecting an exit a few seconds sooner worth little.
const defaultLivenessInterval = 15 * time.Second

// After startup reports running, the liveness monitor checks the container
// and schedules an independent HTTP probe for each awake serving instance.
// Container exits and repeated HTTP failures retire the instance through the
// same cleanup/report path, so the controller can rebuild a failed attempt.
//
// Which instances it watches cannot be derived from the desired, in-flight and
// cleanup maps. Once an ensure attempt has ended, an instance that is desired
// running with an absent or exited container is in one of two states that look
// the same from the runtime: its startup failed and was already reported (the
// startup path reclaims the container before it reports), or it was serving
// and its container is gone. Reporting the first would repeat a failure the
// control plane already has, on every round. Only the attempt's own outcome
// tells them apart, so servingEpoch records the attempt that reported running.
// Tying it to the epoch makes it expire by construction: removal invalidates
// the epoch and every later start/adopt attempt allocates a new one.

// RunLivenessMonitor watches running instances until ctx ends.
func (e *Engine) RunLivenessMonitor(ctx context.Context) {
	defer e.stopHealthProbes()
	interval := e.livenessInterval
	if interval <= 0 {
		interval = defaultLivenessInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.checkLiveness(ctx)
		}
	}
}

// probeAndWatch runs the startup probe and, once it has reported the instance
// running, hands the container to the liveness monitor.
func (e *Engine) probeAndWatch(ctx context.Context, spec proto.InstanceDesiredState, id string, epoch uint64) {
	e.probeAndWatchStartup(ctx, spec, id, epoch, false)
}

func (e *Engine) probeAndWatchStartup(ctx context.Context, spec proto.InstanceDesiredState, id string, epoch uint64, fresh bool) {
	if !e.probeAndReportStartup(ctx, spec, id, fresh, fresh) {
		return
	}
	e.mu.Lock()
	if e.runtimeEpoch[spec.UUID] == epoch {
		e.servingEpoch[spec.UUID] = epoch
	}
	e.mu.Unlock()
}

type servingInstance struct {
	spec  proto.InstanceDesiredState
	epoch uint64
}

// checkLiveness runs one round over every watched instance. The runtime's
// listing only spares the inspect: a container exit is decided by its own
// inspect, so a listing that fails or leaves a container out costs that round's
// saving and decides nothing. HTTP retries run outside this loop.
func (e *Engine) checkLiveness(ctx context.Context) {
	instances := e.servingInstances()
	if len(instances) == 0 {
		return
	}
	running := e.runningContainers(ctx)
	for _, serving := range instances {
		if ctx.Err() != nil {
			return
		}
		if running[containerName(serving.spec.UUID)] {
			e.startHealthProbe(ctx, serving)
			continue
		}
		e.checkServing(ctx, serving.spec, serving.epoch)
	}
}

// runningContainers is the set of container names the runtime lists as
// running, empty when it could not be asked.
func (e *Engine) runningContainers(ctx context.Context) map[string]bool {
	driver := e.Driver()
	if driver == nil {
		return nil
	}
	names, err := driver.RunningContainers(ctx)
	if err != nil {
		e.log.Debug("liveness listing failed", "err", err)
		return nil
	}
	running := make(map[string]bool, len(names))
	for _, name := range names {
		running[name] = true
	}
	return running
}

// servingInstances lists the instances the monitor may inspect now. Markers
// that no longer match the instance's epoch are dropped: a removal or a later
// attempt owns that instance, and a later attempt marks it again if it serves.
func (e *Engine) servingInstances() []servingInstance {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]servingInstance, 0, len(e.servingEpoch))
	for uuid, epoch := range e.servingEpoch {
		if _, desired := e.desired[uuid]; !desired || e.runtimeEpoch[uuid] != epoch {
			delete(e.servingEpoch, uuid)
			continue
		}
		if spec, ok := e.watchableLocked(uuid, epoch); ok {
			out = append(out, servingInstance{spec: spec, epoch: epoch})
		}
	}
	return out
}

// watchableLocked reports whether the instance is still the one the serving
// attempt left behind: desired running, in the same epoch, with no cleanup
// pending and no reconcile, removal or sleep change in flight.
func (e *Engine) watchableLocked(uuid string, epoch uint64) (proto.InstanceDesiredState, bool) {
	spec, desired := e.desired[uuid]
	_, cleanup := e.cleanupRequired[uuid]
	ok := desired && spec.DesiredState == "running" && !cleanup && !e.inFlight[uuid] &&
		e.runtimeEpoch[uuid] == epoch && e.servingEpoch[uuid] == epoch
	return spec, ok
}

// checkServing inspects one watched container and retires the instance when
// the runtime says, without error, that the container is no longer running.
func (e *Engine) checkServing(ctx context.Context, spec proto.InstanceDesiredState, epoch uint64) {
	driver := e.Driver()
	if driver == nil {
		return
	}
	name := containerName(spec.UUID)
	st, err := driver.Status(ctx, name)
	if err != nil {
		// A failed inspect proves nothing about the container (the same rule
		// as startOrAdoptEpoch); the next round looks again.
		e.log.Debug("liveness inspect failed", "uuid", spec.UUID, "container", name, "err", err)
		return
	}
	if st.Exists && st.Running {
		e.startHealthProbe(ctx, servingInstance{spec: spec, epoch: epoch})
		return
	}
	// An absent container counts as well: the agent's own teardowns take the
	// instance out of the watch before they touch the container, so a watched
	// container that is gone was removed behind the agent's back.

	// Take the instance's serialized ownership exactly as ensure does, after
	// checking under the lock that nothing has replaced or removed it since
	// the round began. Clearing the marker here is what makes the report
	// happen once: no later round considers this generation again.
	e.mu.Lock()
	current, ok := e.watchableLocked(spec.UUID, epoch)
	if !ok {
		e.mu.Unlock()
		return
	}
	delete(e.servingEpoch, spec.UUID)
	e.inFlight[spec.UUID] = true
	attemptCtx, cancel := context.WithCancel(ctx)
	e.inFlightCancel[spec.UUID] = cancel
	e.mu.Unlock()
	// finishEnsure releases the ownership; if a removal arrived meanwhile it
	// also reclaims the instance and acknowledges the teardown, and a desired
	// state queued behind this check runs next.
	defer e.finishEnsure(current)

	message := e.servingExitMessage(attemptCtx, current, name, st)
	e.log.Warn("running instance's container stopped", "uuid", current.UUID, "container", name, "exists", st.Exists, "exit_code", st.ExitCode, "reason", message)
	e.retireStopped(attemptCtx, current, epoch, message)
}

// servingExitMessage words the failure the way the startup probes do, with
// the exit code always included. The OOM check reads the container's logs, so
// it runs before the container is reclaimed.
func (e *Engine) servingExitMessage(ctx context.Context, spec proto.InstanceDesiredState, name string, st runtime.Status) string {
	subject := "container"
	if spec.Headless {
		subject = "headless worker"
	}
	if !st.Exists {
		return subject + " disappeared while running (removed outside the agent)"
	}
	if e.detectOOM(ctx, name, st) {
		return fmt.Sprintf("%s OOM-killed while running (code %d)", subject, st.ExitCode)
	}
	return e.exitMessage(ctx, name, fmt.Sprintf("%s exited while running (code %d)", subject, st.ExitCode))
}

// retireStopped reclaims the dead generation and reports it failed, the same
// order failAfterContainer uses at startup: the partition and pre-start state
// go with the container, so the controller's rebuild never waits on them. A
// removal that took the instance over meanwhile owns the outcome instead.
func (e *Engine) retireStopped(ctx context.Context, spec proto.InstanceDesiredState, epoch uint64, message string) {
	err := e.reclaimInstance(ctx, spec.UUID)
	if err == nil {
		e.forgetSleepState(spec.UUID)
	}
	e.mu.Lock()
	current, desired := e.desired[spec.UUID]
	_, cleanup := e.cleanupRequired[spec.UUID]
	owned := desired && current.DesiredState == "running" && !cleanup && e.runtimeEpoch[spec.UUID] == epoch
	e.mu.Unlock()
	if !owned {
		return
	}
	if err != nil {
		// Reported once like a startup failure whose cleanup failed; a later
		// state sync retries the teardown.
		e.log.Error("stopped instance cleanup pending", "uuid", spec.UUID, "err", err)
		e.report(proto.InstanceStatus{UUID: spec.UUID, State: proto.InstRemoving, Message: message + "; cleanup pending: " + err.Error()})
		return
	}
	e.reportState(spec, proto.InstFailed, "", "", message)
}
