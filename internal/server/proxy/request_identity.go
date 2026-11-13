package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"gpuworks.ai/gpuworks/internal/server/tracecontext"
)

// requestIdentity names one forwarded request: the request id the client
// sees in X-Request-ID and the gateway's span of the request's trace (ADR
// 0046). It is settled once, where every forwarded request passes
// (serveSelectedTarget), so the response header, the request record, the
// engine's traceparent and a PD prefill copy all name the same request.
type requestIdentity struct {
	id    string
	trace tracecontext.Span
	// model is the model name the client asked for.
	model string
}

type identityKeyType struct{}

var identityKey identityKeyType

// Trace context headers (W3C Trace Context).
const (
	traceparentHeader = "Traceparent"
	tracestateHeader  = "Tracestate"
)

// SetTraceSampler wires the sampling decision for traces the gateway starts
// (a request that came without a valid traceparent); a request continuing a
// caller's trace keeps the caller's decision. Without a sampler no root trace
// is sampled. Call it at most once, before the Proxy serves traffic; the
// sampler must be safe for concurrent use and cheap.
func (p *Proxy) SetTraceSampler(sample func(tracecontext.TraceID) bool) {
	p.sampleRoot = sample
}

// identify settles r's identity and returns r carrying it. A request that
// already carries one keeps it, so the later steps of the same request (the
// dispatch after a PD prefill stage) and a direct call that skipped the
// routing entry agree. A probe's own
// trace is never sampled: it leaves no record to export.
func (p *Proxy) identify(r *http.Request, model string) (*http.Request, requestIdentity) {
	if id, ok := r.Context().Value(identityKey).(requestIdentity); ok {
		return r, id
	}
	parent, hasParent := tracecontext.ParseTraceparent(r.Header.Values(traceparentHeader), r.Header.Values(tracestateHeader))
	sample := p.sampleRoot
	if probeFrom(r.Context()) != nil {
		sample = nil
	}
	id := requestIdentity{
		id:    requestID(r),
		trace: tracecontext.Begin(parent, hasParent, time.Now(), sample),
		model: model,
	}
	return r.WithContext(context.WithValue(r.Context(), identityKey, id)), id
}

// setTraceHeaders makes an upstream request carry the gateway's span as the
// engine's parent, replacing whatever the client sent; the caller's
// tracestate goes along unchanged. A context without an identity (the PD
// verifier's own requests) leaves the headers alone.
func setTraceHeaders(ctx context.Context, h http.Header) {
	id, ok := ctx.Value(identityKey).(requestIdentity)
	if !ok {
		return
	}
	h.Set(traceparentHeader, id.trace.Traceparent())
	if id.trace.State != "" {
		h.Set(tracestateHeader, id.trace.State)
	} else {
		h.Del(tracestateHeader)
	}
}

// requestID reuses the chi RequestID middleware value when the proxy is mounted
// behind it, and otherwise mints one so every proxied request is traceable and
// the X-Request-ID response header is always populated.
func requestID(r *http.Request) string {
	if id, ok := r.Context().Value(identityKey).(requestIdentity); ok {
		return id.id
	}
	if id := strings.TrimSpace(middleware.GetReqID(r.Context())); id != "" {
		return id
	}
	if id := strings.TrimSpace(r.Header.Get("X-Request-ID")); id != "" {
		return id
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "proxy-unknown"
	}
	return "proxy-" + hex.EncodeToString(b[:])
}
