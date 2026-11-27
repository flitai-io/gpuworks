package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// probeAndReport polls the health endpoint until ready or the budget expires,
// then reports running/failed. It returns true only once running was reported.
func (e *Engine) probeAndReport(ctx context.Context, spec proto.InstanceDesiredState, id string) bool {
	return e.probeAndReportStartup(ctx, spec, id, false, true)
}

// probeAndReportStartup observes the health budget only for a newly created
// container. Adoption must not turn a quick probe of an existing service into
// a new startup sample or replace the original startup log observations.
func (e *Engine) probeAndReportStartup(ctx context.Context, spec proto.InstanceDesiredState, id string, fresh, readLogs bool) bool {
	if spec.Headless {
		return e.probeHeadlessAndReport(ctx, spec, id)
	}
	if ctx.Err() != nil {
		return false
	}
	driver := e.Driver()
	timeout := time.Duration(spec.StartTimeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: engineHealthTimeout}
	healthStarted := time.Now()
	if fresh {
		e.reportStateProgress(spec, proto.InstStarting, id, "", "waiting for engine health", proto.PhaseHealthCheck, 0, 0, 0, 0)
	}
	nextHealthReport := 10 * time.Second
	loadRequested := false
	var loadResult <-chan string
	var lastObservedAt time.Time
	var lastTarget, lastResult string
	// What ends the start of a newly created container before its timeout
	// (start_stall.go): a stall, where its signals can be observed, and an
	// engine that says it gave up. A container that is adopted, or one whose
	// engine served and is being brought back, is not a start: its output
	// holds what it logged while serving, and its quiet is not a hang.
	var stall *stallWatch
	var abortedBefore bool
	nextStallSample := time.Now().Add(e.startStall.intervalOrDefault())
	if fresh && e.watchesStartStall(spec) {
		stall = e.startStall.watch()
	}

	for time.Now().Before(deadline) {
		st, err := driver.Status(ctx, id)
		if ctx.Err() != nil {
			return false
		}
		if err == nil && !st.Exists {
			e.failAfterContainer(ctx, spec, proto.RuntimeFailure{Code: proto.ErrorContainerDisappeared, Phase: proto.PhaseHealthCheck}, "container disappeared during startup")
			return false
		}
		if err == nil && st.Exists && !st.Running {
			failure, msg := e.exitedFailure(ctx, id, st, spec.VRAMLimitBytes, proto.PhaseHealthCheck, "container OOM-killed", fmt.Sprintf("container exited (code %d)", st.ExitCode))
			e.failAfterContainer(ctx, spec, failure, msg)
			return false
		}
		req, err := engineHealthRequest(ctx, spec)
		if err != nil {
			e.failAfterContainer(ctx, spec, proto.RuntimeFailure{Code: proto.ErrorEngineHealthEndpointInvalid, Phase: proto.PhaseHealthCheck}, "invalid engine health endpoint")
			return false
		}
		lastTarget = healthDiagnosticTarget(req.URL)
		resp, err := client.Do(req)
		lastObservedAt = time.Now()
		lastResult = healthTransportResult(err)
		if err == nil {
			lastResult = fmt.Sprintf("HTTP %d", resp.StatusCode)
			readiness, decodeErr := readEngineReadiness(resp)
			if spec.Engine == engines.Callable && decodeErr == nil {
				if failure, message := callableStartupFailure(resp, readiness); message != "" {
					if ctx.Err() == nil {
						e.failAfterContainer(ctx, spec, failure, message)
					}
					return false
				}
			}
			ready := resp.StatusCode == http.StatusOK
			if ready && readiness.Service == "strata" {
				var childAbsent bool
				ready, lastResult, childAbsent = strataChildReady(ctx, client, spec.APIPort, resp.StatusCode, readiness, decodeErr)
				if childAbsent && !loadRequested {
					loadRequested = true
					e.reportStateProgress(spec, proto.InstStarting, id, "", "engine child is not loaded; recovering", proto.PhaseHealthCheck, 0, 0, 0, 0)
					loadResult = requestStrataLoad(ctx, client, spec.APIPort)
				}
			}
			if ready {
				return e.reportHealthy(ctx, client, spec, id, fresh, readLogs, healthStarted)
			}
		}
		if now := time.Now(); fresh && !now.Before(nextStallSample) {
			nextStallSample = now.Add(e.startStall.intervalOrDefault())
			failure, message, failed := e.startMakesNoProgress(ctx, spec, id, stall, &abortedBefore)
			if ctx.Err() != nil {
				return false
			}
			if failed {
				failure, message = unstatedPortFailure(spec, lastResult, failure, message)
				e.log.Warn("engine start failed before its timeout", "uuid", spec.UUID, "code", failure.Code, "detail", failure.Detail)
				// No startup observation: its budget did not run out.
				e.failAfterContainer(ctx, spec, failure, message)
				return false
			}
		}
		if loadResult != nil {
			select {
			case detail := <-loadResult:
				loadResult = nil
				if detail != "" && ctx.Err() == nil {
					e.reportStateProgress(spec, proto.InstStarting, id, "", detail, proto.PhaseHealthCheck, 0, 0, 0, 0)
				}
			default:
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(engineHealthRetryInterval):
			// The message stays identical for every heartbeat: a progress
			// report is overwritable state, and an elapsed counter baked into
			// the text would make each one look like a new milestone. The UI
			// derives "how long" from the event and instance timestamps.
			if elapsed := time.Since(healthStarted); elapsed >= nextHealthReport {
				e.reportStateProgress(spec, proto.InstStarting, id, "", healthWaitMessage(spec, lastTarget, lastResult), proto.PhaseHealthCheck, 0, 0, 0, 0)
				nextHealthReport += 10 * time.Second
			}
		}
	}
	e.failHealthTimeout(ctx, spec, id, fresh, healthStarted, lastObservedAt, lastTarget, lastResult, stall)
	return false
}

// strataChildReady reads a strata supervisor's healthy answer: its engine
// child is ready when loaded, or when its models were unloaded on purpose.
// result is what the health diagnostic shows for this answer. childAbsent
// says the supervisor lists no model at all, so a load has to be asked for.
func strataChildReady(ctx context.Context, client *http.Client, port, status int, readiness engineReadiness, decodeErr error) (ready bool, result string, childAbsent bool) {
	if decodeErr != nil || readiness.Loaded == nil {
		return false, fmt.Sprintf("HTTP %d, readiness=unknown", status), false
	}
	if *readiness.Loaded {
		return true, fmt.Sprintf("HTTP %d", status), false
	}
	result = fmt.Sprintf("HTTP %d, readiness=not_loaded", status)
	if readiness.APIKey == nil || *readiness.APIKey {
		return false, result, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/models", port), nil)
	if err != nil {
		return false, result, false
	}
	resp, err := client.Do(req) //nolint:bodyclose // readStrataModels consumes and closes the successful response.
	if err != nil {
		return false, result, false
	}
	models, valid := readStrataModels(resp)
	if !valid {
		return false, result, false
	}
	return intentionallyUnloaded(models), result, len(models) == 0
}

// reportHealthy takes an instance whose engine answered healthy to running:
// the answer must be its own model's, a sleepable instance is prewarmed
// once, and the report carries what the start measured. It returns false
// when one of those fails the instance or ctx ended first.
func (e *Engine) reportHealthy(ctx context.Context, client *http.Client, spec proto.InstanceDesiredState, id string, fresh, readLogs bool, healthStarted time.Time) bool {
	// A healthy answer on the port may come from someone else's
	// engine while ours is still loading weights.
	if err := checkServesOwnModel(ctx, client, spec); err != nil {
		e.failAfterContainer(ctx, spec, failureOf(err, proto.ErrorEnginePortInUse, proto.PhaseHealthCheck), err.Error())
		return false
	}
	e.log.Info("instance healthy", "uuid", spec.UUID, "port", spec.APIPort)
	var startupSeconds *float64
	if fresh {
		elapsed := time.Since(healthStarted).Seconds()
		startupSeconds = &elapsed
	}
	e.mu.Lock()
	process := e.processLocked(spec.UUID)
	doPrewarm := spec.Sleepable && spec.DesiredSleep == 0 && !process.prewarmed
	if doPrewarm {
		process.prewarmed = true
	}
	e.mu.Unlock()
	if doPrewarm {
		e.reportState(spec, proto.InstOptimizing, id, "", "prewarming sleep")
		if err := e.prewarmSleep(ctx, spec); err != nil {
			e.failAfterContainer(ctx, spec, proto.RuntimeFailure{Code: proto.ErrorSleepPrewarmFailed, Phase: proto.PhaseHealthCheck, Detail: err.Error()}, "sleep prewarm failed: "+err.Error())
			return false
		}
	}
	e.mu.Lock()
	actualSleep := e.processOfLocked(spec.UUID).sleepLevel
	e.mu.Unlock()
	var measured json.RawMessage
	if readLogs {
		measured = e.collectMeasured(ctx, spec, id, startupSeconds)
	}
	paths := readOpenAPIPaths(ctx, client, spec.APIPort)
	if ctx.Err() != nil {
		return false
	}
	e.report(proto.InstanceStatus{
		UUID: spec.UUID, State: proto.InstRunning, ContainerID: id,
		Ports: spec.Ports, DirectAddr: fmt.Sprintf("127.0.0.1:%d", spec.APIPort),
		Measured: measured, SleepLevel: actualSleep,
		OpenAPIPaths:     paths,
		L1HostBufferHeld: e.holdsL1HostBuffer(spec.UUID),
	})
	return true
}

// failHealthTimeout fails an instance whose engine never answered healthy
// within its start timeout, with the last answer seen and, where the start
// was watched, what its processes were doing.
func (e *Engine) failHealthTimeout(ctx context.Context, spec proto.InstanceDesiredState, id string, fresh bool, healthStarted, lastObservedAt time.Time, lastTarget, lastResult string, stall *stallWatch) {
	message := "health check timed out"
	failure := proto.RuntimeFailure{Code: proto.ErrorEngineHealthTimeout, Phase: proto.PhaseHealthCheck}
	if !lastObservedAt.IsZero() {
		failure.Detail = healthDiagnosticDetail(lastTarget, lastResult)
		message += ": " + failure.Detail + " observed_at=" + lastObservedAt.UTC().Format(time.RFC3339Nano)
	}
	if stall != nil {
		// What the container's processes were doing when the time ran out:
		// a start that was slow short of a stall shows why here.
		if processes := stall.processes(); processes != "" {
			failure.Detail = strings.TrimPrefix(failure.Detail+"; "+processes, "; ")
		}
	}
	failure, message = unstatedPortFailure(spec, lastResult, failure, message)
	if ctx.Err() != nil {
		return
	}
	var measured json.RawMessage
	if fresh {
		elapsed := time.Since(healthStarted).Seconds()
		measured, _ = json.Marshal(enginelog.Measured{StartupTimeoutSeconds: &elapsed})
	}
	e.attachOutput(ctx, id, spec.VRAMLimitBytes, &failure, true)
	e.failAfterContainer(ctx, spec, failure, message, measured)
}

// probeHeadlessAndReport verifies that a distributed worker remains alive for
// several consecutive runtime inspections. A follower intentionally has no
// HTTP API, so probing APIPort would always time out. The leader's HTTP health
// check plus the server-side all-ranks readiness gate complete the group check.
func (e *Engine) probeHeadlessAndReport(ctx context.Context, spec proto.InstanceDesiredState, id string) bool {
	driver := e.Driver()
	timeout := time.Duration(spec.StartTimeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	interval := e.headlessProbeInterval
	if interval <= 0 {
		interval = time.Second
	}
	required := e.headlessStableChecks
	if required <= 0 {
		required = 3
	}
	deadline := time.Now().Add(timeout)
	stable := 0
	for time.Now().Before(deadline) {
		st, err := driver.Status(ctx, id)
		if ctx.Err() != nil {
			return false
		}
		if err == nil && !st.Exists {
			e.failAfterContainer(ctx, spec, proto.RuntimeFailure{Code: proto.ErrorContainerDisappeared, Phase: proto.PhaseHealthCheck}, "headless worker disappeared during startup")
			return false
		}
		if err == nil && st.Exists {
			if !st.Running || st.Health == "unhealthy" {
				var failure proto.RuntimeFailure
				var msg string
				if st.Running && st.Health == "unhealthy" && !e.detectOOM(ctx, id, st) {
					failure, msg = proto.RuntimeFailure{Code: proto.ErrorContainerUnhealthy, Phase: proto.PhaseHealthCheck}, "headless worker is unhealthy"
					e.attachOutput(ctx, id, spec.VRAMLimitBytes, &failure, false)
				} else {
					failure, msg = e.exitedFailure(ctx, id, st, spec.VRAMLimitBytes, proto.PhaseHealthCheck, "headless worker OOM-killed", fmt.Sprintf("headless worker exited (code %d)", st.ExitCode))
				}
				e.failAfterContainer(ctx, spec, failure, msg)
				return false
			}
			stable++
			if stable >= required {
				e.log.Info("headless worker stable", "uuid", spec.UUID, "rank", spec.NodeRank)
				e.report(proto.InstanceStatus{
					UUID: spec.UUID, State: proto.InstRunning, ContainerID: id,
					Message: fmt.Sprintf("headless rank %d ready", spec.NodeRank),
				})
				return true
			}
		} else {
			stable = 0
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
	failure := proto.RuntimeFailure{Code: proto.ErrorWorkerReadinessTimeout, Phase: proto.PhaseHealthCheck}
	e.attachOutput(ctx, id, spec.VRAMLimitBytes, &failure, true)
	e.failAfterContainer(ctx, spec, failure, "headless worker readiness timed out")
	return false
}

func (e *Engine) detectOOM(ctx context.Context, id string, st runtime.Status) bool {
	if st.OOMKilled {
		return true
	}
	if st.ExitCode != 137 {
		return false
	}
	rc, err := e.Driver().Logs(ctx, id, false, 200)
	if err != nil {
		return false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 256*1024))
	return err == nil && (strings.Contains(string(b), "OutOfMemoryError") || strings.Contains(string(b), "CUDA out of memory"))
}

// collectMeasured reads recent container logs and parses the engine's real
// memory breakdown for estimate calibration. Unsupported log formats are
// intentionally silent.
func (e *Engine) collectMeasured(ctx context.Context, spec proto.InstanceDesiredState, id string, startupSeconds *float64) json.RawMessage {
	// The window must reach back to the start of the launch: the SGLang parser
	// reports no memory sums from a tail that has lost it, and an engine on
	// many GPUs logs dozens of lines per GPU before it is healthy. This runs
	// once per healthy transition, so the larger read is not on a hot path.
	var m enginelog.Measured
	rc, err := e.Driver().Logs(ctx, id, false, 4000)
	if err == nil {
		defer rc.Close()
		if data, readErr := io.ReadAll(io.LimitReader(rc, 4<<20)); readErr == nil {
			m = enginelog.ParseDialect(spec.LogDialect, string(data))
		}
	}
	// The successful readiness observation is independent of whether the
	// engine logs were available or in a recognized dialect.
	m.StartupSeconds = startupSeconds
	if !m.Found && m.StartupSeconds == nil && m.ModelLoadingSeconds == nil && m.CompilationSeconds == nil && m.GraphCaptureSeconds == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}
