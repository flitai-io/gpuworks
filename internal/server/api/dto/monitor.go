package dto

// Point is one (unix ms, value) sample on the wire, encoded as a [t, v]
// two-element JSON array rather than an object - the shape every /monitor
// chart and sparkline consumes (plan artifacts/archive/2026-09-19-monitor-center/plan.md
// §6). The timestamp travels as float64 like the value: unix-millisecond
// timestamps for the lifetime of this product stay well inside float64's
// 53-bit exact-integer range, and a fixed-shape array cannot mix element
// types.
type Point [2]float64

// MonitorSignal is one cell of the cluster overview's signal row. Collected
// is false for a metric this stage of the product cannot compute yet (no
// proxy accounting, no engine histograms) - the frontend renders a muted
// dash instead of a misleading zero, and Value/Denominator/Detail/Spark are
// all absent from the wire in that case.
//
// Collected is a distinct question from "did anything happen just now":
// error_rate/request_rate/preemptions/waiting/token_rate/ttft/tpot are all
// driven by metric families the agent/proxy only emit samples for when
// something actually happens in that tick (a proxy request, a completed
// engine request) - see internal/server/proxy/accounting.go's flush and
// internal/server/obsingest/quantile.go's applyQuantiles. A
// deployment that served heavy traffic ten minutes ago but nothing in the
// last five minutes therefore has real data in a wider chart range but none
// in the fixed diagnosis window monitor.BuildCluster reduces signals from.
// Collected is true whenever the family exists anywhere in the range the
// caller is looking at, even when the diagnosis window itself is quiet; in
// that "quiet window" state, Detail carries {"no_recent_requests": true} in
// addition to any other detail keys (error_rate keeps its normal 5xx/429/
// asleep_503 breakdown, all zero) so the frontend can show a "quiet, not
// broken" state rather than either a misleading dash or an unexplained
// zero. request_rate/error_rate/preemptions/waiting/token_rate report a
// real Value of 0 in this state (a true statement: no traffic happened);
// ttft/tpot instead omit Value/Denominator entirely even though Collected
// is true, since a p95 of "0ms" would misreport an instant response rather
// than "no request completed this window" - only Detail carries the
// no_recent_requests flag (alongside the upper_bound flag below) for those
// two.
//
// no_recent_requests means specifically "no samples in the five-minute
// window", not "the window's own value happened to be zero/flat". A
// deployment with, say, two proxy samples inside the window whose counters
// simply did not increase between them (traffic paused mid-window, or a
// steady non-2xx-free stretch) is collected=true with Value 0 the ordinary
// way - the window itself answered the question, it just answered "zero" -
// and carries no no_recent_requests marker at all. The marker is reserved
// for the case where the window has no sample to answer from in the first
// place and BuildCluster had to fall back to the wider chart range to even
// know the family exists.
//
// For the "ttft" and "tpot" keys specifically, Value/Denominator (p95/p99),
// when present, are each the max, over the window, of the per-tick quantile
// gauge the phase2 ingest pipeline writes - a conservative upper bound on
// the true percentile over raw samples, not an exact one (see the ingest
// pipeline's own "Quantile-at-ingest" contract). Detail carries
// {"upper_bound": true} on those two signals for exactly this reason, so
// the UI can label the number instead of presenting it as an exact
// statistic.
//
// For "request_rate", Denominator is the gateway's average requests in
// flight over the same window (the concurrency the rate was served at),
// absent when no deployment reports the in-flight gauge.
type MonitorSignal struct {
	Key         string         `json:"key"`
	Collected   bool           `json:"collected"`
	Value       *float64       `json:"value,omitempty"`
	Denominator *float64       `json:"denominator,omitempty"`
	Detail      map[string]any `json:"detail,omitempty"`
	Spark       []Point        `json:"spark,omitempty"`
}

// MonitorSeries is one labeled line of a chart or the /monitor/series
// browser. Labels is never null on the wire (an unlabeled/combined series
// still carries an empty object) so the frontend never has to nil-check it.
type MonitorSeries struct {
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

// MonitorReplicaCount is a deployment's ready/desired replica count, used by
// both the cluster overview's service row and the service-layer summary.
type MonitorReplicaCount struct {
	Ready   int `json:"ready"`
	Desired int `json:"desired"`
}

// MonitorServiceRow is one row of the cluster overview's service table.
// Every *float64 field is nil - never 0 - when the underlying metric was not
// collected for this deployment (see MonitorSignal's doc comment for why).
type MonitorServiceRow struct {
	DeploymentID   uint                `json:"deployment_id"`
	Name           string              `json:"name"`
	Engine         string              `json:"engine"`
	Placement      string              `json:"placement"`
	Replicas       MonitorReplicaCount `json:"replicas"`
	State          string              `json:"state"`
	RequestRate    *float64            `json:"request_rate"`
	TTFTP95Ms      *float64            `json:"ttft_p95_ms"`
	Running        *float64            `json:"running"`
	Waiting        *float64            `json:"waiting"`
	KVUsagePct     *float64            `json:"kv_usage_pct"`
	KVHitRatePct   *float64            `json:"kv_hit_rate_pct"`
	PromptTokS     *float64            `json:"prompt_tok_s"`
	GenerationTokS *float64            `json:"generation_tok_s"`
	// Health is the deployment's health check.
	Health DeploymentHealth `json:"health"`
}

// MonitorVRAMNode is one row of the cluster overview's per-node VRAM bar.
type MonitorVRAMNode struct {
	NodeUUID       string `json:"node_uuid"`
	Name           string `json:"name"`
	GPUCount       int    `json:"gpu_count"`
	TotalBytes     int64  `json:"total_bytes"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	UsedBytes      *int64 `json:"used_bytes"`
	UsedAsOf       *int64 `json:"used_as_of"`
}

// MonitorClusterTokens is the cluster overview's tok/s chart: separate
// prompt (input) and generation (output) throughput series.
type MonitorClusterTokens struct {
	Prompt     MonitorSeries `json:"prompt"`
	Generation MonitorSeries `json:"generation"`
}

// MonitorEvent is one capacity-changing marker to annotate the cluster
// tok/s chart with (plan §3.1: only instance running/failed/stopped
// transitions, sleep/wake and node online/offline - progress events never
// appear here).
type MonitorEvent struct {
	TimeMS       int64  `json:"time_ms"`
	Kind         string `json:"kind"`
	DeploymentID uint   `json:"deployment_id"`
	InstanceUUID string `json:"instance_uuid"`
	Label        string `json:"label"`
}

// MonitorStateCount is how many services[] rows are in one state.
type MonitorStateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

// MonitorOverview is the GET /api/v1/monitor/overview response.
type MonitorOverview struct {
	AsOf     int64               `json:"as_of"`
	Stale    bool                `json:"stale"`
	Signals  []MonitorSignal     `json:"signals"`
	Services []MonitorServiceRow `json:"services"`
	// StateCounts counts services[] by state: every state of the
	// services[].state vocabulary, in the order the state summary lists
	// them (serving first, then the ones that need a look, then the
	// stopped), zero counts included.
	StateCounts   []MonitorStateCount  `json:"state_counts"`
	VRAMByNode    []MonitorVRAMNode    `json:"vram_by_node"`
	ClusterTokens MonitorClusterTokens `json:"cluster_tokens"`
	Events        []MonitorEvent       `json:"events"`
}

// MonitorDeploymentSummary is the /monitor/deployments/{id} response's
// breadcrumb/subtitle data.
type MonitorDeploymentSummary struct {
	ID        uint                `json:"id"`
	Name      string              `json:"name"`
	Engine    string              `json:"engine"`
	Placement string              `json:"placement"`
	Replicas  MonitorReplicaCount `json:"replicas"`
}

// MonitorAlignment is the service-layer proxy/engine request-count
// cross-check (plan §3.2). Nil until proxy accounting exists.
//
// ProxyRequests, ClientErrors and ServerErrors are each an exact cumulative
// counter delta over the diagnosis window (first vs last sample, walked
// point-to-point and reset-aware - see monitor.SumSeriesDelta), not a rate
// multiplied by the window's length: a smoothed instantaneous rate and the
// counter's true growth over the same span can disagree by more than
// rounding, which would make Lost noisy for reasons that have nothing to do
// with a real proxy/engine mismatch.
//
// ProxyRequests, ClientErrors and ServerErrors ARE a partition of each
// other (proxy/accounting.go's "class" label is exhaustive: every finished
// request is exactly one of 2xx/4xx/429/5xx/asleep_503), but
// EngineCompleted and InFlight are not part of that partition:
//
//   - EngineCompleted comes from an independent counter (the engine's own
//     gpuworks_infer_requests_success_total, not derived from
//     ProxyRequests or the other fields), so it does not sum with
//     ClientErrors/ServerErrors to exactly ProxyRequests - a genuine
//     mismatch between what the proxy recorded finishing and what the
//     engine reports completing is exactly what makes Lost nonzero, rather
//     than being algebraically impossible.
//   - InFlight is NOT part of ProxyRequests at all: proxy/accounting.go
//     only increments gpuworks_proxy_requests_total at Finish or
//     RecordImmediate, i.e. once a request has already resolved to one of
//     its classes. A request still in flight has therefore not yet been
//     counted by ProxyRequests (or EngineCompleted) either - it is purely
//     a displayed figure (how many requests were open at the moment this
//     window ended), never subtracted from Lost. See DiscrepancyPersistent
//     for how it is instead used as an upper bound.
type MonitorAlignment struct {
	ProxyRequests int64 `json:"proxy_requests"`
	// EngineCompleted is derived from the engine's own
	// gpuworks_infer_requests_success_total counter (its own cumulative
	// delta over the window, the same reset-aware treatment ProxyRequests
	// gets - see monitor.SumSeriesDelta), not from ProxyRequests minus the
	// other fields - so a genuine mismatch between what the proxy
	// dispatched and what the engine reports completing is exactly what
	// makes Lost nonzero, rather than being algebraically impossible.
	EngineCompleted int64 `json:"engine_completed"`
	// ClientErrors is the client's own fault: 4xx (bad request, unknown
	// model, ...) plus 429 (rate limited) - never 5xx or asleep_503, which
	// are ServerErrors instead.
	ClientErrors int64 `json:"client_errors"`
	// ServerErrors is the server's fault: 5xx plus the "model is asleep"
	// 503 (asleep_503) - a wake-then-retry case the client did nothing
	// wrong to trigger, so it is not counted as a ClientError even though
	// the client did receive an error response.
	ServerErrors int64 `json:"server_errors"`
	// InFlight is the proxy's in-flight request gauge
	// (gpuworks_proxy_requests_inflight) sampled at the end of the window -
	// requests the proxy has dispatched that are still open (most commonly
	// a long generation still streaming). It is purely informational: NOT
	// subtracted from Lost (see MonitorAlignment's own doc comment for
	// why), only used as the upper bound DiscrepancyPersistent compares
	// Lost against.
	InFlight int64 `json:"in_flight"`
	// Lost is max(0, proxy_requests - engine_completed - client_errors -
	// server_errors): requests the proxy recorded as finished that neither
	// the engine's own success counter nor a classified error response
	// accounts for. InFlight plays no part in this formula.
	Lost int64 `json:"lost"`
	// DiscrepancyPersistent is true only when the in-flight gauge was
	// collected at all AND Lost exceeds the largest in-flight count
	// observed anywhere in the window (see monitor.Window.InflightMax) -
	// i.e. even crediting every concurrently open request at its busiest
	// moment, the gap still would not close, so it cannot be dismissed as
	// "some of those are just still streaming". The client only labels
	// this row "lost" when this is true; otherwise it shows "in flight N"
	// instead. Always false when no in-flight series exists at all (an old
	// agent/proxy, or truly no data) - there is nothing to compare Lost
	// against.
	DiscrepancyPersistent bool `json:"discrepancy_persistent"`
}

// MonitorTTFTBreakdown splits TTFT into its queue/prefill/first-token
// components (plan §3.2). Nil until the engine timing histograms exist.
//
// Every millisecond value here is its own metric's p95 - the max, across
// the window, of the per-tick quantile gauge the phase2 ingest pipeline
// writes (see monitor.Window's doc comment) - not three components of one
// traced request. QueueMs, PrefillMs and FirstTokenMs are therefore each
// individually a conservative upper bound, and their sum is not guaranteed
// to equal TotalMs (TTFT's own p95) even though the product presents them
// as if it were a breakdown of it.
type MonitorTTFTBreakdown struct {
	QueueMs      float64 `json:"queue_ms"`
	PrefillMs    float64 `json:"prefill_ms"`
	FirstTokenMs float64 `json:"first_token_ms"`
	TotalMs      float64 `json:"total_ms"`
	// Approximate is true whenever the three segments needed adjusting to
	// actually sum to TotalMs: QueueMs and PrefillMs were scaled down
	// proportionally because their sum alone exceeded TotalMs, and/or
	// FirstTokenMs was clamped to 0 because queue+prefill (after that same
	// scaling) still left no room for it. The UI shows this as a
	// "approximate" label rather than presenting the bar as an exact
	// decomposition.
	Approximate bool `json:"approximate"`
}

// MonitorCharts is the service layer's fixed set of named chart panels.
// Every field is always present as a (possibly empty) list, never null -
// "latency", "stage_time" and "preemptions" are empty at this stage because
// the histograms/counters that would feed them do not exist yet (plan §8).
type MonitorCharts struct {
	// Latency is TTFT (p50/p95/p99) and end-to-end p95; TPOT is its own group.
	Latency     []MonitorSeries `json:"latency"`
	TPOT        []MonitorSeries `json:"tpot"`
	Concurrency []MonitorSeries `json:"concurrency"`
	KV          []MonitorSeries `json:"kv"`
	Throughput  []MonitorSeries `json:"throughput"`
	StageTime   []MonitorSeries `json:"stage_time"`
	Preemptions []MonitorSeries `json:"preemptions"`
}

// MonitorReplicaGPU is one GPU row within a service layer replica. Every
// value comes from the node's collector under the {node, gpu} labels, and a
// null means that card did not report the metric (an old agent, a card
// without the sensor) - never "0": a card drawing no power and a card whose
// power is unknown are different facts.
//
// Utilization, memory bandwidth and power are each the card's mean over the
// diagnosis window; TemperatureC is the window's maximum, since a transient
// hot reading is the thing worth surfacing.
type MonitorReplicaGPU struct {
	Index        int      `json:"index"`
	UtilPct      *float64 `json:"util_pct"`
	MemBWPct     *float64 `json:"mem_bw_pct"`
	TemperatureC *float64 `json:"temperature_c"`
	PowerW       *float64 `json:"power_w"`
}

// MonitorReplicaRow is one row of the service layer's replica table.
type MonitorReplicaRow struct {
	InstanceUUID   string              `json:"instance_uuid"`
	Role           string              `json:"role,omitempty"`
	State          string              `json:"state"`
	NodeName       string              `json:"node_name"`
	GPUIndexes     []int               `json:"gpu_indexes"`
	Running        *float64            `json:"running"`
	Waiting        *float64            `json:"waiting"`
	KVUsagePct     *float64            `json:"kv_usage_pct"`
	KVHitRatePct   *float64            `json:"kv_hit_rate_pct"`
	PromptTokS     *float64            `json:"prompt_tok_s"`
	GenerationTokS *float64            `json:"generation_tok_s"`
	GPUs           []MonitorReplicaGPU `json:"gpus"`
}

// MonitorDeployment is the GET /api/v1/monitor/deployments/{id} response.
type MonitorDeployment struct {
	AsOf          int64                    `json:"as_of"`
	Stale         bool                     `json:"stale"`
	Deployment    MonitorDeploymentSummary `json:"deployment"`
	Alignment     *MonitorAlignment        `json:"alignment"`
	TTFTBreakdown *MonitorTTFTBreakdown    `json:"ttft_breakdown"`
	Charts        MonitorCharts            `json:"charts"`
	Replicas      []MonitorReplicaRow      `json:"replicas"`
	// LogInstances is the deployment's live replica UUIDs plus any instance
	// UUID that appears in its event timeline within the requested window
	// (a recently stopped/removed replica - the store deletes an Instance
	// row once the agent confirms teardown, so its UUID would otherwise
	// disappear from the live set entirely), capped at 64. It is the UI's
	// deep link from the service page's merged log stream into
	// GET /logs/search?instances=... (plan §3.2, §9) - always a
	// (possibly empty) list, never null.
	LogInstances []string `json:"log_instances"`
	// PD is the PD disaggregation summary of a PD deployment; absent for
	// every other deployment. Each figure is null until it has data.
	PD *MonitorPD `json:"pd,omitempty"`
	// Sources says, for each signal the gateway can stand in for an engine
	// (ttft, e2e, prompt_tokens, generation_tokens, running), whether the
	// charts drew it from the engine ("engine") or the gateway ("gateway");
	// a signal with data from neither is absent. The page does not label
	// the source; it uses this to explain charts only an engine can fill.
	Sources map[string]string `json:"sources"`
	// Health is the deployment's health check.
	Health DeploymentHealth `json:"health"`
}

// MonitorPD is a PD deployment's window: the share of the decoders' prompt
// tokens that arrived by KV transfer, the share of requests the gateway sent
// to a decoder alone (by reason), and the gateway's prefill stage time.
type MonitorPD struct {
	EffectivenessPct  *float64           `json:"effectiveness_pct"`
	FallbackRatePct   *float64           `json:"fallback_rate_pct"`
	Fallbacks         map[string]float64 `json:"fallbacks"`
	PrefillStageAvgMs *float64           `json:"prefill_stage_avg_ms"`
	PrefillWaitingAvg *float64           `json:"prefill_waiting_avg"`
	AvgPromptTokens   *float64           `json:"avg_prompt_tokens"`
	// PrefillStageP95Ms is the prefill stage's p95 over the same window, from
	// the request store's per-request stages.
	PrefillStageP95Ms *float64 `json:"prefill_stage_p95_ms"`
	// KVTransfer is the engine's own KV transfer statistics.
	KVTransfer MonitorPDKVTransfer `json:"kv_transfer"`
}

// MonitorPDKVTransfer is the decode instances' KV transfer over the window,
// per transfer (not per request: one request may take several transfers).
// Exposed is false when the PD mode's engine exports no transfer metrics
// (vllm-ascend's Mooncake connector); every value is then null.
type MonitorPDKVTransfer struct {
	Exposed  bool     `json:"exposed"`
	AvgMs    *float64 `json:"avg_ms"`
	P95Ms    *float64 `json:"p95_ms"`
	AvgMB    *float64 `json:"avg_mb"`
	MBPerSec *float64 `json:"mb_per_s"`
	Failed   *float64 `json:"failed"`
	Expired  *float64 `json:"expired"`
}

// MonitorMetricSeries is one stored series in the metric catalog GET
// /api/v1/monitor/metrics answers (inferencemetrics and platformmetrics).
// An inference histogram contributes three: the family itself, stored as
// quantile gauges (Quantiles true, one series per "quantile" label), and
// its _sum and _count counters.
type MonitorMetricSeries struct {
	Series string `json:"series"`
	// Name is the vocabulary's short name the series belongs to.
	Name string `json:"name"`
	// Source is what measures it: "engine" (scraped from the engine),
	// "gateway" (measured by the gateway), or the platform's own "gpu",
	// "dgpu", "node", "network", "kv_disk", "benchmark".
	Source    string `json:"source"`
	Kind      string `json:"kind"`
	Unit      string `json:"unit"`
	Quantiles bool   `json:"quantiles"`
	// Labels are the label keys every sample carries, when the vocabulary
	// states them.
	Labels []string `json:"labels,omitempty"`
	// Description says what the value measures (English), when the
	// vocabulary states it.
	Description string `json:"description,omitempty"`
}
