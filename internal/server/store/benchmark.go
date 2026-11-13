package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
)

const BenchmarkErrorServerRestarted = "BENCHMARK_SERVER_RESTARTED"

var ErrBenchmarkActive = errors.New("benchmark is active; cancel it before deleting")

func (s *Store) CreateBenchmarkRun(run *BenchmarkRun) (*BenchmarkRun, error) {
	if run.State == "" {
		run.State = "pending"
	}
	if run.DetailCapMB <= 0 {
		run.DetailCapMB = benchmeta.DefaultDetailCapMB
	}
	if run.DetailCapMB > benchmeta.MaxDetailCapMB {
		run.DetailCapMB = benchmeta.MaxDetailCapMB
	}
	if err := s.db.Create(run).Error; err != nil {
		return nil, err
	}
	if s.Bus != nil {
		s.Bus.PublishChange("benchmark", fmt.Sprint(run.ID), "created")
	}
	return run, nil
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func (s *Store) GetBenchmarkRun(id uint) (*BenchmarkRun, error) {
	var r BenchmarkRun
	err := s.db.First(&r, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &r, err
}

func (s *Store) ListBenchmarkRuns() ([]BenchmarkRun, error) {
	var out []BenchmarkRun
	return out, s.db.Order("id desc").Find(&out).Error
}

// CountActiveBenchmarkRuns counts the runs not yet finished.
func (s *Store) CountActiveBenchmarkRuns() (int64, error) {
	var n int64
	return n, s.db.Model(&BenchmarkRun{}).Where("state IN ?", []string{"pending", "running"}).Count(&n).Error
}

// RecoverServerBenchmarkRuns fails workloads that could only have been driven
// by the previous server process. Agent-origin runs remain reconcilable through
// desired state and are intentionally left untouched.
func (s *Store) RecoverServerBenchmarkRuns() (int64, error) {
	var ids []uint
	if err := s.db.Model(&BenchmarkRun{}).
		Where("load_origin = ? AND state IN ?", "server", []string{"pending", "running"}).
		Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	verdict := string(mustJSON(BenchmarkVerdict{Kind: "failed"}))
	result := s.db.Model(&BenchmarkRun{}).Where("id IN ?", ids).Updates(map[string]any{
		"state": "failed", "error": BenchmarkErrorServerRestarted, "verdict": verdict,
		"ended_at": now, "updated_at": now,
	})
	if result.Error != nil {
		return 0, result.Error
	}
	if s.Bus != nil {
		for _, id := range ids {
			s.Bus.PublishChange("benchmark", fmt.Sprint(id), "status")
		}
	}
	return result.RowsAffected, nil
}
func (s *Store) ListBenchmarkPoints(runID uint) ([]BenchmarkPoint, error) {
	var out []BenchmarkPoint
	return out, s.db.Where("run_id = ?", runID).Order("seq asc").Find(&out).Error
}

func (s *Store) GetBenchmarkPoint(runID uint, seq int) (*BenchmarkPoint, error) {
	var point BenchmarkPoint
	err := s.db.Where("run_id = ? AND seq = ?", runID, seq).First(&point).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &point, err
}

func (s *Store) UpsertBenchmarkPoint(p *BenchmarkPoint) error {
	var existing BenchmarkPoint
	err := s.db.Where("run_id = ? AND seq = ?", p.RunID, p.Seq).First(&existing).Error
	if err == gorm.ErrRecordNotFound {
		return s.db.Create(p).Error
	}
	if err != nil {
		return err
	}
	if p.Curve == "" {
		p.Curve = existing.Curve
	}
	p.ID = existing.ID
	p.CreatedAt = existing.CreatedAt
	return s.db.Save(p).Error
}

func (s *Store) SetBenchmarkPointDetail(runID uint, seq int, path string) error {
	return s.db.Model(&BenchmarkPoint{}).Where("run_id = ? AND seq = ?", runID, seq).Update("detail_path", path).Error
}

func (s *Store) SetBenchmarkSpec(id uint, spec string) error {
	return s.db.Model(&BenchmarkRun{}).Where("id = ?", id).Update("search", spec).Error
}

func (s *Store) CancelBenchmarkRun(id uint, reason string) error {
	run, err := s.GetBenchmarkRun(id)
	if err != nil {
		return err
	}
	if terminalBenchmarkState(run.State) {
		return nil
	}
	now := time.Now().UTC()
	updates := map[string]any{"state": "canceled", "error": reason, "ended_at": now, "updated_at": now}
	result := s.db.Model(&BenchmarkRun{}).
		Where("id = ? AND state IN ?", id, []string{"", "pending", "running"}).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 && s.Bus != nil {
		s.Bus.PublishChange("benchmark", fmt.Sprint(id), "canceled")
	}
	return nil
}

func (s *Store) DeleteBenchmarkRun(id uint) error {
	run, err := s.GetBenchmarkRun(id)
	if err != nil {
		return err
	}
	if run.State == "pending" || run.State == "running" {
		return ErrBenchmarkActive
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("run_id = ?", id).Delete(&BenchmarkPoint{}).Error; err != nil {
			return err
		}
		return tx.Delete(&BenchmarkRun{}, id).Error
	}); err != nil {
		return err
	}
	if s.Bus != nil {
		s.Bus.PublishChange("benchmark", fmt.Sprint(id), "deleted")
	}
	return nil
}

func (s *Store) ApplyBenchmarkStatus(nodeID uint, st proto.BenchmarkStatus) error {
	run, err := s.GetBenchmarkRun(st.ID)
	if err != nil {
		return err
	}
	if run.LoadOrigin != "server" {
		n, err := s.NodeByID(nodeID)
		if err != nil || n.UUID != run.LoadOrigin {
			return errors.New("benchmark status node mismatch")
		}
	}
	if terminalBenchmarkState(run.State) {
		if run.State == "canceled" && len(st.Warmup) > 0 {
			// Enrich cancellation with completed warmup attempts without reopening the
			// run or replacing the user's cancellation diagnostic and measured points.
			var retained proto.BenchmarkStatus
			if json.Unmarshal([]byte(run.Progress), &retained) == nil {
				retained.Warmup = retainBenchmarkWarmup(retained.Warmup, st.Warmup)
				if err := s.db.Model(&BenchmarkRun{}).Where("id = ? AND state = ? AND progress = ?", run.ID, "canceled", run.Progress).Update("progress", string(mustJSON(retained))).Error; err != nil {
					return err
				}
			}
		}
		// Cancellation is persisted before the executor is stopped. Accept only
		// its final canceled point evidence without reopening the run or
		// overwriting an already retained point.
		if run.State == "canceled" && (st.State == "running" || st.State == "canceled") && len(st.Point) > 0 {
			return s.preserveCanceledBenchmarkPoint(run.ID, st.Point)
		}
		return nil
	}
	if !validBenchmarkTransition(run.State, st.State) {
		return fmt.Errorf("invalid benchmark state transition %q -> %q", run.State, st.State)
	}
	if run.Progress != "" {
		// Measured progress omits the unchanged warmup payload on the control lane.
		// Keep the earlier diagnostic in the existing progress document.
		var previous proto.BenchmarkStatus
		if json.Unmarshal([]byte(run.Progress), &previous) == nil {
			st.Warmup = retainBenchmarkWarmup(previous.Warmup, st.Warmup)
		}
	}
	now := time.Now().UTC()
	updates := map[string]any{"state": st.State, "progress": string(mustJSON(st)), "error": st.Message, "updated_at": now}
	if len(st.Warnings) > 0 {
		var warnings []string
		if run.Warnings != "" {
			_ = json.Unmarshal([]byte(run.Warnings), &warnings)
		}
		seen := make(map[string]bool, len(warnings)+len(st.Warnings))
		for _, warning := range warnings {
			seen[warning] = true
		}
		for _, warning := range st.Warnings {
			if warning != "" && !seen[warning] {
				warnings = append(warnings, warning)
				seen[warning] = true
			}
		}
		updates["warnings"] = string(mustJSON(warnings))
	}
	if st.State == "running" && run.StartedAt == nil {
		updates["started_at"] = now
	}
	if st.State == "done" || st.State == "failed" || st.State == "canceled" {
		updates["ended_at"] = now
	}
	// Claim the transition before changing point evidence. Both writes share
	// one transaction, so a canceled/done/failed run cannot be overwritten by
	// a report that read an earlier active state, even when it carries a point.
	allowed := []string{"", "pending", "running"}
	if st.State == "pending" {
		allowed = []string{"", "pending"}
	}
	applied := false
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&BenchmarkRun{}).
			Where("id = ? AND state IN ?", st.ID, allowed).Updates(updates)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		applied = true
		scoped := &Store{db: tx}
		if len(st.Point) > 0 {
			if p, ok := decodeBenchmarkPoint(st.Point); ok {
				p.RunID = st.ID
				if err := scoped.UpsertBenchmarkPoint(&p); err != nil {
					return err
				}
			}
		}
		if st.State == "done" || st.State == "failed" {
			points, err := scoped.ListBenchmarkPoints(st.ID)
			if err != nil {
				return err
			}
			if verdict, ok := computeBenchmarkVerdict(run, points, st); ok {
				return tx.Model(&BenchmarkRun{}).Where("id = ?", st.ID).
					Update("verdict", string(mustJSON(verdict))).Error
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if !applied {
		// Cancellation may have won after the initial read. Only its final
		// partial point is still admissible; ordinary late results are ignored.
		current, err := s.GetBenchmarkRun(st.ID)
		if err != nil {
			return err
		}
		if current.State == "canceled" && (st.State == "running" || st.State == "canceled") && len(st.Point) > 0 {
			return s.preserveCanceledBenchmarkPoint(st.ID, st.Point)
		}
		return nil
	}
	if s.Bus != nil {
		s.Bus.PublishChange("benchmark", fmt.Sprint(st.ID), "status")
	}
	return nil
}

// benchmarkPointWire is the protocol DTO for status.benchmark. It must stay
// separate from the GORM entity because agents use stable snake_case JSON.
type benchmarkPointWire = proto.BenchmarkPointResult

func decodeBenchmarkPoint(raw []byte) (BenchmarkPoint, bool) {
	var w benchmarkPointWire
	var legacy BenchmarkPoint
	if err := json.Unmarshal(raw, &w); err != nil {
		return BenchmarkPoint{}, false
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return BenchmarkPoint{}, false
	}
	if w.Seq == 0 {
		w.Seq = legacy.Seq
	}
	if w.ID == 0 {
		w.ID = legacy.ID
	}
	if w.Concurrency == 0 {
		w.Concurrency = legacy.Concurrency
	}
	if w.TargetISL == 0 {
		w.TargetISL = legacy.TargetISL
	}
	if w.TargetOSL == 0 {
		w.TargetOSL = legacy.TargetOSL
	}
	if w.RequestRate == 0 {
		w.RequestRate = legacy.RequestRate
	}
	if w.Repeat == 0 {
		w.Repeat = legacy.Repeat
	}
	if w.State == "" {
		w.State = legacy.State
	}
	if w.ActualISL == 0 {
		w.ActualISL = legacy.ActualISL
	}
	if w.ActualOSL == 0 {
		w.ActualOSL = legacy.ActualOSL
	}
	if w.InputTokens == 0 {
		w.InputTokens = legacy.InputTokens
	}
	if w.OutputTokens == 0 {
		w.OutputTokens = legacy.OutputTokens
	}
	if w.Success == 0 {
		w.Success = legacy.Success
	}
	if w.Errors == 0 {
		w.Errors = legacy.Errors
	}
	if w.OutputTput == 0 {
		w.OutputTput = legacy.OutputTput
	}
	if w.TotalTput == 0 {
		w.TotalTput = legacy.TotalTput
	}
	if w.MaxOutputTput == 0 {
		w.MaxOutputTput = legacy.MaxOutputTput
	}
	if w.MaxTotalTput == 0 {
		w.MaxTotalTput = legacy.MaxTotalTput
	}
	if w.RequestTput == 0 {
		w.RequestTput = legacy.RequestTput
	}
	if w.AvgTTFT == 0 {
		w.AvgTTFT = legacy.AvgTTFT
	}
	if w.P50TTFT == 0 {
		w.P50TTFT = legacy.P50TTFT
	}
	if w.P99TTFT == 0 {
		w.P99TTFT = legacy.P99TTFT
	}
	if w.AvgTPOT == 0 {
		w.AvgTPOT = legacy.AvgTPOT
	}
	if w.P50TPOT == 0 {
		w.P50TPOT = legacy.P50TPOT
	}
	if w.P99TPOT == 0 {
		w.P99TPOT = legacy.P99TPOT
	}
	if w.AvgLatency == 0 {
		w.AvgLatency = legacy.AvgLatency
	}
	if w.P50Latency == 0 {
		w.P50Latency = legacy.P50Latency
	}
	if w.P99Latency == 0 {
		w.P99Latency = legacy.P99Latency
	}
	if w.DetailPath == "" {
		w.DetailPath = legacy.DetailPath
	}
	if w.StartedAt == nil {
		w.StartedAt = legacy.StartedAt
	}
	if w.EndedAt == nil {
		w.EndedAt = legacy.EndedAt
	}
	if len(w.LongTail) == 0 && legacy.LongTail != "" {
		w.LongTail = json.RawMessage(legacy.LongTail)
	}
	if w.Seq <= 0 {
		return BenchmarkPoint{}, false
	}
	p := BenchmarkPoint{ID: w.ID, Seq: w.Seq, Concurrency: w.Concurrency, TargetISL: w.TargetISL, TargetOSL: w.TargetOSL, RequestRate: w.RequestRate, Repeat: w.Repeat, State: w.State, ActualISL: w.ActualISL, ActualOSL: w.ActualOSL, InputTokens: w.InputTokens, OutputTokens: w.OutputTokens, Success: w.Success, Errors: w.Errors, OutputTput: w.OutputTput, TotalTput: w.TotalTput, MaxOutputTput: w.MaxOutputTput, MaxTotalTput: w.MaxTotalTput, RequestTput: w.RequestTput, AvgTTFT: w.AvgTTFT, P50TTFT: w.P50TTFT, P99TTFT: w.P99TTFT, AvgTPOT: w.AvgTPOT, P50TPOT: w.P50TPOT, P99TPOT: w.P99TPOT, AvgLatency: w.AvgLatency, P50Latency: w.P50Latency, P99Latency: w.P99Latency, DetailPath: w.DetailPath, StartedAt: w.StartedAt, EndedAt: w.EndedAt}
	if len(w.LongTail) > 0 {
		p.LongTail = string(w.LongTail)
	}
	return p, true
}
func terminalBenchmarkState(s string) bool { return s == "done" || s == "failed" || s == "canceled" }

func validBenchmarkTransition(from, to string) bool {
	if to == "" {
		return false
	}
	if from == "" || from == "pending" {
		return to == "pending" || to == "running" || terminalBenchmarkState(to)
	}
	if from == "running" {
		return to == "running" || terminalBenchmarkState(to)
	}
	return false
}
