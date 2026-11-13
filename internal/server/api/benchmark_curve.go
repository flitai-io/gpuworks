package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/store"
)

type benchmarkCurveDTO struct {
	Points []benchmarkPointCurveDTO `json:"points"`
}

type benchmarkPointCurveDTO struct {
	Seq         int                       `json:"seq"`
	Concurrency int                       `json:"concurrency"`
	Samples     []benchmarkCurveSampleDTO `json:"samples"`
}

// Nil metrics preserve missing observations separately from measured zero.
type benchmarkCurveSampleDTO struct {
	T           int64    `json:"t"`
	OutputTput  *float64 `json:"output_tput"`
	TotalTput   *float64 `json:"total_tput"`
	RequestTput *float64 `json:"request_tput"`
	TTFTP50     *float64 `json:"ttft_p50"`
	TPOTP50     *float64 `json:"tpot_p50"`
	LatencyP99  *float64 `json:"latency_p99"`
}

func (a *apiHandlers) getBenchmarkCurve(w http.ResponseWriter, r *http.Request) {
	runID := parseUint(chi.URLParam(r, "id"))
	if _, err := a.d.Store.GetBenchmarkRun(runID); err != nil {
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	points, err := a.d.Store.ListBenchmarkPoints(runID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := benchmarkCurveDTO{Points: make([]benchmarkPointCurveDTO, 0, len(points))}
	for _, point := range points {
		if point.Curve == "" {
			continue
		}
		var rawSamples []json.RawMessage
		if err := json.Unmarshal([]byte(point.Curve), &rawSamples); err != nil {
			if a.d.Log != nil {
				a.d.Log.WarnContext(r.Context(), "benchmark curve discarded", "run_id", runID, "point_seq", point.Seq)
			}
			continue
		}
		samples := make([]benchmarkCurveSampleDTO, 0, len(rawSamples))
		for i, raw := range rawSamples {
			var sample *benchmarkCurveSampleDTO
			if err := json.Unmarshal(raw, &sample); err != nil || sample == nil {
				// Identify the damaged observation without logging stored payloads.
				if a.d.Log != nil {
					a.d.Log.WarnContext(r.Context(), "benchmark curve sample discarded", "run_id", runID, "point_seq", point.Seq, "sample_index", i)
				}
				continue
			}
			samples = append(samples, *sample)
		}
		out.Points = append(out.Points, benchmarkPointCurveDTO{Seq: point.Seq, Concurrency: point.Concurrency, Samples: samples})
	}
	writeJSON(w, http.StatusOK, out)
}
