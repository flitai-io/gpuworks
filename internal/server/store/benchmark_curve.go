package store

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

const (
	BenchmarkCurveMaxSamples      = 300
	BenchmarkWarningTokenizerLoad = proto.BenchmarkWarningTokenizerLoad
)

// BenchmarkCurveSample is the persisted, executor-independent shape of one
// process sample. T is Unix milliseconds.
type BenchmarkCurveSample struct {
	T           int64   `json:"t"`
	OutputTput  float64 `json:"output_tput"`
	TotalTput   float64 `json:"total_tput"`
	RequestTput float64 `json:"request_tput"`
	TTFTP50     float64 `json:"ttft_p50"`
	TPOTP50     float64 `json:"tpot_p50"`
	LatencyP99  float64 `json:"latency_p99"`
}

func curveSample(at time.Time, sample metrics.ProcessSample) BenchmarkCurveSample {
	return BenchmarkCurveSample{
		T: at.UTC().UnixMilli(), OutputTput: sample.OutputTput,
		TotalTput: sample.TotalTput, RequestTput: sample.RequestTput,
		TTFTP50: sample.TTFTP50, TPOTP50: sample.TPOTP50, LatencyP99: sample.LatencyP99,
	}
}

// appendBoundedCurve preserves the full time span while keeping recent samples
// dense. Whenever the bound is crossed, every other historical sample plus the
// newest sample is retained; later incremental writes fill the freed capacity.
func appendBoundedCurve(samples []BenchmarkCurveSample, sample BenchmarkCurveSample) []BenchmarkCurveSample {
	samples = append(samples, sample)
	if len(samples) <= BenchmarkCurveMaxSamples {
		return samples
	}
	compacted := make([]BenchmarkCurveSample, 0, BenchmarkCurveMaxSamples/2+1)
	for i := 0; i < len(samples)-1; i += 2 {
		compacted = append(compacted, samples[i])
	}
	last := samples[len(samples)-1]
	if compacted[len(compacted)-1].T != last.T || compacted[len(compacted)-1] != last {
		compacted = append(compacted, last)
	}
	return compacted
}

// AppendBenchmarkCurveSample incrementally persists the same progress samples
// used for live reporting. A point row may be created before its aggregate is
// complete; UpsertBenchmarkPoint preserves the curve when final results arrive.
func (s *Store) AppendBenchmarkCurveSample(runID uint, seq int, at time.Time, sample metrics.ProcessSample) error {
	if runID == 0 || seq <= 0 {
		return nil
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		// Samples can follow a terminal status, but cannot outlive their run.
		// The no-op parent write serializes this transaction with deletion
		// without changing terminal state, timestamps, or final point facts.
		parent := tx.Model(&BenchmarkRun{}).Where("id = ?", runID).
			UpdateColumn("state", gorm.Expr("state"))
		if parent.Error != nil || parent.RowsAffected == 0 {
			return parent.Error
		}
		var point BenchmarkPoint
		err := tx.Where("run_id = ? AND seq = ?", runID, seq).First(&point).Error
		if err == gorm.ErrRecordNotFound {
			point = BenchmarkPoint{RunID: runID, Seq: seq, State: "running"}
		} else if err != nil {
			return err
		}
		var samples []BenchmarkCurveSample
		if point.Curve != "" {
			if err := json.Unmarshal([]byte(point.Curve), &samples); err != nil {
				return err
			}
		}
		samples = appendBoundedCurve(samples, curveSample(at, sample))
		raw, err := json.Marshal(samples)
		if err != nil {
			return err
		}
		point.Curve = string(raw)
		if point.ID == 0 {
			return tx.Create(&point).Error
		}
		return tx.Model(&BenchmarkPoint{}).Where("id = ?", point.ID).Update("curve", point.Curve).Error
	})
}

func (s *Store) AddBenchmarkWarning(id uint, warning string) error {
	if warning == "" {
		return nil
	}
	run, err := s.GetBenchmarkRun(id)
	if err != nil {
		return err
	}
	var warnings []string
	if run.Warnings != "" {
		_ = json.Unmarshal([]byte(run.Warnings), &warnings)
	}
	for _, existing := range warnings {
		if existing == warning {
			return nil
		}
	}
	warnings = append(warnings, warning)
	raw, err := json.Marshal(warnings)
	if err != nil {
		return err
	}
	return s.db.Model(&BenchmarkRun{}).Where("id = ?", id).Update("warnings", string(raw)).Error
}
