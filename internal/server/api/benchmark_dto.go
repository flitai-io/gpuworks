package api

import (
	"encoding/json"
	"time"

	"gpuworks.ai/gpuworks/internal/benchrun"
	"gpuworks.ai/gpuworks/internal/server/api/dto"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
)

// benchmarkDTO is deliberately separate from the GORM entity.
type benchmarkDTO struct {
	ID                uint                      `json:"id"`
	Name              string                    `json:"name"`
	TargetKind        string                    `json:"target_kind"`
	DeploymentID      uint                      `json:"deployment_id,omitempty"`
	InstanceUUID      string                    `json:"instance_uuid,omitempty"`
	BaseURL           string                    `json:"base_url,omitempty"`
	Model             string                    `json:"model"`
	LoadOrigin        string                    `json:"load_origin"`
	NumGPUs           int                       `json:"num_gpus"`
	Dataset           json.RawMessage           `json:"dataset,omitempty"`
	Mode              string                    `json:"mode"`
	Search            json.RawMessage           `json:"search,omitempty"`
	TokenizerRef      string                    `json:"tokenizer_ref,omitempty"`
	TargetSnapshot    json.RawMessage           `json:"target_snapshot,omitempty"`
	Warnings          []string                  `json:"warnings,omitempty"`
	DeploymentDeleted bool                      `json:"deployment_deleted,omitempty"`
	InstanceDeleted   bool                      `json:"instance_deleted,omitempty"`
	KeepDetail        bool                      `json:"keep_detail"`
	DetailCapMB       int                       `json:"detail_cap_mb"`
	State             string                    `json:"state"`
	Progress          json.RawMessage           `json:"progress,omitempty"`
	Verdict           json.RawMessage           `json:"verdict,omitempty"`
	Error             string                    `json:"error,omitempty"`
	BestPoint         *benchmarkPointSummaryDTO `json:"best_point,omitempty"`
	Points            []benchmarkPointDTO       `json:"points,omitempty"`
	CreatedAt         time.Time                 `json:"created_at"`
	StartedAt         *time.Time                `json:"started_at,omitempty"`
	EndedAt           *time.Time                `json:"ended_at,omitempty"`
}

type benchmarkModelsDTO struct {
	Models        []string `json:"models"`
	NormalizedURL string   `json:"normalized_url"`
	Notice        string   `json:"notice,omitempty"`
}

type benchmarkPointDTO struct {
	ID            uint                   `json:"id"`
	Seq           int                    `json:"seq"`
	Concurrency   int                    `json:"concurrency"`
	TargetISL     int                    `json:"target_isl"`
	TargetOSL     int                    `json:"target_osl"`
	RequestRate   float64                `json:"request_rate"`
	State         string                 `json:"state"`
	ActualISL     float64                `json:"actual_isl"`
	ActualOSL     float64                `json:"actual_osl"`
	InputTokens   int64                  `json:"input_tokens"`
	OutputTokens  int64                  `json:"output_tokens"`
	Success       int                    `json:"success"`
	Errors        int                    `json:"errors"`
	OutputTput    float64                `json:"output_tput"`
	TotalTput     float64                `json:"total_tput"`
	MaxOutputTput float64                `json:"max_output_tput"`
	MaxTotalTput  float64                `json:"max_total_tput"`
	RequestTput   float64                `json:"request_tput"`
	AvgTTFT       float64                `json:"avg_ttft"`
	P50TTFT       float64                `json:"p50_ttft"`
	P99TTFT       float64                `json:"p99_ttft"`
	AvgTPOT       float64                `json:"avg_tpot"`
	P50TPOT       float64                `json:"p50_tpot"`
	P99TPOT       float64                `json:"p99_tpot"`
	AvgLatency    float64                `json:"avg_latency"`
	P50Latency    float64                `json:"p50_latency"`
	P99Latency    float64                `json:"p99_latency"`
	LongTail      *dto.BenchmarkLongTail `json:"long_tail,omitempty"`
	DetailPath    string                 `json:"detail_path,omitempty"`
	StartedAt     *time.Time             `json:"started_at,omitempty"`
	EndedAt       *time.Time             `json:"ended_at,omitempty"`
}

// benchmarkPointSummaryDTO keeps list responses small while exposing the
// persisted verdict point needed to render an evidence-backed summary.
type benchmarkPointSummaryDTO struct {
	Seq         int     `json:"seq"`
	Concurrency int     `json:"concurrency"`
	TargetISL   int     `json:"target_isl"`
	TargetOSL   int     `json:"target_osl"`
	RequestRate float64 `json:"request_rate"`
	OutputTput  float64 `json:"output_tput"`
	RequestTput float64 `json:"request_tput"`
	P99TTFT     float64 `json:"p99_ttft"`
}

func benchmarkPointDTOFromEntity(p store.BenchmarkPoint) benchmarkPointDTO {
	out := benchmarkPointDTO{ID: p.ID, Seq: p.Seq, Concurrency: p.Concurrency, TargetISL: p.TargetISL, TargetOSL: p.TargetOSL, RequestRate: p.RequestRate, State: p.State, ActualISL: p.ActualISL, ActualOSL: p.ActualOSL, InputTokens: p.InputTokens, OutputTokens: p.OutputTokens, Success: p.Success, Errors: p.Errors, OutputTput: p.OutputTput, TotalTput: p.TotalTput, MaxOutputTput: p.MaxOutputTput, MaxTotalTput: p.MaxTotalTput, RequestTput: p.RequestTput, AvgTTFT: p.AvgTTFT, P50TTFT: p.P50TTFT, P99TTFT: p.P99TTFT, AvgTPOT: p.AvgTPOT, P50TPOT: p.P50TPOT, P99TPOT: p.P99TPOT, AvgLatency: p.AvgLatency, P50Latency: p.P50Latency, P99Latency: p.P99Latency, LongTail: benchmarkLongTailDTO(p.LongTail), DetailPath: p.DetailPath, StartedAt: utcTimePtr(p.StartedAt), EndedAt: utcTimePtr(p.EndedAt)}
	if out.LongTail != nil && out.LongTail.Evidence != nil && out.LongTail.Evidence.Version == 1 {
		all := out.LongTail.Evidence.All
		out.AvgTTFT, out.P50TTFT, out.P99TTFT = all.TTFT.MeanMs, all.TTFT.P50Ms, all.TTFT.P99Ms
		out.AvgTPOT, out.P50TPOT, out.P99TPOT = all.TPOT.MeanMs, all.TPOT.P50Ms, all.TPOT.P99Ms
		out.AvgLatency, out.P50Latency, out.P99Latency = all.LastOutputLatency.MeanMs, all.LastOutputLatency.P50Ms, all.LastOutputLatency.P99Ms
	}
	return out
}

func (a *apiHandlers) benchmarkDTO(rn *store.BenchmarkRun, points bool) benchmarkDTO {
	d := benchmarkDTO{ID: rn.ID, Name: rn.Name, TargetKind: rn.TargetKind, DeploymentID: rn.DeploymentID, InstanceUUID: rn.InstanceUUID, BaseURL: rn.BaseURL, Model: rn.Model, LoadOrigin: rn.LoadOrigin, NumGPUs: rn.NumGPUs, Dataset: json.RawMessage(rn.Dataset), Mode: rn.Mode, Search: json.RawMessage(rn.Search), TokenizerRef: rn.TokenizerRef, TargetSnapshot: json.RawMessage(rn.TargetSnapshot), KeepDetail: rn.KeepDetail, DetailCapMB: rn.DetailCapMB, State: rn.State, Progress: json.RawMessage(rn.Progress), Verdict: json.RawMessage(rn.Verdict), Error: rn.Error, CreatedAt: utcTime(rn.CreatedAt), StartedAt: utcTimePtr(rn.StartedAt), EndedAt: utcTimePtr(rn.EndedAt)}
	if rn.Warnings != "" {
		_ = json.Unmarshal([]byte(rn.Warnings), &d.Warnings)
	}
	if rn.TargetKind == "instance" {
		if _, err := a.d.Store.GetDeployment(rn.DeploymentID); err == store.ErrNotFound {
			d.DeploymentDeleted = true
		}
		if rn.InstanceUUID != "" {
			if _, err := a.d.Store.InstanceByUUID(rn.InstanceUUID); err == store.ErrNotFound {
				d.InstanceDeleted = true
			}
		}
	}
	var verdict store.BenchmarkVerdict
	if json.Unmarshal([]byte(rn.Verdict), &verdict) == nil && verdict.BestPointSeq > 0 {
		if entity, err := a.d.Store.GetBenchmarkPoint(rn.ID, verdict.BestPointSeq); err == nil {
			point := benchmarkPointSummaryDTO{Seq: entity.Seq, Concurrency: entity.Concurrency, TargetISL: entity.TargetISL, TargetOSL: entity.TargetOSL, RequestRate: entity.RequestRate, OutputTput: entity.OutputTput, RequestTput: entity.RequestTput, P99TTFT: entity.P99TTFT}
			d.BestPoint = &point
		}
	}
	if points {
		entities, _ := a.d.Store.ListBenchmarkPoints(rn.ID)
		d.Points = make([]benchmarkPointDTO, 0, len(entities))
		for _, p := range entities {
			if p.State == "running" {
				continue
			}
			d.Points = append(d.Points, benchmarkPointDTOFromEntity(p))
		}
		d.Warnings = currentBenchmarkWarnings(d.Warnings, rn.Dataset, entities)
	}
	return d
}

func currentBenchmarkWarnings(warnings []string, rawDataset string, points []store.BenchmarkPoint) []string {
	const agenticDrift = "BENCHMARK_AGENTIC_LENGTH_DRIFT"
	var config dataset.Config
	if json.Unmarshal([]byte(rawDataset), &config) != nil || config.Kind != "agentic" {
		return warnings
	}
	observed, exceeded := false, false
	for _, point := range points {
		if point.State == "running" || point.ActualISL <= 0 {
			continue
		}
		observed = true
		exceeded = exceeded || dataset.AgenticLengthDriftExceeded(point.ActualISL, config.MeanInputTokens)
	}
	if !observed || exceeded {
		return warnings
	}
	out := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		if warning != agenticDrift {
			out = append(out, warning)
		}
	}
	return out
}

// benchmarkLongTailDTO reads a point's stored long_tail document
// (benchrun.LongTailDoc); nil when there is none or it does not parse.
func benchmarkLongTailDTO(raw string) *dto.BenchmarkLongTail {
	if raw == "" {
		return nil
	}
	var doc benchrun.LongTailDoc
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return nil
	}
	out := &dto.BenchmarkLongTail{
		TTFTMS: doc.TTFT, TPOTMS: doc.TPOT, LatencyMS: doc.Latency, ITLMS: doc.ITL,
		ApproximateTokens: doc.ApproximateTokens, KVHitRate: doc.KVHitRate, CacheReportCoverage: doc.CacheReportCoverage,
		PeakInputTokens: doc.PeakInputTokens,
		Evidence:        doc.Evidence,
	}
	if doc.Speculative != nil {
		out.Speculative = &dto.BenchmarkSpeculative{TokensPerIteration: doc.Speculative.TokensPerIteration, AcceptanceRate: doc.Speculative.AcceptanceRate}
	}
	return out
}
