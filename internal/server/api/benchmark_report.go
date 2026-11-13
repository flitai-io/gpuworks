package api

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	benchreport "gpuworks.ai/gpuworks/pkg/bench/report"
)

func (a *apiHandlers) downloadBenchmarkReport(w http.ResponseWriter, r *http.Request) {
	run, err := a.d.Store.GetBenchmarkRun(parseUint(chi.URLParam(r, "id")))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	points, err := a.d.Store.ListBenchmarkPoints(run.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	input := benchreport.Run{
		Error: run.Error, ID: run.ID, Name: run.Name, EvaluationURL: a.managementRequestOrigin(r) + "/evaluations/" + chi.URLParam(r, "id"), State: run.State, Mode: run.Mode, Model: run.Model,
		TokenizerRef: run.TokenizerRef, Dataset: json.RawMessage(run.Dataset), Search: json.RawMessage(run.Search),
		TargetSnapshot: json.RawMessage(run.TargetSnapshot), CreatedAt: run.CreatedAt, StartedAt: run.StartedAt, EndedAt: run.EndedAt,
		Verdict: json.RawMessage(run.Verdict), KeepDetail: run.KeepDetail, DetailCapMB: run.DetailCapMB,
	}
	var progress struct {
		Warmup []loadgen.DetailRecord `json:"warmup"`
	}
	_ = json.Unmarshal([]byte(run.Progress), &progress)
	input.Warmup = progress.Warmup
	if run.Warnings != "" {
		_ = json.Unmarshal([]byte(run.Warnings), &input.Warnings)
	}
	detailBudget := newBenchmarkReportDetailBudget()
	for _, point := range points {
		if point.State == "running" {
			continue
		}
		row := benchreport.Point{
			Seq: point.Seq, State: point.State, Concurrency: point.Concurrency, RequestRate: point.RequestRate, Success: point.Success, Errors: point.Errors,
			ActualISL: point.ActualISL, ActualOSL: point.ActualOSL, OutputTput: point.OutputTput,
			TotalTput: point.TotalTput, RequestTput: point.RequestTput, P50TTFT: point.P50TTFT,
			P99TTFT: point.P99TTFT, P50TPOT: point.P50TPOT, P99TPOT: point.P99TPOT,
			P50Latency: point.P50Latency, P99Latency: point.P99Latency,
			LongTail:  json.RawMessage(point.LongTail),
			TargetISL: point.TargetISL, TargetOSL: point.TargetOSL, Repeat: point.Repeat,
			InputTokens: point.InputTokens, OutputTokens: point.OutputTokens,
			AvgTTFT: point.AvgTTFT, AvgTPOT: point.AvgTPOT, AvgLatency: point.AvgLatency,
			StartedAt: point.StartedAt, EndedAt: point.EndedAt,
		}
		row.Details, row.DetailStatus, row.DetailNote = a.readBenchmarkReportDetailsWithin(r.Context(), run, point, detailBudget)
		input.Points = append(input.Points, row)
	}
	document, err := benchreport.Render(r.URL.Query().Get("format"), input)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", document.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": document.Filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(document.Body)
}
