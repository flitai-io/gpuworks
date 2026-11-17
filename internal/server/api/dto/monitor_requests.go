package dto

// MonitorRequests is GET /monitor/deployments/{id}/requests: one
// deployment's requests as the gateway saw them - those in flight now
// (view=live) or those finished within the range (view=history).
type MonitorRequests struct {
	// View is "live" or "history".
	View string `json:"view"`
	// AsOf is when the answer was taken, Unix milliseconds.
	AsOf int64 `json:"as_of"`
	// Counts are the live phase counts over every in-flight request, not
	// only the ones listed; null for history.
	Counts *MonitorRequestCounts `json:"counts"`
	// Requests are newest first for history; for live, those waiting for a
	// first token longest first, then thinking, then answering.
	Requests []MonitorRequest `json:"requests"`
	// NextCursor pages history further back; empty when there is no more.
	NextCursor string `json:"next_cursor"`
	// FromMS and ToMS are the history's time range as applied, Unix
	// milliseconds; zero for live.
	FromMS int64 `json:"from_ms,omitempty"`
	ToMS   int64 `json:"to_ms,omitempty"`
	// RangeTotal counts every request the gateway finished in that range,
	// before the filters and the log's sampling; null for live.
	RangeTotal *int64 `json:"range_total,omitempty"`
}

// MonitorRequestCounts are the in-flight requests per phase.
type MonitorRequestCounts struct {
	Waiting   int `json:"waiting"`
	Thinking  int `json:"thinking"`
	Answering int `json:"answering"`
}

// MonitorRequest is one request. Times are milliseconds since the gateway
// received it; a null time is one that did not happen (yet) or was not
// observed, a null count one the engine did not report.
type MonitorRequest struct {
	ID string `json:"id"`
	// TraceID is the request's W3C trace id, 32 hex digits: the caller's
	// trace when it sent a traceparent, else one the gateway started; empty
	// for a request recorded before trace ids were.
	TraceID      string `json:"trace_id"`
	DeploymentID uint   `json:"deployment_id"`
	ReceivedMS   int64  `json:"received_ms"`
	// Phase is waiting, thinking or answering while in flight, done after.
	Phase string `json:"phase"`
	// AgeMS is how long ago the request was received (live only).
	AgeMS        *int64 `json:"age_ms"`
	InstanceUUID string `json:"instance_uuid"`
	APIKeyID     uint   `json:"api_key_id"`
	// APIKeyName is the key's name; empty for none or a deleted key.
	APIKeyName string `json:"api_key_name"`
	// PriorityClass is the priority class the gateway ran the request at;
	// empty for a request recorded before classes, or still in flight.
	PriorityClass string `json:"priority_class"`
	UserID        uint   `json:"user_id"`
	// Source is "api", "playground", "batch" or "evaluation" (a
	// server-origin evaluation's request).
	Source    string `json:"source"`
	Stream    bool   `json:"stream"`
	BodyBytes int64  `json:"body_bytes"`
	Images    int32  `json:"images"`

	DispatchMS    *int32 `json:"dispatch_ms"`
	FirstByteMS   *int32 `json:"first_byte_ms"`
	FirstThinkMS  *int32 `json:"first_think_ms"`
	FirstAnswerMS *int32 `json:"first_answer_ms"`
	LastChunkMS   *int32 `json:"last_chunk_ms"`
	EndMS         *int32 `json:"end_ms"`
	// Events and ThinkEvents count the stream's text events (all, and those
	// carrying reasoning); MaxGapMS is the longest pause between two.
	Events      int32  `json:"events"`
	ThinkEvents int32  `json:"think_events"`
	MaxGapMS    *int32 `json:"max_gap_ms"`

	// Status, Outcome and FinishReason describe how a finished request
	// ended; null/empty while in flight. Outcome is ok, client_closed,
	// upstream_error or truncated.
	Status       *int   `json:"status"`
	Outcome      string `json:"outcome"`
	FinishReason string `json:"finish_reason"`
	// Result is how a finished request turned out, as the history filters
	// by it: completed, length, client_closed, interrupted or error; empty
	// while in flight.
	Result string `json:"result"`

	PromptTokens     *int32 `json:"prompt_tokens"`
	CachedTokens     *int32 `json:"cached_tokens"`
	CompletionTokens *int32 `json:"completion_tokens"`
	ReasoningTokens  *int32 `json:"reasoning_tokens"`

	// The engine's own account of the request's phases.
	EngineQueueMS   *int32 `json:"engine_queue_ms"`
	EnginePrefillMS *int32 `json:"engine_prefill_ms"`
	EngineDecodeMS  *int32 `json:"engine_decode_ms"`

	// TTFTMS is the time from receipt to the first generated output,
	// reasoning included; null for a reply that does not stream, one still
	// waiting for it, and a stream that ended without one.
	TTFTMS *int32 `json:"ttft_ms"`
	// TPOTMS is the decode time per output token, and PrefillTokS and
	// DecodeTokS the prefill and decode speeds (uxrecord.SpeedsOf): decode is
	// the engine's decode time over the output when the engine reports it,
	// else the stream from the first token to the last over the tokens after
	// the first; null when a figure they need is missing, and for
	// a request still in flight.
	TPOTMS      *float64 `json:"tpot_ms"`
	PrefillTokS *float64 `json:"prefill_tok_s"`
	DecodeTokS  *float64 `json:"decode_tok_s"`

	// Weight is how many requests this one stands for in a sampled history.
	Weight int32 `json:"weight"`

	// Segments split the request's time (its end-to-end time, or its age in
	// flight) into the phases it went through, in order
	// (uxrecord.SegmentsOf): the row's bar.
	Segments []MonitorRequestSegment `json:"segments"`

	// PD is a PD disaggregated request's prefill stage; null for any other
	// request. In PD the engine figures above are the decode instance's:
	// its queue includes waiting for the KV, its prefill recomputes the last
	// token.
	PD *MonitorRequestPD `json:"pd"`
}

// MonitorRequestPD is a request's PD prefill stage.
type MonitorRequestPD struct {
	// PrefillInstanceUUID is the prefill instance that took the prefill
	// copy (or, in flight, is computing it); empty when none did.
	PrefillInstanceUUID string `json:"prefill_instance_uuid"`
	// PrefillMS is how long the gateway waited for the stage before handing
	// the request to the decode instance; null when it did not wait
	// (concurrent handoff) or, in flight, the stage is still running.
	PrefillMS *int32 `json:"prefill_ms"`
	// PrefillQueueMS and PrefillComputeMS are the prefill instance's own
	// account of its queue and prefill; null when it gave none.
	PrefillQueueMS   *int32 `json:"prefill_queue_ms"`
	PrefillComputeMS *int32 `json:"prefill_compute_ms"`
	// Fallback is why the request was decoded alone; empty when it was not.
	Fallback string `json:"fallback"`
}

// MonitorRequestSegment is one phase of a request: gateway, queue, prefill,
// waiting, thinking, answering or tail, in milliseconds. Stage is the part of
// the request's time it belongs to: first_token (the segments up to the
// first token, which add up to ttft_ms), decode or tail; empty for a reply
// that does not stream and a stream that ended without a token.
type MonitorRequestSegment struct {
	Kind  string  `json:"kind"`
	MS    float64 `json:"ms"`
	Stage string  `json:"stage"`
}

// MonitorDistributions is GET /monitor/distributions: the shape of the
// requests finished within the range, one deployment's or every one's.
type MonitorDistributions struct {
	FromMS int64 `json:"from_ms"`
	ToMS   int64 `json:"to_ms"`
	// TokenBuckets are the token buckets' lower bounds: bucket i holds
	// [TokenBuckets[i], TokenBuckets[i+1]), the last one everything above.
	TokenBuckets []int64 `json:"token_buckets"`
	// TTFTBoundsMS are the first-token buckets' upper bounds: bucket i holds
	// below TTFTBoundsMS[i] (and from the previous bound), the last bucket
	// everything slower than the last bound.
	TTFTBoundsMS []int32 `json:"ttft_bounds_ms"`
	// InputOutput counts requests by prompt bucket (x) and completion bucket (y).
	InputOutput []MonitorGridCell `json:"input_output"`
	// UncachedTTFT counts requests by uncached prompt bucket (x) and first
	// token bucket (y).
	UncachedTTFT []MonitorGridCell `json:"uncached_ttft"`
	// Coverage says how many requests the grids could place.
	Coverage MonitorDistributionCoverage `json:"coverage"`
}

// MonitorGridCell is one counted cell.
type MonitorGridCell struct {
	X int   `json:"x"`
	Y int   `json:"y"`
	N int64 `json:"n"`
}

// MonitorDistributionCoverage counts the range's requests: all of them,
// those with the engine's usage (in the input/output grid), and those whose
// usage also said what was cached (in the uncached/first-token grid, when
// streamed).
type MonitorDistributionCoverage struct {
	Total     int64 `json:"total"`
	WithUsage int64 `json:"with_usage"`
	WithCache int64 `json:"with_cache"`
}

// MonitorRequestsQuery is the request view's query, as the API parsed it.
type MonitorRequestsQuery struct {
	// View is "live" or "history".
	View    string
	RangeMS int64
	// FromMS and ToMS, when set, replace the range ending now with an
	// explicit one (history only).
	FromMS, ToMS int64
	Limit        int
	Cursor       string
	APIKeyID     uint
	// InstanceUUID narrows to one replica.
	InstanceUUID string
	// PriorityClass narrows history to the requests run at one class.
	PriorityClass string
	// Results, MinEndMS and MaxEndMS filter history by result and by
	// end-to-end time; MinTTFTMS and MaxTTFTMS by the time to the first
	// token, which keeps only streamed replies that had one.
	Results              []string
	MinEndMS, MaxEndMS   int32
	MinTTFTMS, MaxTTFTMS int32
}

// MonitorRequestLookup is GET /monitor/requests/lookup: the finished
// requests carrying a trace id or a request id, across deployments, newest
// first.
type MonitorRequestLookup struct {
	AsOf int64 `json:"as_of"`
	// FromMS and ToMS are the window searched, Unix milliseconds.
	FromMS int64 `json:"from_ms"`
	ToMS   int64 `json:"to_ms"`
	// Window says where the window came from: "requested" (from and to),
	// "trace_time" (the time a gateway-started trace id carries) or
	// "default" (the last hour).
	Window   string           `json:"window"`
	Requests []MonitorRequest `json:"requests"`
	// Deployments name the deployments the requests went to; a deleted
	// deployment is listed with an empty name.
	Deployments []MonitorLookupDeployment `json:"deployments"`
}

// MonitorLookupDeployment names one deployment of a lookup's requests: by
// its served name, a deleted one by the name it left (Deleted set); empty
// for one deleted before deployments' names were kept.
type MonitorLookupDeployment struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

// MonitorRequestLookupQuery is the lookup's query, as the API parsed it:
// one of TraceID and RequestID; FromMS and ToMS together or neither.
type MonitorRequestLookupQuery struct {
	TraceID      string
	RequestID    string
	FromMS, ToMS int64
	Limit        int
}
