package proto

import (
	"encoding/json"
	"time"
)

// BenchmarkSpec is a complete idempotent desired-state workload for the
// embedded benchmark engine. Long runs use state sync instead of RPC.
type BenchmarkSpec struct {
	ID           uint            `json:"id"`
	DesiredState string          `json:"desired_state"`
	TargetURL    string          `json:"target_url"`
	APIKey       string          `json:"api_key,omitempty"`
	Model        string          `json:"model"`
	Mode         string          `json:"mode"`
	Dataset      json.RawMessage `json:"dataset,omitempty"`
	Search       json.RawMessage `json:"search,omitempty"`
	TokenizerRef string          `json:"tokenizer_ref,omitempty"`
	Concurrency  int             `json:"concurrency,omitempty"`
	KeepDetail   bool            `json:"keep_detail,omitempty"`
	DetailCapMB  int             `json:"detail_cap_mb,omitempty"`
}

type BenchmarkStatus struct {
	ID         uint            `json:"id"`
	State      string          `json:"state"`
	PointIndex int             `json:"point_index,omitempty"`
	PointTotal int             `json:"point_total,omitempty"`
	PointCount int             `json:"point_count,omitempty"`
	Completed  int             `json:"completed,omitempty"`
	Errors     int             `json:"errors,omitempty"`
	Point      json.RawMessage `json:"point,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	// Warmup retains non-measured attempts as loadgen DetailRecord JSON; no prompt/reply text.
	Warmup       json.RawMessage        `json:"warmup,omitempty"`
	Sample       json.RawMessage        `json:"sample,omitempty"`
	CurrentPoint *BenchmarkCurrentPoint `json:"current_point,omitempty"`
	Message      string                 `json:"message,omitempty"`
	// Code and Params classify a failed run (for example
	// BENCHMARK_DATASET_LOAD_FAILED with {"dataset": "multiturn"}); Message
	// stays the English fallback. Agents older than codes send neither.
	Code     string         `json:"code,omitempty"`
	Params   map[string]any `json:"params,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
	// StopReason records why the search ended. The server uses this explicit
	// execution fact when computing a persisted verdict; it deliberately does
	// not guess a reason for statuses from older agents.
	StopReason string `json:"stop_reason,omitempty"`
	// DetailChunk carries one gzip JSONL artifact chunk for agent-origin runs.
	// Chunks are sent over the existing status.benchmark control stream so long
	// benchmark execution still uses desired state rather than a long RPC.
	DetailSeq   int    `json:"detail_seq,omitempty"`
	DetailChunk []byte `json:"detail_chunk,omitempty"`
	DetailFinal bool   `json:"detail_final,omitempty"`
	DetailPath  string `json:"detail_path,omitempty"`
}

// BenchmarkCurrentPoint identifies the in-progress load point. It is carried
// alongside cumulative samples so clients never mistake the previous completed
// point for the live point.
type BenchmarkCurrentPoint struct {
	Seq          int `json:"seq"`
	Concurrency  int `json:"concurrency"`
	TargetISL    int `json:"target_isl"`
	TargetOSL    int `json:"target_osl"`
	RequestTotal int `json:"request_total"`
	// RequestRate is the open-loop arrival rate in req/s; 0 is closed loop,
	// where Concurrency is the load. In open loop Concurrency is the in-flight
	// cap, 0 meaning unlimited.
	RequestRate float64 `json:"request_rate,omitempty"`
	// Warmup marks the discarded warmup requests that run before the first
	// point. Seq is then 0.
	Warmup bool `json:"warmup,omitempty"`
}

// BenchmarkPointResult is the stable completed-point wire contract shared by
// server-origin and agent-origin execution paths.
type BenchmarkPointResult struct {
	ID            uint            `json:"id,omitempty"`
	Seq           int             `json:"seq"`
	Concurrency   int             `json:"concurrency"`
	TargetISL     int             `json:"target_isl"`
	TargetOSL     int             `json:"target_osl"`
	RequestRate   float64         `json:"request_rate,omitempty"`
	Repeat        int             `json:"repeat,omitempty"`
	State         string          `json:"state"`
	ActualISL     float64         `json:"actual_isl"`
	ActualOSL     float64         `json:"actual_osl"`
	InputTokens   int64           `json:"input_tokens"`
	OutputTokens  int64           `json:"output_tokens"`
	Success       int             `json:"success"`
	Errors        int             `json:"errors"`
	OutputTput    float64         `json:"output_tput"`
	TotalTput     float64         `json:"total_tput"`
	MaxOutputTput float64         `json:"max_output_tput"`
	MaxTotalTput  float64         `json:"max_total_tput"`
	RequestTput   float64         `json:"request_tput"`
	AvgTTFT       float64         `json:"avg_ttft"`
	P50TTFT       float64         `json:"p50_ttft"`
	P99TTFT       float64         `json:"p99_ttft"`
	AvgTPOT       float64         `json:"avg_tpot"`
	P50TPOT       float64         `json:"p50_tpot"`
	P99TPOT       float64         `json:"p99_tpot"`
	AvgLatency    float64         `json:"avg_latency"`
	P50Latency    float64         `json:"p50_latency"`
	P99Latency    float64         `json:"p99_latency"`
	LongTail      json.RawMessage `json:"long_tail,omitempty"`
	DetailPath    string          `json:"detail_path,omitempty"`
	StartedAt     *time.Time      `json:"started_at,omitempty"`
	EndedAt       *time.Time      `json:"ended_at,omitempty"`
}

const (
	BenchmarkWarningTokenizerLoad = "BENCHMARK_TOKENIZER_LOAD_FAILED"
	BenchmarkWarningAgenticDrift  = "BENCHMARK_AGENTIC_LENGTH_DRIFT"
	// BenchmarkWarningConflictOverridden marks a run the user started on top of
	// an active one. Concurrent runs share GPUs and node resources, so the
	// results of both may be depressed; the warning keeps that decision visible
	// on the run instead of only in the dialog that approved it.
	BenchmarkWarningConflictOverridden = "BENCHMARK_CONFLICT_OVERRIDDEN"
	// BenchmarkWarningWarmupUnsupported marks a run whose load-origin agent
	// predates warmup (no CapBenchWarmup); it ran without warmup requests.
	BenchmarkWarningWarmupUnsupported = "BENCHMARK_WARMUP_UNSUPPORTED"
	// BenchmarkWarningRequestTimeoutUnsupported marks a run whose load-origin
	// agent predates the configurable request timeout (no
	// CapBenchRequestTimeout); it bounded every request by five minutes
	// instead of the configured timeout.
	BenchmarkWarningRequestTimeoutUnsupported = "BENCHMARK_REQUEST_TIMEOUT_UNSUPPORTED"
	// BenchmarkWarningStreamOutputUnrecognized marks a run in which a
	// successful streamed request reported output tokens in its usage but no
	// stream chunk carried text the load generator recognizes, so its TTFT,
	// TPOT and output latency are missing from the sample counts.
	BenchmarkWarningStreamOutputUnrecognized = "BENCHMARK_STREAM_OUTPUT_UNRECOGNIZED"
	BenchmarkStopGainBelowThreshold          = "gain_below_threshold"
	BenchmarkStopHitMaxConcurrency           = "hit_max_concurrency"
	BenchmarkStopErrorRate                   = "error_rate"
	BenchmarkStopSLAViolated                 = "sla_violated"
	BenchmarkStopGridExhausted               = "grid_exhausted"
	BenchmarkStopCanceled                    = "canceled"
	BenchmarkStopFatal                       = "fatal"
)

type BenchmarkCancelParams struct {
	ID uint `json:"id"`
}

type BenchmarkCancelResult struct {
	ID      uint   `json:"id"`
	State   string `json:"state"`
	Running bool   `json:"running"`
}
