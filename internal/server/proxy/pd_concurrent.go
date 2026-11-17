// pd_concurrent.go is the concurrent handoff (pdmode.WireConcurrent, SGLang):
// both instances receive the request at once, paired by a bootstrap address
// and room.

package proxy

import (
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// serveConcurrentPrefill starts the prefill half of a concurrent handoff
// (SGLang): the same request goes to a prefill instance in the background and
// to the decoder in the foreground, both carrying the prefill instance's
// bootstrap address and a room number that pairs them. The decoder's answer
// is the client's. A decoder of such a mode cannot compute a prefill alone,
// so without a prefill instance the request is refused, retryably.
func (p *Proxy) serveConcurrentPrefill(w http.ResponseWriter, r *http.Request, tgtp *target, deploymentID uint) (bool, func()) {
	noop := func() {}
	route := tgtp.pd
	lease := p.pickPrefill(route, tgtp.affinity, tgtp.nodeUUID, nil)
	if lease == nil || lease.c.kvHost == "" || lease.c.kvPort == 0 {
		reason := pdFallbackNoPrefill
		if lease != nil {
			lease.release(outcomeCanceled)
			reason = pdFallbackUnreachable
		}
		p.acctFor(r.Context()).RecordPDFallback(deploymentID, reason)
		p.releaseTarget(*tgtp)
		p.acctFor(r.Context()).RecordImmediate(deploymentID, http.StatusServiceUnavailable, false)
		w.Header().Set("Retry-After", strconv.Itoa(dataLaneUnavailableRetryAfterSeconds))
		writeAPIError(w, r, ErrorPDPrefillUnavailable, map[string]any{"reason": reason})
		return false, noop
	}
	original, err := readRequestBody(r, p.maxBodyBytes)
	if err != nil {
		lease.release(outcomeCanceled)
		return true, noop
	}
	room := rand.Int64N(1 << 62)
	fields := map[string]json.RawMessage{
		"bootstrap_host": mustRaw(lease.c.kvHost),
		"bootstrap_port": mustRaw(lease.c.kvPort),
		"bootstrap_room": mustRaw(room),
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(original, &doc) != nil || doc == nil {
		lease.release(outcomeCanceled)
		return true, noop
	}
	for key, value := range fields {
		doc[key] = value
	}
	doc["stream"] = json.RawMessage("false")
	delete(doc, "stream_options")
	prefillRequest, _ := json.Marshal(doc)
	var cleanups []func()
	for key, value := range fields {
		cleanup, err := setRequestJSONField(r, key, value, true, p.maxBodyBytes)
		if err != nil {
			lease.release(outcomeCanceled)
			for _, c := range cleanups {
				c()
			}
			return true, noop
		}
		cleanups = append(cleanups, cleanup)
	}
	done := p.handoffs.begin(lease.c.instanceUUID)
	tgtp.firstByte = done
	started := time.Now()
	// The gateway does not wait for this stage: the record names the prefill
	// instance and no stage time, and the prefill copy is the gateway span's
	// child (no stage span without a stage time).
	stage := newPDStage(started)
	stage.tried(lease.c.instanceUUID)
	tgtp.stage = stage
	ctx := r.Context()
	go func() {
		status, err := p.postStage(ctx, lease.c, r.URL.Path, prefillRequest, nil)
		reason := ""
		switch {
		case err != nil:
			reason = pdFallbackUnreachable
			lease.release(outcomeConnFailure)
		case status >= 500:
			reason = pdFallbackServerError
			lease.release(outcomeUpstreamUnavailable)
		default:
			lease.release(outcomeOK)
		}
		p.acctFor(r.Context()).RecordPDPrefill(deploymentID, time.Since(started), reason)
	}()
	return true, func() {
		done()
		for _, c := range cleanups {
			c()
		}
	}
}
