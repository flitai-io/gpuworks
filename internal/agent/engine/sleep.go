package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// sleepOrderTimeout bounds one HTTP order. A wait-mode sleep finishes the
// requests in flight first and an L2 wake reads the weights back from disk, so
// this is generous; the 2026-08 evaluation measured a 756 GB model's L2 wake at
// ~5 min native and ~7 min under udocker.
const sleepOrderTimeout = 10 * time.Minute

// This file is the agent's implementation of the vLLM sleep-mode protocol,
// which is the only sleep protocol an engine profile declares today
// (SleepAPI "/sleep"). The three-step L2 wake is protocol knowledge, not
// deployment knowledge: it is the same for every model and image.
//
//	POST /sleep?level=N&mode=M   level 1 offloads weights to host RAM and drops the
//	                             KV cache; level 2 discards both. mode says what
//	                             happens to requests in flight (wait | abort).
//	POST /wake_up                brings an L1 sleeper back in one step.
//	POST /wake_up?tags=weights   an L2 sleeper first gets its weight buffers back,
//	POST /collective_rpc         then has every worker reload them from the model
//	                             directory ({"method":"reload_weights"}),
//	POST /wake_up?tags=kv_cache  then gets its KV cache back,
//	POST /reset_prefix_cache     then drops cache entries built from the old
//	                             weight buffers before serving again.

// reconcileSleep drives a running instance to its desired sleep level and
// reports the resulting level. A failed order is reported in the sleep phase
// with the reason; the server rolls the deployment intent back so a later
// explicit order is a fresh transition.
func (e *Engine) reconcileSleep(ctx context.Context, spec proto.InstanceDesiredState) {
	if !spec.Sleepable {
		return
	}
	e.mu.Lock()
	cur := e.processOfLocked(spec.UUID).sleepLevel
	e.mu.Unlock()
	if cur == spec.DesiredSleep {
		return
	}

	err := e.applySleepLevel(ctx, spec, cur, spec.DesiredSleep)
	if err != nil {
		target := spec.DesiredSleep
		e.log.Warn("sleep reconcile failed", "uuid", spec.UUID, "desired", spec.DesiredSleep, "err", err)
		// /is_sleeping reports only a boolean. An L2 wake can allocate the weight
		// buffers and then fail while the executor still reports sleeping. There
		// is no safe way to infer how much GPU memory is held in that state, so
		// fail closed and let the controller rebuild the instance.
		if cur >= 2 && target < cur {
			e.reportUncertainSleep(spec, err, errors.New("L2 wake may have partially allocated weights"))
			return
		}
		// For an ordinary failed sleep or L1 wake, probe the boolean endpoint and
		// restore the old level only when the probe proves that the engine is
		// awake. This keeps a confirmed sleeper's residue claim conservative.
		actual, probeErr := e.probeSleepState(context.WithoutCancel(ctx), spec)
		recovered := false
		if cur > 0 && (probeErr != nil || actual == 0) {
			recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			recoverErr := e.sleepTo(recoverCtx, fmt.Sprintf("http://127.0.0.1:%d", spec.APIPort), cur, proto.SleepModeAbort)
			cancel()
			if recoverErr == nil {
				actual, probeErr = e.probeSleepState(context.WithoutCancel(ctx), spec)
				recovered = recoverErr == nil && probeErr == nil && actual > 0
			}
			if recoverErr != nil || probeErr != nil || actual == 0 {
				e.reportUncertainSleep(spec, err, fmt.Errorf("restore level %d: %v; probe: %v", cur, recoverErr, probeErr))
				return
			}
		}
		if (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) && !recovered {
			e.reportUncertainSleep(spec, err, fmt.Errorf("timed-out request may still change engine state"))
			return
		}
		if probeErr != nil {
			e.reportUncertainSleep(spec, err, probeErr)
			return
		}
		if cur == 0 && actual > 0 {
			// The engine answered the sleep with an error yet says it sleeps:
			// vLLM pauses its scheduler before it offloads, so /is_sleeping
			// reads true while the weights may be only partly moved out. Such
			// an engine wakes into a no-op and crashes on the next request
			// (an illegal memory access), so fail closed and rebuild it.
			var refused *engineStatusError
			if errors.As(err, &refused) {
				e.reportUncertainSleep(spec, err, errors.New("the engine refused the sleep but reports sleeping; its weights may be partly offloaded"))
				return
			}
			// The sleep took effect despite a transport failure. The target
			// level is the only depth the boolean endpoint lets us infer; report
			// it as a successful transition so the UI does not show a false
			// failure.
			e.mu.Lock()
			e.setSleepLevelLocked(spec.UUID, target)
			e.mu.Unlock()
			e.report(proto.InstanceStatus{
				UUID: spec.UUID, State: proto.InstRunning, ContainerID: containerName(spec.UUID),
				Ports: spec.Ports, DirectAddr: fmt.Sprintf("127.0.0.1:%d", spec.APIPort), SleepLevel: target, SleepTarget: &target,
				SleepResidueBytes: e.measureSleepResidue(ctx, spec), L1HostBufferHeld: e.holdsL1HostBuffer(spec.UUID),
			})
			return
		}
		e.report(proto.InstanceStatus{
			UUID: spec.UUID, State: proto.InstRunning, ContainerID: containerName(spec.UUID),
			Ports: spec.Ports, DirectAddr: fmt.Sprintf("127.0.0.1:%d", spec.APIPort),
			SleepLevel: cur, SleepTarget: &target, Phase: proto.PhaseSleep, L1HostBufferHeld: e.holdsL1HostBuffer(spec.UUID),
			Message: fmt.Sprintf("sleep level %d → %d failed: %v", cur, spec.DesiredSleep, err),
		})
		return
	}

	e.mu.Lock()
	e.setSleepLevelLocked(spec.UUID, spec.DesiredSleep)
	process := e.processLocked(spec.UUID)
	if spec.DesiredSleep == 0 {
		process.sleepPIDs = nil
	}
	if spec.DesiredSleep == proto.SleepLevelL1 {
		process.l1HostHeld = true
	}
	e.mu.Unlock()
	target := spec.DesiredSleep

	// Only a completed sleep has a residue; a wake or a failed order reports none.
	var residue int64
	if target > 0 {
		residue = e.measureSleepResidue(ctx, spec)
	}
	e.report(proto.InstanceStatus{
		UUID: spec.UUID, State: proto.InstRunning, ContainerID: containerName(spec.UUID),
		Ports: spec.Ports, DirectAddr: fmt.Sprintf("127.0.0.1:%d", spec.APIPort),
		SleepLevel: spec.DesiredSleep, SleepTarget: &target, SleepResidueBytes: residue,
		L1HostBufferHeld: e.holdsL1HostBuffer(spec.UUID),
	})
	e.log.Info("instance sleep level applied", "uuid", spec.UUID, "level", spec.DesiredSleep)
}

// reportUncertainSleep keeps the gateway closed and the full GPU claim held.
// The controller deliberately rebuilds this failed generation into a known
// engine state. That is the recovery action; the agent never retries the
// ambiguous command in the background.
func (e *Engine) reportUncertainSleep(spec proto.InstanceDesiredState, orderErr, recoveryErr error) {
	// This failure ends the generation; its container's later exit is no news.
	e.mu.Lock()
	delete(e.servingEpoch, spec.UUID)
	e.mu.Unlock()
	detail := fmt.Sprintf("%v: %v", orderErr, recoveryErr)
	e.reportFailed(spec.UUID, proto.RuntimeFailure{Code: proto.ErrorSleepStateUncertain, Phase: proto.PhaseSleep, Detail: detail},
		"sleep level uncertain after "+detail, failedStatus{ContainerID: containerName(spec.UUID), Phase: proto.PhaseSleep})
}

// applySleepLevel issues the orders that take the engine from level cur to
// level want. A change between two sleep levels goes through awake, because
// the engine only sleeps from awake.
func (e *Engine) applySleepLevel(ctx context.Context, spec proto.InstanceDesiredState, cur, want int) error {
	base := fmt.Sprintf("http://127.0.0.1:%d", spec.APIPort)
	if cur > 0 {
		if err := e.wakeFrom(ctx, base, cur); err != nil {
			return err
		}
	}
	if want > 0 {
		return e.sleepTo(ctx, base, want, spec.SleepMode)
	}
	return nil
}

func (e *Engine) sleepTo(ctx context.Context, base string, level int, mode string) error {
	if mode == "" {
		mode = proto.SleepModeWait
	}
	return e.vllmPost(ctx, fmt.Sprintf("%s/sleep?level=%d&mode=%s", base, level, mode), nil)
}

// wakeFrom brings the engine back from the given level. Only an L2 sleeper
// needs the weights reloaded; waking it with the plain order would leave it
// serving from buffers that hold nothing.
func (e *Engine) wakeFrom(ctx context.Context, base string, level int) error {
	if level < 2 {
		return e.vllmPost(ctx, base+"/wake_up", nil)
	}
	if err := e.vllmPost(ctx, base+"/wake_up?tags=weights", nil); err != nil {
		return err
	}
	if err := e.vllmPost(ctx, base+"/collective_rpc", []byte(`{"method":"reload_weights"}`)); err != nil {
		return fmt.Errorf("reload weights after L2 sleep: %w", err)
	}
	if err := e.vllmPost(ctx, base+"/wake_up?tags=kv_cache", nil); err != nil {
		return err
	}
	// L2 discards the model weights. vLLM's prefix cache can still contain
	// blocks produced with those old buffers, so it must be invalidated after
	// the staged wake completes or the next request may decode garbage.
	return e.vllmPost(ctx, base+"/reset_prefix_cache", nil)
}

// prewarmSleep hides the one-time cold L1 sleep cost (17s–307s in the eval) by
// doing one sleep→wake cycle right after the instance becomes ready. Nothing is
// in flight yet, so the pause mode is irrelevant and the default is used.
func (e *Engine) prewarmSleep(ctx context.Context, spec proto.InstanceDesiredState) error {
	if !spec.Sleepable {
		return nil
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", spec.APIPort)
	if err := e.vllmPost(ctx, base+"/sleep?level=1", nil); err != nil {
		e.log.Debug("prewarm sleep failed", "uuid", spec.UUID, "err", err)
		return err
	}
	e.mu.Lock()
	e.setSleepLevelLocked(spec.UUID, 1)
	e.processLocked(spec.UUID).l1HostHeld = true
	e.mu.Unlock()
	if err := e.vllmPost(ctx, base+"/wake_up", nil); err != nil {
		e.log.Debug("prewarm wake failed", "uuid", spec.UUID, "err", err)
		return err
	}
	e.mu.Lock()
	e.setSleepLevelLocked(spec.UUID, 0)
	e.mu.Unlock()
	e.log.Info("sleep prewarm complete", "uuid", spec.UUID)
	return nil
}

// adoptSleepLevel records the level of a container adopted after an agent
// restart. A level this agent already applied itself is kept: the reconcile
// that follows a desired-state change adopts the same container again, and
// the engine's own answer is only whether it is asleep, not how deeply.
//
// For a sleeper this agent has no memory of, the server's desired level is the
// last order pushed to it, so that is its level. When that order was to be
// awake, the sleep predates the agent's memory and the depth is unknown; it is
// read as L2, because the three-step L2 wake also brings an L1 sleeper back
// (reloading weights it still had is merely slower), whereas the one-step L1
// wake leaves an L2 sleeper serving from empty buffers.
func (e *Engine) adoptSleepLevel(ctx context.Context, spec proto.InstanceDesiredState) error {
	e.mu.Lock()
	known := e.processOfLocked(spec.UUID).sleepKnown
	e.mu.Unlock()
	if known {
		return nil
	}
	sleeping, err := e.probeSleepState(ctx, spec)
	if err != nil {
		return err
	}
	level := 0
	if sleeping > 0 {
		level = spec.DesiredSleep
		if level == 0 {
			level = 2
		}
	}
	e.mu.Lock()
	e.setSleepLevelLocked(spec.UUID, level)
	e.mu.Unlock()
	return nil
}

// holdsL1HostBuffer says the engine of uuid completed an L1 sleep since its
// container started, here (its prewarm or an order). vLLM copies the weights
// into pinned host memory on an L1 sleep and keeps that memory for the life of
// the process, awake or asleep, so its next L1 sleep takes no host memory it
// does not already hold. A container adopted after an agent restart is not
// known to, though it may.
func (e *Engine) holdsL1HostBuffer(uuid string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.processOfLocked(uuid).l1HostHeld
}

func (e *Engine) vllmPost(ctx context.Context, url string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, sleepOrderTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &engineStatusError{url: url, status: resp.StatusCode}
	}
	return nil
}

// engineStatusError is an order the engine answered with an error status: it
// says the order failed, unlike a transport error, after which the order may
// still have run to the end.
type engineStatusError struct {
	url    string
	status int
}

func (e *engineStatusError) Error() string { return fmt.Sprintf("%s: http %d", e.url, e.status) }
