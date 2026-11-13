// Package uxrecord defines the per-request record the gateway produces for
// every inference request it forwards, and the bucketing of the request-shape
// distributions built from those records. The gateway (proxy) writes records,
// the request store (uxstore) keeps them and the monitor API reads them, so
// the shape lives here, in a leaf package none of them owns.
//
// Times are milliseconds since the moment the gateway received the request;
// Unknown (-1) marks a moment that did not happen or was not observed.
package uxrecord

import (
	"context"
	"math"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tracecontext"
)

// Unknown marks a time or count that was not observed.
const Unknown int32 = -1

// MaxLookupWindow bounds the time a lookup by trace id or request id scans
// (uxstore.Lookup): neither id is indexed, so the window keeps a lookup
// cheap. The monitor API chooses windows within it.
const MaxLookupWindow = 24 * time.Hour

// Source is the entry a request came through.
type Source uint8

const (
	// SourceAPI is the public OpenAI-compatible /v1 entry (API key auth).
	SourceAPI Source = 0
	// SourcePlayground is the management console's playground.
	SourcePlayground Source = 1
	// SourceBatch is a batch's request, sent by the batch executor for a
	// batch created through /v1/batches or the console.
	SourceBatch Source = 2
)

// String is the source's wire name: api, playground or batch.
func (s Source) String() string {
	switch s {
	case SourcePlayground:
		return "playground"
	case SourceBatch:
		return "batch"
	}
	return "api"
}

// Outcome is how the reply ended, from the gateway's side.
type Outcome uint8

const (
	// OutcomeOK is a reply copied to the client whole.
	OutcomeOK Outcome = 0
	// OutcomeClientClosed is a reply the client stopped reading before its end.
	OutcomeClientClosed Outcome = 1
	// OutcomeUpstreamError is a request the instance never answered.
	OutcomeUpstreamError Outcome = 2
	// OutcomeTruncated is a reply the upstream cut short.
	OutcomeTruncated Outcome = 3
)

// String is the outcome's wire name: ok, client_closed, upstream_error or
// truncated; "" for a value not listed.
func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeClientClosed:
		return "client_closed"
	case OutcomeUpstreamError:
		return "upstream_error"
	case OutcomeTruncated:
		return "truncated"
	}
	return ""
}

// Request is one forwarded inference request.
type Request struct {
	// ID is the gateway request id (the X-Request-ID the client saw).
	ID string
	// Trace is the gateway's span of the request (W3C trace context, ADR
	// 0046). The request store keeps its trace id; the span id, the
	// caller's span, the sampling decision and the tracestate only travel
	// in memory, to the trace exporter.
	Trace tracecontext.Span
	// Model is the model name the client asked for; in memory only, for the
	// trace exporter (the store has the deployment).
	Model string
	// Sampling are the sampling parameters the client's request set; in
	// memory only, for the trace exporter.
	Sampling Sampling
	// ReceivedMS is when the gateway received the request, Unix milliseconds.
	ReceivedMS   int64
	DeploymentID uint
	InstanceUUID string
	// APIKeyID and UserID identify the caller; zero when unknown (the
	// playground's APIKeyID, or a keyless request the gateway accepted because
	// open access is on).
	APIKeyID uint
	UserID   uint
	Source   Source
	// Path is the gateway request path, for example /v1/chat/completions.
	Path   string
	Stream bool
	// BodyBytes is the request body size; Images the image parts it carried.
	BodyBytes int64
	Images    int32

	// Timeline, milliseconds since ReceivedMS.
	DispatchMS    int32 // handed to the instance
	FirstByteMS   int32 // first upstream byte
	FirstThinkMS  int32 // first non-empty reasoning delta
	FirstAnswerMS int32 // first non-empty content or tool call delta
	LastChunkMS   int32 // last stream event
	EndMS         int32 // reply finished (or the client went away)
	// Events counts stream data events, ThinkEvents those carrying reasoning.
	Events      int32
	ThinkEvents int32
	// MaxGapMS is the longest pause between two stream events.
	MaxGapMS int32

	Status       int
	Outcome      Outcome
	FinishReason string

	// Usage reports whether the reply carried the engine's usage; the token
	// counts are meaningful only when it did.
	Usage            bool
	PromptTokens     int32
	CompletionTokens int32
	// CachedTokens is Unknown when the engine reported no cache figure.
	CachedTokens    int32
	ReasoningTokens int32

	// Engine-reported phases of this request, Unknown when not reported.
	EngineQueueMS   int32
	EnginePrefillMS int32
	EngineDecodeMS  int32

	// PD is the prefill stage of a PD disaggregated request (ADR 0028,
	// ADR 0047); nil for a request that was not disaggregated. In PD the
	// engine figures above are the decode instance's: its queue includes
	// waiting for the KV to arrive, its prefill recomputes the last token.
	PD *PDStage

	// Weight is how many requests this record stands for: 1, or the sampling
	// ratio when the store kept one in Weight normal requests.
	Weight int32
}

// FirstTokenMS is the time to the first generated token of any kind, or
// Unknown.
func (r *Request) FirstTokenMS() int32 {
	switch {
	case r.FirstThinkMS >= 0 && r.FirstAnswerMS >= 0:
		return min(r.FirstThinkMS, r.FirstAnswerMS)
	case r.FirstThinkMS >= 0:
		return r.FirstThinkMS
	default:
		return r.FirstAnswerMS
	}
}

// UncachedTokens is the prompt the engine had to compute, or Unknown when the
// cache figure or usage is missing.
func (r *Request) UncachedTokens() int32 {
	if !r.Usage || r.CachedTokens < 0 {
		return Unknown
	}
	return max(0, r.PromptTokens-r.CachedTokens)
}

// UncachedComputeMS is the engine time that computed the uncached prompt, or
// Unknown: the prefill instance's own prefill for a PD handoff (the decode
// instance only recomputes the last token, and its cache figure is the
// prefill instance's), the engine's reported prefill otherwise.
func (r *Request) UncachedComputeMS() int32 {
	if pd := r.PD; pd != nil && pd.Fallback == "" {
		return pd.PrefillComputeMS
	}
	return r.EnginePrefillMS
}

// Grid kinds: the two request-shape distributions.
const (
	// GridIO buckets prompt tokens (x) against completion tokens (y).
	GridIO = "io"
	// GridUncachedTTFT buckets uncached prompt tokens (x) against the time to
	// the first token (y).
	GridUncachedTTFT = "ut"
)

// TokenBuckets is the number of token buckets: bucket 0 holds 0..1 tokens,
// bucket b (b >= 1) holds [2^b, 2^(b+1)), and the last bucket everything from
// 2^(TokenBuckets-1) up (a 1M-token context lands in it).
const TokenBuckets = 21

// TokenBucket returns the bucket of a token count.
func TokenBucket(tokens int32) int {
	if tokens < 2 {
		return 0
	}
	b := int(math.Floor(math.Log2(float64(tokens))))
	return min(b, TokenBuckets-1)
}

// TokenBucketLow is the smallest token count in bucket b.
func TokenBucketLow(b int) int64 {
	if b <= 0 {
		return 0
	}
	return int64(1) << b
}

// TTFTBoundsMS are the upper bounds of the time-to-first-token buckets; the
// last bucket is everything slower than the last bound.
var TTFTBoundsMS = []int32{100, 250, 500, 1000, 2000, 5000, 10000, 30000, 60000, 120000, 300000}

// TTFTBuckets is the number of time-to-first-token buckets.
var TTFTBuckets = len(TTFTBoundsMS) + 1

// TTFTBucket returns the bucket of a time to first token.
func TTFTBucket(ms int32) int {
	for i, bound := range TTFTBoundsMS {
		if ms < bound {
			return i
		}
	}
	return len(TTFTBoundsMS)
}

// Cell is one counted cell of a grid.
type Cell struct {
	X, Y int
	N    int64
}

// Cells are the grid cells one request adds to. A request without usage adds
// to neither grid; one without a cache figure or a first token adds to the
// input/output grid only.
type Cells struct {
	IO, UncachedTTFT     Cell
	HasIO, HasUncachedTT bool
}

// GridCells returns the cells a request adds to.
func GridCells(r *Request) Cells {
	var out Cells
	if !r.Usage {
		return out
	}
	out.IO, out.HasIO = Cell{X: TokenBucket(r.PromptTokens), Y: TokenBucket(r.CompletionTokens), N: 1}, true
	if u, ft := r.UncachedTokens(), r.FirstTokenMS(); u >= 0 && ft >= 0 {
		out.UncachedTTFT, out.HasUncachedTT = Cell{X: TokenBucket(u), Y: TTFTBucket(ft), N: 1}, true
	}
	return out
}

// PDStage is what the gateway knows of a PD request's prefill stage.
type PDStage struct {
	// PrefillInstance is the prefill instance that took the prefill copy;
	// "" when none did (the request was decoded alone).
	PrefillInstance string
	// PrefillMS is how long the gateway waited for the prefill stage before
	// it handed the request to the decode instance (sequential handoff:
	// the prefill copy, its round trip, a retry on another prefill
	// instance); Unknown when it did not wait (concurrent handoff).
	PrefillMS int32
	// PrefillQueueMS and PrefillComputeMS are the prefill instance's own
	// account of its queue and prefill in its reply; Unknown when it gave
	// none.
	PrefillQueueMS, PrefillComputeMS int32
	// Fallback is why the request was decoded alone, the gateway's fallback
	// reason (prefill_unreachable, prefill_5xx, ...); "" when it was not.
	Fallback string
	// SpanID is the stage's own span, the prefill copy's parent in the
	// trace, and StartMS when the stage began (milliseconds since the
	// request was received); in memory only, for the trace exporter.
	SpanID  tracecontext.SpanID
	StartMS int32
}

// Sampling are the sampling parameters a request set at its top level, as
// the client sent them (the engine's defaults fill in the rest). A Has flag
// says the value was there; the zero value is none set.
type Sampling struct {
	// MaxTokens is max_completion_tokens, or max_tokens without it.
	MaxTokens                    int32
	Temperature, TopP            float64
	N                            int32
	HasMaxTokens, HasTemperature bool
	HasTopP, HasN                bool
}

// Caller is who sent a request and when the gateway received it. The entry
// that authenticated the request puts it on the request context; the gateway
// reads it back when it writes the request's record.
type Caller struct {
	APIKeyID uint
	UserID   uint
	Source   Source
	// ReceivedMS is when the entry received the request, Unix milliseconds;
	// zero when unknown (the gateway then uses its own dispatch time).
	ReceivedMS int64
}

type callerKey struct{}

// WithCaller returns ctx carrying the caller.
func WithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}

// CallerFrom returns the caller ctx carries.
func CallerFrom(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerKey{}).(Caller)
	return caller, ok
}
