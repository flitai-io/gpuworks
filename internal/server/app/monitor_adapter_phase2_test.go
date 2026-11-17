package app

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// buildPhase2Fixture extends the shared newMonitorFixtureBase deployment
// with proxy accounting counters and ingest-time histogram quantile gauges
// (the phase2 metrics contract's own shapes - proxy/accounting.go and
// observability's quantile-at-ingest conversion), so the app-layer e2e
// tests below exercise the whole stack: store -> monitorInventoryAdapter ->
// monitor.ComputeWindow's phase2 fields -> monitorapi's wire DTO.
func buildPhase2Fixture(t *testing.T) monitorFixture {
	t.Helper()
	fx := newMonitorFixtureBase(t)
	depKey := strconv.FormatUint(uint64(fx.dep.ID), 10)
	depLabels := map[string]string{"deployment": depKey}
	instLabels := map[string]string{"deployment": depKey, "instance": fx.inst.UUID}

	start := fx.now.Add(-100 * time.Second)
	for i := 0; i < 11; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_requests_running", instLabels, 10, at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_requests_waiting", instLabels, 0, at)
		// TTFT p95 3s, TPOT p95 20ms, queue and prefill times, persisted for
		// the full 100s run.
		withQuantile := func(q string) map[string]string {
			m := map[string]string{"quantile": q}
			for k, v := range instLabels {
				m[k] = v
			}
			return m
		}
		writeMonitorSample(t, fx.ts, "gpuworks_infer_ttft_seconds", withQuantile("0.5"), 2.0, at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_ttft_seconds", withQuantile("0.95"), 3.0, at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_ttft_seconds", withQuantile("0.99"), 3.5, at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_tpot_seconds", withQuantile("0.95"), 0.02, at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_queue_time_seconds", withQuantile("0.95"), 0.1, at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_prefill_time_seconds", withQuantile("0.95"), 2.1, at)

		// Proxy accounting: cumulative counters growing by 100 requests per
		// tick (95 2xx, 5 5xx - a 5% error rate).
		n := float64(i + 1)
		writeMonitorSample(t, fx.ts, "gpuworks_proxy_requests_total", withClass(depLabels, "2xx"), 95*n, at)
		writeMonitorSample(t, fx.ts, "gpuworks_proxy_requests_total", withClass(depLabels, "5xx"), 5*n, at)
		// The engine's own completion counter: alignment reconciles the proxy
		// against it and is absent without one.
		writeMonitorSample(t, fx.ts, "gpuworks_infer_requests_success_total", instLabels, 95*n, at)
	}
	return fx
}

// buildPhase2IdleWindowFixture writes the same proxy/ttft/tpot shape as
// buildPhase2Fixture, but the whole run ends 10 minutes before "now" -
// comfortably outside the 5-minute recent window but still inside
// the 1-hour chart range the tests below query. This is the "quiet window,
// not a platform gap" state ClusterSignals.ProxyNoRecent/TTFTNoRecent/
// TPOTNoRecent exist for: proxy_requests_total's flush and the
// quantile-at-ingest fold both only write a sample for a tick that actually
// saw activity (proxy/accounting.go's flush, obsingest/quantile.go's
// applyQuantiles "total <= 0" early return), so a deployment quiet for the
// last five minutes has literally no sample in that window even though it
// was busy minutes before.
func buildPhase2IdleWindowFixture(t *testing.T) monitorFixture {
	t.Helper()
	fx := newMonitorFixtureBase(t)
	depKey := strconv.FormatUint(uint64(fx.dep.ID), 10)
	depLabels := map[string]string{"deployment": depKey}
	instLabels := map[string]string{"deployment": depKey, "instance": fx.inst.UUID}

	start := fx.now.Add(-10*time.Minute - 100*time.Second)
	for i := 0; i < 11; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		withQuantile := func(q string) map[string]string {
			m := map[string]string{"quantile": q}
			for k, v := range instLabels {
				m[k] = v
			}
			return m
		}
		writeMonitorSample(t, fx.ts, "gpuworks_infer_ttft_seconds", withQuantile("0.5"), 2.0, at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_ttft_seconds", withQuantile("0.95"), 3.0, at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_ttft_seconds", withQuantile("0.99"), 3.5, at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_tpot_seconds", withQuantile("0.95"), 0.02, at)

		n := float64(i + 1)
		writeMonitorSample(t, fx.ts, "gpuworks_proxy_requests_total", withClass(depLabels, "2xx"), 95*n, at)
		writeMonitorSample(t, fx.ts, "gpuworks_proxy_requests_total", withClass(depLabels, "5xx"), 5*n, at)
	}
	return fx
}

func withClass(base map[string]string, class string) map[string]string {
	m := map[string]string{"class": class}
	for k, v := range base {
		m[k] = v
	}
	return m
}

// TestMonitorServiceDeploymentPhase2FieldsEndToEnd proves the whole phase2
// stack - proxy accounting and quantile-at-ingest gauges - reaches the
// deployment page's wire DTO: TTFT/queue/prefill in the breakdown bar and a
// populated alignment row (proxy traffic exists).
func TestMonitorServiceDeploymentPhase2FieldsEndToEnd(t *testing.T) {
	fx := buildPhase2Fixture(t)
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil)

	out, err := svc.Deployment(context.Background(), fx.dep.ID, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if out.TTFTBreakdown == nil {
		t.Fatal("TTFTBreakdown = nil, want populated (TTFT collected)")
	}
	if out.TTFTBreakdown.TotalMs != 3000 {
		t.Errorf("TTFTBreakdown.TotalMs = %v, want 3000", out.TTFTBreakdown.TotalMs)
	}
	if out.Alignment == nil {
		t.Fatal("Alignment = nil, want populated (proxy traffic exists)")
	}
	if out.Alignment.ProxyRequests == 0 {
		t.Errorf("Alignment.ProxyRequests = 0, want > 0")
	}
	if len(out.Charts.Latency) == 0 {
		t.Error("Charts.Latency = empty, want ttft series")
	}
	if len(out.Charts.TPOT) == 0 {
		t.Error("Charts.TPOT = empty, want tpot series")
	}
	for _, series := range out.Charts.Latency {
		if strings.HasPrefix(series.Labels["metric"], "tpot") {
			t.Errorf("Charts.Latency carries %q; TPOT has its own chart", series.Labels["metric"])
		}
	}
}

// TestMonitorServiceOverviewPhase2SignalsEndToEnd proves the cluster
// overview's signal row surfaces the same phase2 data: error_rate,
// request_rate and ttft all collected with real values.
func TestMonitorServiceOverviewPhase2SignalsEndToEnd(t *testing.T) {
	fx := buildPhase2Fixture(t)
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil)

	out, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	byKey := map[string]int{}
	for i, s := range out.Signals {
		byKey[s.Key] = i
	}
	errRate := out.Signals[byKey["error_rate"]]
	if !errRate.Collected || errRate.Value == nil || *errRate.Value <= 0 {
		t.Fatalf("error_rate signal = %+v, want collected with a positive value", errRate)
	}
	reqRate := out.Signals[byKey["request_rate"]]
	if !reqRate.Collected || reqRate.Value == nil || *reqRate.Value <= 0 {
		t.Fatalf("request_rate signal = %+v, want collected with a positive value", reqRate)
	}
	ttft := out.Signals[byKey["ttft"]]
	if !ttft.Collected || ttft.Value == nil || *ttft.Value != 3000 {
		t.Fatalf("ttft signal = %+v, want collected value=3000ms", ttft)
	}

	if len(out.Services) != 1 {
		t.Fatalf("Services = %+v, want 1", out.Services)
	}
	svcRow := out.Services[0]
	if svcRow.RequestRate == nil || *svcRow.RequestRate <= 0 {
		t.Fatalf("services[0].RequestRate = %v, want a positive value", svcRow.RequestRate)
	}
	if svcRow.TTFTP95Ms == nil || *svcRow.TTFTP95Ms != 3000 {
		t.Fatalf("services[0].TTFTP95Ms = %v, want 3000", svcRow.TTFTP95Ms)
	}
}

// TestMonitorServiceOverviewPhase2SignalsIdleWindowEndToEnd proves the
// cluster overview reports request_rate/error_rate/ttft/tpot as
// collected=true - with a real zero value for the rate signals and an
// omitted value/denominator for ttft/tpot - plus detail.no_recent_requests
// = true, when the underlying family has data somewhere in the chart range
// but nothing in the last five minutes. Before this fix all four read
// collected=false, indistinguishable on the wire from "this platform never
// collects this metric" (see monitor.ClusterSignals.ProxyNoRecent/
// TTFTNoRecent/TPOTNoRecent's doc comments).
func TestMonitorServiceOverviewPhase2SignalsIdleWindowEndToEnd(t *testing.T) {
	fx := buildPhase2IdleWindowFixture(t)
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil)

	out, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	byKey := map[string]int{}
	for i, s := range out.Signals {
		byKey[s.Key] = i
	}

	errRate := out.Signals[byKey["error_rate"]]
	if !errRate.Collected || errRate.Value == nil || *errRate.Value != 0 {
		t.Fatalf("error_rate signal = %+v, want collected/0 (quiet window, not a platform gap)", errRate)
	}
	if errRate.Detail["no_recent_requests"] != true {
		t.Fatalf("error_rate.Detail = %+v, want no_recent_requests:true", errRate.Detail)
	}
	if errRate.Detail["5xx"] != 0.0 || errRate.Detail["429"] != 0.0 || errRate.Detail["asleep_503"] != 0.0 {
		t.Fatalf("error_rate.Detail = %+v, want 5xx/429/asleep_503 all 0", errRate.Detail)
	}

	reqRate := out.Signals[byKey["request_rate"]]
	if !reqRate.Collected || reqRate.Value == nil || *reqRate.Value != 0 {
		t.Fatalf("request_rate signal = %+v, want collected/0", reqRate)
	}
	if reqRate.Detail["no_recent_requests"] != true {
		t.Fatalf("request_rate.Detail = %+v, want no_recent_requests:true", reqRate.Detail)
	}

	ttft := out.Signals[byKey["ttft"]]
	if !ttft.Collected || ttft.Value != nil || ttft.Denominator != nil {
		t.Fatalf("ttft signal = %+v, want collected but nil value/denominator (no fresh sample this window)", ttft)
	}
	if ttft.Detail["upper_bound"] != true || ttft.Detail["no_recent_requests"] != true {
		t.Fatalf("ttft.Detail = %+v, want upper_bound and no_recent_requests both true", ttft.Detail)
	}

	tpot := out.Signals[byKey["tpot"]]
	if !tpot.Collected || tpot.Value != nil || tpot.Denominator != nil {
		t.Fatalf("tpot signal = %+v, want collected but nil value/denominator", tpot)
	}
	if tpot.Detail["upper_bound"] != true || tpot.Detail["no_recent_requests"] != true {
		t.Fatalf("tpot.Detail = %+v, want upper_bound and no_recent_requests both true", tpot.Detail)
	}

	if len(out.Services) != 1 {
		t.Fatalf("Services = %+v, want 1", out.Services)
	}
	svcRow := out.Services[0]
	if svcRow.RequestRate == nil || *svcRow.RequestRate != 0 {
		t.Fatalf("services[0].RequestRate = %v, want 0, not null (this deployment's own proxy series exist in range)", svcRow.RequestRate)
	}
	if svcRow.TTFTP95Ms != nil {
		t.Fatalf("services[0].TTFTP95Ms = %v, want nil (the row carries no detail to explain a stale ttft)", svcRow.TTFTP95Ms)
	}
}
