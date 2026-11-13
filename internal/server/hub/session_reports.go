package hub

import (
	"encoding/json"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

// Status reports leave the read loop by one of two paths, never as an inline
// store write. A full snapshot of a node's fact (status.node, status.storage)
// goes through a coalescing nodeQueue (statusqueue.go); a delta, a chunk or a
// progress step (instance, model file, benchmark, image operation) goes
// through the session's ordered worker below. Both write under applyCurrent,
// so a report from a replaced session never lands, and no store I/O holds
// hub.mu: register and every hub.session() lookup (the gateway's tunnel
// opens among them) would otherwise queue behind a fleet's status storm.
// Heartbeats are the one report without the fence (newTouchQueue).

// sessionReportDepth bounds one session's unapplied ordered reports. Past it
// the session's own read loop waits for its worker: ordered reports cannot be
// dropped, and back-pressure confined to one node is what the store's single
// writer can honestly offer. Other sessions are unaffected. Progress reports
// coalesce per instance (session_report_queue.go), so a storm of them holds
// at most two pending reports per instance.
const sessionReportDepth = 1024

// applyCurrent runs write with the session's authenticated node ID only while
// s is still the node's registered session, and reports whether it applied.
// reportMu keeps the check and the write together without holding hub.mu
// across store I/O. register and deregister wait on the same mutex
// (fenceReports) once s left the table, so a write s validated finishes
// before the next session's first report or the node's offline mark.
func (s *Session) applyCurrent(write func(nodeID uint) error) (bool, error) {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	if s.nodeID == 0 || s.hub.session(s.NodeUUID) != s {
		return false, nil
	}
	if err := write(s.nodeID); err != nil {
		return false, err
	}
	return true, nil
}

// fenceReports returns once no report write or coalesced submit of s is in
// flight. Called after s left the session table, it guarantees none starts
// afterwards either.
func (s *Session) fenceReports() {
	s.submitMu.Lock()
	s.submitMu.Unlock() //nolint:staticcheck // SA2001: wait for in-flight submits, then release before waiting on report I/O.
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
}

// submitReport queues job on the session's ordered worker. Only the read loop
// submits, so jobs run in receipt order. A job offered after the session closed
// may be dropped; applyCurrent would refuse it anyway. ctrlLoop drains before
// it marks the session closed, so a normal disconnect drops nothing.
func (s *Session) submitReport(job func()) { s.pushReport(&reportJob{run: job}) }

func (s *Session) pushReport(job *reportJob) {
	q := s.reportQ.Load()
	if q == nil {
		if fresh := newReportQueue(); s.reportQ.CompareAndSwap(nil, fresh) {
			s.hub.trackSessionWork(func() { s.runReports(fresh) })
		}
		q = s.reportQ.Load()
	}
	q.push(job, s.isClosed)
}

func (s *Session) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// drainReports applies what the session already received before its read loop
// ends, so reports sent just before a disconnect still land while the session
// is current. Called on the read loop, after its last submitReport.
func (s *Session) drainReports() {
	q := s.reportQ.Load()
	if q == nil {
		return
	}
	q.close()
	<-q.done
}

func (s *Session) handleInstanceStatus(raw json.RawMessage) {
	var st proto.InstanceStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		s.hub.log.Warn("bad status.instance", "node", s.NodeUUID, "err", err)
		return
	}
	s.submitInstanceStatus(st)
}

// applyInstanceReport persists one instance report through the full path.
func (s *Session) applyInstanceReport(st proto.InstanceStatus) {
	accepted, err := s.hub.applyInstanceStatus(s, st)
	if err != nil {
		s.hub.log.Debug("apply status.instance", "uuid", st.UUID, "err", err)
	}
	if accepted {
		s.rememberAssetDigests(st.RuntimeAssetDigests)
	}
}

func (s *Session) handleModelFileStatus(raw json.RawMessage) {
	var st proto.ModelFileStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return
	}
	s.submitReport(func() { _ = s.hub.applyModelFileStatus(s, st) })
}

// handleStorageStatus queues the report: each one lists every storage the
// node mounts, so only the newest per node needs to persist.
func (s *Session) handleStorageStatus(raw json.RawMessage) {
	var report proto.StorageStatusReport
	if err := json.Unmarshal(raw, &report); err != nil {
		s.hub.log.Warn("bad status.storage", "node", s.NodeUUID, "err", err)
		return
	}
	// Agents send a storage report only when it changed (or on a desired-state
	// replay), so a replaced session must not take the slot of its
	// replacement's pending snapshot. Checking and submitting under submitMu
	// orders this against register's fence: either s was still current and its
	// report is superseded by anything the new session sends, or it is
	// skipped. submitMu is never held across store I/O, so this never waits
	// on a write.
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	if s.hub.session(s.NodeUUID) != s {
		return
	}
	s.hub.storageQ.submit(s.NodeUUID, queuedStorageStatus{report: report, session: s})
}

func (s *Session) handleBenchmarkStatus(raw json.RawMessage) {
	var st proto.BenchmarkStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		s.hub.log.Warn("bad status.benchmark", "node", s.NodeUUID, "err", err)
		return
	}
	s.submitReport(func() {
		accepted, err := s.applyCurrent(func(nodeID uint) error {
			if err := s.hub.store.ApplyBenchmarkStatus(nodeID, st); err != nil {
				return err
			}
			if len(st.Sample) > 0 && st.PointIndex > 0 {
				var sample metrics.ProcessSample
				if json.Unmarshal(st.Sample, &sample) == nil {
					_ = s.hub.store.AppendBenchmarkCurveSample(st.ID, st.PointIndex, time.Now().UTC(), sample)
				}
			}
			return nil
		})
		if err != nil {
			s.hub.log.Debug("apply status.benchmark", "id", st.ID, "err", err)
		}
		if !accepted {
			return
		}
		s.benchmarkStatusApplied(st)
	})
}

// benchmarkStatusApplied forwards an applied benchmark report to the detail
// and metrics sinks and retires a finished run from the agent's desired state.
func (s *Session) benchmarkStatusApplied(st proto.BenchmarkStatus) {
	if (len(st.DetailChunk) > 0 || st.DetailFinal) && s.hub.onBenchmarkDetail != nil {
		s.hub.onBenchmarkDetail(s.NodeUUID, st)
	}
	if len(st.Sample) > 0 && s.hub.onMetrics != nil {
		var sample metrics.ProcessSample
		if json.Unmarshal(st.Sample, &sample) == nil {
			now := time.Now().UnixMilli()
			labels := map[string]string{"benchmark": fmt.Sprint(st.ID), "point": fmt.Sprint(st.PointIndex), "node": s.NodeUUID}
			s.hub.onMetrics(s.NodeUUID, []proto.MetricSample{{Metric: "gpuworks_bench_output_tput", Labels: labels, Value: sample.OutputTput, TimeMS: now}, {Metric: "gpuworks_bench_total_tput", Labels: labels, Value: sample.TotalTput, TimeMS: now}, {Metric: "gpuworks_bench_request_tput", Labels: labels, Value: sample.RequestTput, TimeMS: now}, {Metric: "gpuworks_bench_ttft_p50_ms", Labels: labels, Value: sample.TTFTP50, TimeMS: now}, {Metric: "gpuworks_bench_tpot_p50_ms", Labels: labels, Value: sample.TPOTP50, TimeMS: now}, {Metric: "gpuworks_bench_latency_p99_ms", Labels: labels, Value: sample.LatencyP99, TimeMS: now}})
		}
	}
	if st.State == "done" || st.State == "failed" || st.State == "canceled" {
		_ = s.PushUpdate(proto.StateUpdate{RemovedBenchmarks: []uint{st.ID}})
	}
}
