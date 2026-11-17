package proxy

import (
	"context"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// serveTarget hands a routed request to its instance and observes the reply:
// the accounting counters, the in-flight entry and the request's record
// (ADR 0042), none of which a probe (probe.go) touches.
func (p *Proxy) serveTarget(w http.ResponseWriter, r *http.Request, tgt target, deploymentID uint) {
	// Strip the platform Authorization; the engine binds localhost and needs
	// no auth through the tunnel.
	r.Header.Del("Authorization")
	r, ident := p.identify(r, "")
	id := ident.id
	w.Header().Set("X-Request-ID", id)
	reading, askUsage := p.routes.observationFor(deploymentID)
	body, injected, cleanupInjection, err := injectUsage(r, p.maxBodyBytes, askUsage)
	defer cleanupInjection()
	if err != nil {
		p.log.Warn("read request body", "request_id", id, "err", err)
	}
	if body.bytes == 0 && r.ContentLength > 0 {
		body.bytes = r.ContentLength
	}
	dispatched := time.Now()
	probe := probeFrom(r.Context())
	acct := p.acctFor(r.Context())
	live := tgt.live
	if live == nil {
		live = newLiveRequest(r, ident, deploymentID, nil, dispatched)
		if probe == nil {
			p.live.add(live)
		}
	}
	if probe == nil {
		defer p.live.remove(live)
	} else if tgt.attempt != nil {
		tgt.attempt.accountant = nil
	}
	live.dispatch(dispatched, tgt.instanceUUID, body)
	caller := live.caller
	facts := requestFacts{id: id, trace: ident.trace, model: ident.model, deploymentID: deploymentID, caller: caller, path: r.URL.Path,
		dispatchedAt: dispatched, body: body, pd: tgt.stage.record(live.receivedAt), priorityClass: priorityClassFrom(r.Context())}
	ob := &copyObservation{
		log:         p.log,
		requestID:   id,
		method:      r.Method,
		path:        r.URL.Path,
		started:     dispatched,
		declaredLen: -1,
		reading:     reading,
		live:        live,
		injected:    injected,
	}
	ob.pdCached, ob.pdCachedKnown = tgt.stage.prefillCached()
	if p.recorder != nil && probe == nil {
		if rec := p.recorder.Begin(deploymentID, id); rec != nil {
			ob.capture = newExchangeCapture(rec, dispatched)
			ob.capture.captureRequest(r, p.maxBodyBytes)
			// An exchange that never reached copying (no reply at all) ends
			// here; finish has written the end of any other.
			defer func() { ob.capture.end(map[string]any{"status": ob.status, "outcome": "no_reply"}) }()
			w = &captureWriter{ResponseWriter: w, capture: ob.capture}
		}
	}
	ob.onFinish = func(status int, duration time.Duration, stats responseStats, outcome uxrecord.Outcome) {
		if probe != nil && outcome != uxrecord.OutcomeOK {
			probe.message = "the upstream response did not complete"
		}
		acct.FinishResponse(deploymentID, status, duration, stats, reading.fallbacks)
		if p.sink != nil && probe == nil {
			instanceUUID := tgt.instanceUUID
			if tgt.attempt != nil && tgt.attempt.current != nil {
				instanceUUID = tgt.attempt.current.c.instanceUUID
			}
			p.sink(buildRecord(facts, instanceUUID, status, duration, stats, outcome, ob.firstByte))
		}
	}
	acct.Dispatch(deploymentID)
	if tgt.instanceUUID != "" {
		acct.RecordRouted(deploymentID, tgt.instanceUUID)
	}
	defer func() {
		if tgt.attempt != nil && tgt.attempt.current != nil {
			tgt.attempt.current.release(outcomeCanceled)
		} else if tgt.lease != nil {
			tgt.lease.release(outcomeCanceled)
		}
	}()
	cw := &countingResponseWriter{ResponseWriter: w, obs: ob, firstByte: tgt.firstByte}
	ctx := context.WithValue(r.Context(), targetKey, tgt)
	if tgt.attempt != nil {
		ctx = context.WithValue(ctx, attemptKey, tgt.attempt)
	}
	ctx = context.WithValue(ctx, obsKey, ob)
	p.rp.ServeHTTP(cw, r.WithContext(ctx))
	if probe != nil {
		probe.instanceUUID = tgt.instanceUUID
		if tgt.attempt != nil && tgt.attempt.current != nil {
			probe.instanceUUID = tgt.attempt.current.c.instanceUUID
		}
	}
	if tgt.attempt != nil && tgt.attempt.current != nil {
		outcome := outcomeOK
		if ob.status == http.StatusBadGateway || ob.status == http.StatusServiceUnavailable || ob.status == http.StatusGatewayTimeout {
			outcome = outcomeUpstreamUnavailable
		}
		tgt.attempt.current.release(outcome)
	} else if tgt.lease != nil {
		outcome := outcomeOK
		if ob.status == http.StatusBadGateway || ob.status == http.StatusServiceUnavailable || ob.status == http.StatusGatewayTimeout {
			outcome = outcomeUpstreamUnavailable
		}
		tgt.lease.release(outcome)
	}
}

// newLiveRequest is a request's in-flight entry, received when its caller
// says (the entry's own clock) or now; stage is its PD prefill stage.
func newLiveRequest(r *http.Request, ident requestIdentity, deploymentID uint, stage *pdStage, now time.Time) *liveRequest {
	caller, ok := uxrecord.CallerFrom(r.Context())
	if !ok || caller.ReceivedMS == 0 {
		caller.ReceivedMS = now.UnixMilli()
	}
	return &liveRequest{
		id: ident.id, traceID: ident.trace.Trace, deploymentID: deploymentID, caller: caller,
		receivedAt: time.UnixMilli(caller.ReceivedMS), stage: stage,
	}
}
