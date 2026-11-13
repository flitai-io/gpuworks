package benchrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/search"
)

const warmupRecoveryWindow = 6 * time.Minute
const warmupRetryInterval = 5 * time.Second

func (e *execution) report(st proto.BenchmarkStatus) {
	if len(e.warmupRecords) > 0 && (st.State != "running" || (st.CurrentPoint != nil && st.CurrentPoint.Warmup)) {
		st.Warmup, _ = json.Marshal(e.warmupRecords)
	}
	e.hooks.Report(st)
}

// warmup's recovery window is never shorter than one request's timeout, so a
// slow first request is bounded by its timeout and not cut off by the window.
func (e *execution) warmup(ctx context.Context, first search.Point) error {
	return e.warmupRecovery(ctx, first, max(warmupRecoveryWindow, e.doc.RequestTimeout()), warmupRetryInterval)
}

// Warmup probes are never measured point requests. After the configured batch
// fails transiently, one probe at a time waits for the endpoint to recover.
func (e *execution) warmupRecovery(ctx context.Context, first search.Point, window, interval time.Duration) error {
	n := e.doc.WarmupRequests
	if n <= 0 {
		return nil
	}
	dc := e.dc
	if dc.Kind != "agentic" {
		dc.InputTokens, dc.OutputTokens = first.ISL, first.OSL
	}
	dc.Seed += 7919 // Warmup must not prime the first point's own prompt prefixes.
	reqs, err := e.requests(dc, n)
	if err != nil {
		return err
	}
	for i := range reqs {
		reqs[i].FollowUps, reqs[i].DelayBefore = nil, 0
		reqs[i].SessionID = fmt.Sprintf("warmup-%d", i+1)
	}
	concurrency := n
	if first.Concurrency > 0 {
		concurrency = min(concurrency, first.Concurrency)
	}
	current := &proto.BenchmarkCurrentPoint{Concurrency: concurrency, TargetISL: first.ISL, TargetOSL: first.OSL, RequestTotal: n, Warmup: true}
	recovery, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	e.report(proto.BenchmarkStatus{ID: e.run.ID, State: "running", CurrentPoint: current})
	for {
		results := (loadgen.Runner{Client: e.run.Client, Concurrency: concurrency}).Run(recovery, reqs)
		retry := len(results) > 0
		var diagnostic string
		for _, r := range results {
			d := loadgen.NewDetailRecord(r)
			d.RunID = e.run.ID
			d.ErrorText = loadgen.ResponseErrorText(d.ErrorText, e.run.Client.APIKey)
			d.Text, d.Usage, d.Metrics.ChunkIntervalsMs = "", nil, nil
			d.Attempt = 1
			for _, previous := range e.warmupRecords {
				if previous.SessionID == d.SessionID {
					d.Attempt++
				}
			}
			e.warmupRecords = append(e.warmupRecords, d)
			if r.Metrics.Error == nil {
				e.warmupSucceeded++
			} else {
				retry = retry && retryableWarmup(r)
				if diagnostic == "" || !retryableWarmup(r) {
					diagnostic = warmupDiagnostic(d)
				}
			}
		}
		e.report(proto.BenchmarkStatus{ID: e.run.ID, State: "running", CurrentPoint: current, Completed: min(n, e.warmupSucceeded)})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.warmupSucceeded > 0 {
			return nil
		}
		failure := fmt.Sprintf("all %d warmup requests failed after %d attempts: %s", n, len(e.warmupRecords), diagnostic)
		if recovery.Err() != nil {
			return fmt.Errorf("%s; recovery window %s exhausted", failure, window)
		}
		if !retry {
			return errors.New(failure)
		}
		e.report(proto.BenchmarkStatus{ID: e.run.ID, State: "running", CurrentPoint: current, Message: fmt.Sprintf("Warmup recovery: %s; retrying in %s (budget %s)", diagnostic, interval, window)})
		timer := time.NewTimer(interval)
		select {
		case <-recovery.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s; recovery window %s exhausted", failure, window)
		case <-timer.C:
		}
		reqs = reqs[:1]
		concurrency = 1
	}
}

func retryableWarmup(r loadgen.Result) bool {
	if errors.Is(r.Metrics.Error, context.Canceled) {
		return false
	}
	switch r.Metrics.HTTPStatus {
	case 408, 429, 500, 502, 503, 504:
		return true
	}
	if r.Metrics.ErrorStage != "transport" && r.Metrics.ErrorStage != "stream" {
		return false
	}
	return transientWarmupError(r.Metrics.Error)
}

func transientWarmupError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	for _, cause := range []error{context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH} {
		if errors.Is(err, cause) {
			return true
		}
	}
	var network net.Error
	return errors.As(err, &network) && (network.Timeout() || network.Temporary()) //nolint:staticcheck // SA1019: warmup retries include legacy temporary DNS and transport failures.
}

func warmupDiagnostic(d loadgen.DetailRecord) string {
	message := loadgen.ResponseErrorText(d.ErrorText)
	message = strings.Join(strings.Fields(message), " ")
	if runes := []rune(message); len(runes) > 512 {
		message = string(runes[:512]) + "… (full diagnostic in warmup report)"
	}
	status := "unknown"
	if d.HTTPStatus > 0 {
		status = fmt.Sprint(d.HTTPStatus)
	}
	return fmt.Sprintf("HTTP %s, stage %s: %s", status, d.ErrorStage, message)
}
