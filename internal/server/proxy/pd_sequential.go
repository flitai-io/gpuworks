// pd_sequential.go is the sequential handoff (pdmode.WireSequential): the
// prefill instance answers a one-token copy of the request, and the decode
// instance receives the kv_transfer_params it returned.

package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// maxPrefillResponseBytes bounds the prefill instance's one-token answer.
const maxPrefillResponseBytes = 16 << 20

// prefillBody is the prefill instance's copy of the request: one output
// token, not streamed, and asked to keep the KV for a remote decoder. Fields
// that only make sense for the real generation go. transferID, when set, is
// the id both sides of a synthesized handoff name the KV by
// (pdmode.Mode.SynthesizedHandoff).
func prefillBody(original []byte, transferID string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(original, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("request body is not a JSON object")
	}
	one := json.RawMessage("1")
	doc["max_tokens"] = one
	if _, ok := doc["max_completion_tokens"]; ok {
		doc["max_completion_tokens"] = one
	}
	doc["stream"] = json.RawMessage("false")
	for _, key := range []string{"stream_options", "min_tokens", "min_completion_tokens"} {
		delete(doc, key)
	}
	params := map[string]any{"do_remote_decode": true}
	if transferID != "" {
		params["transfer_id"] = transferID
	}
	doc[kvTransferField] = mustRaw(params)
	return json.Marshal(doc)
}

// newTransferID is a fresh id for one synthesized handoff.
func newTransferID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "gw-" + hex.EncodeToString(b[:])
}

// synthesizedHandoff is the decoder's kv_transfer_params of a mode whose
// prefill answer carries none (vLLM's MooncakeConnector): the prefill
// instance named by its bootstrap server (its KV endpoint, at the node
// address peers use) and the engine id its workers registered there
// (bootstrapEngineID), and the transfer id the prefill copy carried. Nil when
// any of them is missing.
func synthesizedHandoff(c candidate, engineID, transferID string) json.RawMessage {
	if c.kvHost == "" || c.kvPort == 0 || engineID == "" || transferID == "" {
		return nil
	}
	return mustRaw(map[string]any{
		"do_remote_prefill":     true,
		"remote_engine_id":      engineID,
		"remote_bootstrap_addr": "http://" + net.JoinHostPort(c.kvHost, strconv.Itoa(c.kvPort)),
		"transfer_id":           transferID,
	})
}

// prefillTransferID is the transfer id a prefill copy carries, if any.
func prefillTransferID(body []byte) string {
	var doc struct {
		Params struct {
			TransferID string `json:"transfer_id"`
		} `json:"kv_transfer_params"`
	}
	_ = json.Unmarshal(body, &doc)
	return doc.Params.TransferID
}

// prefillOutcome is how one prefill call ended.
type prefillOutcome struct {
	prefill  string          // the prefill instance that answered
	params   json.RawMessage // set on success
	status   int             // upstream status, 0 when none arrived
	body     []byte          // upstream body for a 4xx the client gets
	header   http.Header
	fallback string // reason to fall back, empty on success or 4xx
}

// callPrefill sends the prefill copy to one instance over the same tunnel or
// serving path the decode request uses. A request's prefill copy carries its
// stage's span as the trace parent; the verifier's own probes (no stage)
// carry none.
func (p *Proxy) callPrefill(ctx context.Context, c candidate, path string, body []byte, stage *pdStage) (*http.Response, error) {
	ctx = context.WithValue(ctx, targetKey, target{nodeUUID: c.nodeUUID, port: c.port, modelName: c.modelName, instanceUUID: c.instanceUUID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", c.port, path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	setPrefillTraceHeaders(ctx, req.Header, stage)
	return p.transport.RoundTrip(req)
}

// runPrefill finds a prefill instance that answers, trying the next one when
// a connection fails, and classifies the answer.
func (p *Proxy) runPrefill(ctx context.Context, route *pdRoute, affinity, decodeNode, path string, body []byte, stage *pdStage, reading replyReading) prefillOutcome {
	excluded := map[string]bool{}
	// unreachable is the reason to give when the attempts run out: a
	// bootstrap server that never had every rank registered, or a connection.
	unreachable := pdFallbackUnreachable
	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		l := p.pickPrefill(route, affinity, decodeNode, excluded, admissionFrom(ctx))
		if l == nil {
			if attempt == 0 {
				return prefillOutcome{fallback: pdFallbackNoPrefill}
			}
			return prefillOutcome{fallback: unreachable}
		}
		excluded[l.c.instanceUUID] = true
		stage.tried(l.c.instanceUUID)
		var engineID string
		if route.mode.SynthesizedHandoff {
			// Ask before the prefill copy: an instance the decoder cannot
			// find would keep the KV for nobody until its lease expires.
			id, err := p.bootstrapEngineID(ctx, l.c, route.placement.TP(pdmode.RolePrefill))
			if err != nil {
				if ctx.Err() != nil {
					l.release(outcomeCanceled)
					return prefillOutcome{fallback: pdFallbackPrefillFailed}
				}
				p.log.Warn("prefill instance bootstrap server not usable", "instance", l.c.instanceUUID, "node", l.c.nodeUUID, "err", err)
				l.release(outcomeConnFailure)
				unreachable = pdFallbackNoBootstrap
				continue
			}
			engineID = id
		}
		resp, err := p.callPrefill(ctx, l.c, path, body, stage)
		if err != nil {
			if ctx.Err() != nil {
				l.release(outcomeCanceled)
				return prefillOutcome{fallback: pdFallbackPrefillFailed}
			}
			l.release(outcomeConnFailure)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxPrefillResponseBytes))
		_ = resp.Body.Close()
		switch {
		case readErr != nil:
			l.release(outcomeConnFailure)
			return prefillOutcome{status: resp.StatusCode, fallback: pdFallbackPrefillFailed}
		case resp.StatusCode >= 500:
			l.release(outcomeUpstreamUnavailable)
			return prefillOutcome{status: resp.StatusCode, fallback: pdFallbackServerError}
		case resp.StatusCode >= 400:
			// The request itself is wrong; the decoder would say the same.
			l.release(outcomeOK)
			return prefillOutcome{status: resp.StatusCode, body: data, header: resp.Header}
		}
		var answer struct {
			Params json.RawMessage            `json:"kv_transfer_params"`
			Usage  map[string]json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(data, &answer)
		params := bytes.TrimSpace(answer.Params)
		if route.mode.SynthesizedHandoff {
			params = synthesizedHandoff(l.c, engineID, prefillTransferID(body))
		}
		if len(params) == 0 || bytes.Equal(params, []byte("null")) || params[0] != '{' {
			// 200 without the handoff: the engine is not running as a PD
			// producer (a replaced connector, an image without it). Count it
			// against the instance so a broken one cools down.
			p.log.Warn("prefill instance answered without kv_transfer_params", "instance", l.c.instanceUUID, "node", l.c.nodeUUID)
			l.release(outcomeUpstreamUnavailable)
			return prefillOutcome{status: resp.StatusCode, fallback: pdFallbackNoKVParams}
		}
		l.release(outcomeOK)
		var cached uint64
		var cachedKnown bool
		if shape, ok := inferenceapi.UsageForPath(path); ok && answer.Usage != nil {
			cached, cachedKnown = cacheFigure(answer.Usage, shape, reading)
		}
		stage.answered(l.c.instanceUUID, requestStats(data, reading.rules), cached, cachedKnown)
		return prefillOutcome{prefill: l.c.instanceUUID, status: resp.StatusCode, params: append(json.RawMessage(nil), params...)}
	}
	return prefillOutcome{fallback: unreachable}
}

// serveSequentialPrefill is the prefill stage of a sequential handoff
// (pdmode.WireSequential): the prefill copy first, then the decode request
// carries the kv_transfer_params it answered with.
func (p *Proxy) serveSequentialPrefill(w http.ResponseWriter, r *http.Request, tgtp *target, deploymentID uint) (bool, func()) {
	noop := func() {}
	tgt := *tgtp
	route := tgt.pd
	started := time.Now()
	original, err := readRequestBody(r, p.maxBodyBytes)
	if err != nil {
		// Unreadable here is unreadable for the decoder too; it answers.
		return true, noop
	}
	transferID := ""
	if route.mode.SynthesizedHandoff {
		transferID = newTransferID()
	}
	body, err := prefillBody(original, transferID)
	if err != nil {
		return true, noop
	}
	// The request is in flight from here: the stage shows in the live table
	// before any decode instance has it.
	stage := newPDStage(started)
	tgtp.stage = stage
	if probeFrom(r.Context()) == nil {
		_, ident := p.identify(r, "")
		tgtp.live = newLiveRequest(r, ident, deploymentID, stage, started)
		p.live.add(tgtp.live)
	}
	reading, _ := p.routes.observationFor(deploymentID)
	outcome := p.runPrefill(r.Context(), route, tgt.affinity, tgt.nodeUUID, r.URL.Path, body, stage, reading)
	stage.done(time.Now())
	if outcome.fallback != "" {
		stage.fellBack(outcome.fallback)
	}
	p.acctFor(r.Context()).RecordPDPrefill(deploymentID, time.Since(started), outcome.fallback)
	if r.Context().Err() != nil {
		p.releaseTarget(tgt)
		return false, noop
	}
	if outcome.params != nil {
		cleanup, err := setRequestJSONField(r, kvTransferField, outcome.params, true, p.maxBodyBytes)
		if err != nil {
			p.acctFor(r.Context()).RecordPDFallback(deploymentID, pdFallbackPrefillFailed)
			return true, noop
		}
		done := p.handoffs.begin(outcome.prefill)
		tgtp.firstByte = done
		return true, func() { done(); cleanup() }
	}
	if outcome.fallback == "" {
		// A 4xx from the prefill instance goes back to the client as it came.
		p.releaseTarget(tgt)
		p.acctFor(r.Context()).RecordImmediate(deploymentID, outcome.status, false)
		for _, h := range []string{"Content-Type"} {
			if v := outcome.header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.Header().Set("X-Request-ID", requestID(r))
		w.WriteHeader(outcome.status)
		_, _ = w.Write(outcome.body)
		return false, noop
	}
	p.acctFor(r.Context()).RecordPDFallback(deploymentID, outcome.fallback)
	if route.fallback == pdmode.FallbackFail || !route.mode.DecodeServesAlone {
		p.releaseTarget(tgt)
		p.acctFor(r.Context()).RecordImmediate(deploymentID, http.StatusServiceUnavailable, false)
		w.Header().Set("Retry-After", strconv.Itoa(dataLaneUnavailableRetryAfterSeconds))
		writeAPIError(w, r, ErrorPDPrefillUnavailable, map[string]any{"reason": outcome.fallback})
		return false, noop
	}
	// Fall back: the decoder computes the prefill itself. The body already
	// carries no client kv_transfer_params.
	return true, noop
}
