package proxy

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"gpuworks.ai/gpuworks/internal/proto"
)

func (t *retryTransport) roundTripAttempt(req *http.Request, c candidate, number int) (*http.Response, error) {
	// DialContext can outlive RoundTrip on cancellation. Give it a snapshot
	// of this attempt's destination rather than the mutable routing lease.
	ctx := req.Context()
	if targetForDial, ok := ctx.Value(targetKey).(target); ok &&
		(targetForDial.nodeUUID != c.nodeUUID || targetForDial.port != c.port || targetForDial.instanceUUID != c.instanceUUID) {
		targetForDial.nodeUUID, targetForDial.instanceUUID, targetForDial.port = c.nodeUUID, c.instanceUUID, c.port
		ctx = context.WithValue(ctx, targetKey, targetForDial)
		req = req.WithContext(ctx)
	}
	ob, _ := ctx.Value(obsKey).(*copyObservation)
	if ob == nil || ob.capture == nil {
		return t.guard.roundTrip(t.base, req, c)
	}

	trace := &proto.TunnelTrace{RequestID: ob.requestID, Attempt: number}
	ctx = proto.WithTunnelTrace(ctx, trace)
	var firstByte atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotFirstResponseByte: func() { firstByte.Store(true) }})

	resp, err := t.guard.roundTrip(t.base, req.WithContext(ctx), c)
	event := map[string]any{
		"t": "attempt", "request_id": trace.RequestID, "attempt": number, "instance_uuid": c.instanceUUID,
		"received_first_byte": firstByte.Load(), "client_canceled": req.Context().Err() != nil,
		"body_replayable": req.GetBody != nil,
	}
	if err != nil {
		event["error"] = err.Error()
	}
	if resp != nil {
		event["status"] = resp.StatusCode
	}
	ob.capture.event(event)
	return resp, err
}
