package loadgen

import (
	"encoding/json"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

// DetailRecord is the versioned JSONL wire contract shared by both load origins.
// Usage retains absent/null/empty/explicit-zero distinctions from the endpoint.
type DetailRecord struct {
	Version      int             `json:"version"`
	RunID        uint            `json:"run_id"`
	PointSeq     int             `json:"point_seq"`
	SessionID    string          `json:"session_id"`
	Turn         int             `json:"turn"`
	PlannedTurns int             `json:"planned_turns"`
	Attempt      int             `json:"attempt"`
	Metrics      DetailMetrics   `json:"metrics"`
	Usage        json.RawMessage `json:"usage,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
	ErrorText    string          `json:"error_text,omitempty"`
	Canceled     bool            `json:"canceled,omitempty"`
	ErrorStage   string          `json:"error_stage,omitempty"`
	HTTPStatus   int             `json:"http_status,omitempty"`
	// PriorityClass is the class the gateway says the request ran at;
	// absent when it said none.
	PriorityClass string `json:"priority_class,omitempty"`
	LocalInput    *int   `json:"local_input"`
	LocalOutput   *int   `json:"local_output"`
	InputSource   string `json:"input_source"`
	OutputSource  string `json:"output_source"`
	OutputBudget  int    `json:"output_budget"`
	Stream        bool   `json:"stream"`
	Text          string `json:"text"`
}

type DetailMetrics struct {
	StartedAt        *time.Time `json:"started_at"`
	FirstOutputAt    *time.Time `json:"first_output_at"`
	FirstContentAt   *time.Time `json:"first_content_at"`
	LastOutputAt     *time.Time `json:"last_output_at"`
	FinishedAt       *time.Time `json:"finished_at"`
	TTFTMs           *float64   `json:"ttft_ms"`
	TPOTMs           *float64   `json:"tpot_ms"`
	LatencyMs        *float64   `json:"latency_ms"`
	CompletionMs     *float64   `json:"completion_ms"`
	InputTokens      int        `json:"input_tokens"`
	OutputTokens     int        `json:"output_tokens"`
	Chunks           int        `json:"chunks"`
	ChunkIntervalsMs []float64  `json:"chunk_intervals_ms"`
}

func NewDetailRecord(r Result) DetailRecord {
	m := r.Metrics
	d := DetailRecord{Version: 1, SessionID: m.SessionID, Turn: m.Turn, PlannedTurns: m.PlannedTurns, Attempt: 1,
		Usage: r.UsageJSON, FinishReason: m.FinishReason, ErrorStage: m.ErrorStage, HTTPStatus: m.HTTPStatus, PriorityClass: m.PriorityClass, Canceled: m.Canceled,
		LocalInput: m.LocalInputTokens, LocalOutput: m.LocalOutputTokens, InputSource: m.InputSource, OutputSource: m.OutputSource,
		OutputBudget: r.OutputBudget, Stream: r.Stream, Text: r.Text}
	if m.Error != nil {
		d.ErrorText = RedactErrorText(m.Error.Error())
	}
	d.Metrics = DetailMetrics{StartedAt: optionalTime(m.StartedAt), FirstOutputAt: optionalTime(m.FirstOutputAt), FirstContentAt: optionalTime(m.FirstContentAt), LastOutputAt: optionalTime(m.LastOutputAt), FinishedAt: optionalTime(m.FinishedAt), InputTokens: m.InputTokens, OutputTokens: m.OutputTokens, Chunks: m.Chunks}
	if !m.FirstOutputAt.IsZero() {
		v := metrics.DurationMillis(m.TTFT)
		d.Metrics.TTFTMs = &v
	}
	if v, ok := m.TimePerOutputToken(); ok {
		ms := metrics.DurationMillis(v)
		d.Metrics.TPOTMs = &ms
	}
	if !m.LastOutputAt.IsZero() {
		v := metrics.DurationMillis(m.Latency)
		d.Metrics.LatencyMs = &v
	}
	if !m.StartedAt.IsZero() && !m.FinishedAt.IsZero() {
		v := metrics.DurationMillis(m.FinishedAt.Sub(m.StartedAt))
		d.Metrics.CompletionMs = &v
	}
	for _, v := range m.ITL {
		d.Metrics.ChunkIntervalsMs = append(d.Metrics.ChunkIntervalsMs, metrics.DurationMillis(v))
	}
	return d
}
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}
