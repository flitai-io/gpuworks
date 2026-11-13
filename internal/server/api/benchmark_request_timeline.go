package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

type benchmarkTimelineMetricsDTO struct {
	StartedAt      *time.Time `json:"started_at"`
	FirstOutputAt  *time.Time `json:"first_output_at"`
	FirstContentAt *time.Time `json:"first_content_at"`
	LastOutputAt   *time.Time `json:"last_output_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	TTFTMs         *float64   `json:"ttft_ms"`
	TPOTMs         *float64   `json:"tpot_ms"`
	LatencyMs      *float64   `json:"latency_ms"`
	CompletionMs   *float64   `json:"completion_ms"`
	InputTokens    int        `json:"input_tokens"`
	OutputTokens   int        `json:"output_tokens"`
}

type benchmarkTimelineRecordDTO struct {
	Version      int                         `json:"version"`
	RunID        uint                        `json:"run_id"`
	PointSeq     int                         `json:"point_seq"`
	SessionID    string                      `json:"session_id"`
	Turn         int                         `json:"turn"`
	PlannedTurns int                         `json:"planned_turns"`
	Attempt      int                         `json:"attempt"`
	FinishReason string                      `json:"finish_reason,omitempty"`
	ErrorText    string                      `json:"error_text,omitempty"`
	Canceled     bool                        `json:"canceled,omitempty"`
	ErrorStage   string                      `json:"error_stage,omitempty"`
	HTTPStatus   int                         `json:"http_status,omitempty"`
	InputSource  string                      `json:"input_source"`
	OutputSource string                      `json:"output_source"`
	OutputBudget int                         `json:"output_budget"`
	Metrics      benchmarkTimelineMetricsDTO `json:"metrics"`
}

func (a *apiHandlers) getBenchmarkRequestTimeline(w http.ResponseWriter, r *http.Request) {
	id, idErr := strconv.ParseUint(chi.URLParam(r, "id"), 10, strconv.IntSize)
	seq, seqErr := strconv.Atoi(chi.URLParam(r, "seq"))
	if idErr != nil || id == 0 {
		writeErr(w, http.StatusBadRequest, benchmarkParameterError("id", "benchmark id must be a positive integer"))
		return
	}
	if seqErr != nil || seq <= 0 {
		writeErr(w, http.StatusBadRequest, benchmarkParameterError("seq", "point sequence must be a positive integer"))
		return
	}
	run, err := a.d.Store.GetBenchmarkRun(uint(id))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	point, err := a.d.Store.GetBenchmarkPoint(uint(id), seq)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rows, status, _ := a.readBenchmarkReportDetails(r.Context(), run, *point)
	records := make([]benchmarkTimelineRecordDTO, 0, len(rows))
	for _, row := range rows {
		m := row.Metrics
		records = append(records, benchmarkTimelineRecordDTO{
			Version: row.Version, RunID: row.RunID, PointSeq: row.PointSeq, SessionID: row.SessionID, Turn: row.Turn, PlannedTurns: row.PlannedTurns, Attempt: row.Attempt,
			FinishReason: row.FinishReason, ErrorText: loadgen.RedactErrorText(row.ErrorText), Canceled: row.Canceled, ErrorStage: row.ErrorStage, HTTPStatus: row.HTTPStatus,
			InputSource: row.InputSource, OutputSource: row.OutputSource, OutputBudget: row.OutputBudget,
			Metrics: benchmarkTimelineMetricsDTO{StartedAt: m.StartedAt, FirstOutputAt: m.FirstOutputAt, FirstContentAt: m.FirstContentAt, LastOutputAt: m.LastOutputAt, FinishedAt: m.FinishedAt, TTFTMs: m.TTFTMs, TPOTMs: m.TPOTMs, LatencyMs: m.LatencyMs, CompletionMs: m.CompletionMs, InputTokens: m.InputTokens, OutputTokens: m.OutputTokens},
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		Status  string                       `json:"status"`
		Records []benchmarkTimelineRecordDTO `json:"records"`
	}{Status: status, Records: records})
}
