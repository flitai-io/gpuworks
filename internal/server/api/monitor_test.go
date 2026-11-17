package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fakeMonitorService is a hand-written MonitorService the handler tests
// drive directly: it records every call's arguments so a test can assert on
// exactly what range/step/by/fn/id reached the interface, and it can be
// told to fail a call (including with the sentinel ErrMonitorDeploymentNotFound)
// to exercise error mapping - a fake that only ever returned success would
// never prove monitorDeployment's 404 branch or monitorOverview/Series's 500
// branch actually run.
type fakeMonitorService struct {
	// health answers Health for a deployment with the check on.
	health dto.DeploymentHealth

	overview     dto.MonitorOverview
	overviewErr  error
	overviewArgs []time.Duration

	deployment     dto.MonitorDeployment
	deploymentErr  error
	deploymentArgs []struct {
		id  uint
		rng time.Duration
	}

	series     []dto.MonitorSeries
	seriesErr  error
	seriesArgs []struct {
		metric, by, fn string
		rng, step      time.Duration
	}

	// catalog replaces the default one-entry metric catalog when set.
	catalog []dto.MonitorMetricSeries

	// ends records the picked ranges' ends asked for through At.
	ends []time.Time
}

func (f *fakeMonitorService) At(end time.Time) MonitorService {
	f.ends = append(f.ends, end)
	return f
}

func (f *fakeMonitorService) Overview(_ context.Context, rng time.Duration) (dto.MonitorOverview, error) {
	f.overviewArgs = append(f.overviewArgs, rng)
	return f.overview, f.overviewErr
}

func (f *fakeMonitorService) Deployment(_ context.Context, id uint, rng time.Duration) (dto.MonitorDeployment, error) {
	f.deploymentArgs = append(f.deploymentArgs, struct {
		id  uint
		rng time.Duration
	}{id, rng})
	return f.deployment, f.deploymentErr
}

func (f *fakeMonitorService) Series(_ context.Context, metric, by, fn string, rng, step time.Duration) ([]dto.MonitorSeries, error) {
	f.seriesArgs = append(f.seriesArgs, struct {
		metric, by, fn string
		rng, step      time.Duration
	}{metric, by, fn, rng, step})
	return f.series, f.seriesErr
}

func (f *fakeMonitorService) Metrics() []dto.MonitorMetricSeries {
	if f.catalog != nil {
		return f.catalog
	}
	return []dto.MonitorMetricSeries{{Series: "gpuworks_proxy_ttft_ms", Name: "ttft_ms", Source: "gateway", Kind: "histogram", Unit: "milliseconds", Quantiles: true}}
}

func monitorTestToken(t *testing.T) (*store.Store, *auth.TokenSigner, string) {
	t.Helper()
	signer, err := auth.NewTokenSigner("monitor-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	return st, signer, token
}

func TestMonitorOverviewRequiresAuthentication(t *testing.T) {
	st, signer, _ := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/monitor/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", rec.Code, rec.Body.String())
	}
}

func TestMonitorOverviewSuccessAndRangeDefault(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	fake := &fakeMonitorService{overview: dto.MonitorOverview{
		AsOf: 1000, Signals: []dto.MonitorSignal{{Key: "ready_replicas", Collected: true}},
	}}
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: fake})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/overview", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got dto.MonitorOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AsOf != 1000 || len(got.Signals) != 1 {
		t.Fatalf("decoded overview = %+v", got)
	}
	if len(fake.overviewArgs) != 1 || fake.overviewArgs[0] != time.Hour {
		t.Fatalf("overview called with %+v, want default range 1h", fake.overviewArgs)
	}
}

func TestMonitorOverviewInvalidRange(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/overview?range=3h", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != ErrorMonitorRangeInvalid {
		t.Fatalf("error code = %v, want %s", body["code"], ErrorMonitorRangeInvalid)
	}
}

func TestMonitorOverviewUpstreamErrorIs500(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{overviewErr: errors.New("tsdb unavailable")}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/overview", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", rec.Code, rec.Body.String())
	}
}

func TestMonitorOverviewNilServiceReturnsEmptyShape(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/overview", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got dto.MonitorOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Services == nil || got.Signals == nil || got.StateCounts == nil || got.VRAMByNode == nil || got.Events == nil {
		t.Fatalf("nil-Monitor overview must still carry empty (non-nil) lists: %+v", got)
	}
	if !strings.Contains(rec.Body.String(), `"state_counts":[]`) {
		t.Fatalf("nil-Monitor overview body = %s, want \"state_counts\":[]", rec.Body.String())
	}
}

func TestMonitorDeploymentSuccess(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	fake := &fakeMonitorService{deployment: dto.MonitorDeployment{
		Deployment: dto.MonitorDeploymentSummary{ID: 42, Name: "Qwen3-32B"},
		Charts:     dto.MonitorCharts{Latency: []dto.MonitorSeries{}, Concurrency: []dto.MonitorSeries{}, KV: []dto.MonitorSeries{}, Throughput: []dto.MonitorSeries{}, StageTime: []dto.MonitorSeries{}, Preemptions: []dto.MonitorSeries{}},
	}}
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: fake})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deployments/42?range=6h", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got dto.MonitorDeployment
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Deployment.ID != 42 || got.Deployment.Name != "Qwen3-32B" {
		t.Fatalf("decoded deployment = %+v", got.Deployment)
	}
	if len(fake.deploymentArgs) != 1 || fake.deploymentArgs[0].id != 42 || fake.deploymentArgs[0].rng != 6*time.Hour {
		t.Fatalf("deployment called with %+v", fake.deploymentArgs)
	}
}

func TestMonitorDeploymentInvalidID(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deployments/not-a-number", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != ErrorDeploymentIDInvalid {
		t.Fatalf("error code = %v, want %s", body["code"], ErrorDeploymentIDInvalid)
	}
}

func TestMonitorDeploymentUnknownIs404(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{deploymentErr: ErrMonitorDeploymentNotFound}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deployments/999", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

func TestMonitorDeploymentNilServiceIs404(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deployments/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

func TestMonitorDeploymentUpstreamErrorIs500(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{deploymentErr: errors.New("tsdb unavailable")}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deployments/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", rec.Code, rec.Body.String())
	}
}

func TestDefaultMonitorStepOfAPickedRange(t *testing.T) {
	// A picked range (from/to) is any length: about 240 points, in whole
	// 10 s steps, never under 10 s.
	for rng, want := range map[time.Duration]time.Duration{
		3 * time.Hour: 50 * time.Second, time.Minute: 10 * time.Second, 30 * 24 * time.Hour: 3 * time.Hour,
		2*time.Hour + time.Minute: 40 * time.Second,
	} {
		if got := defaultMonitorStep(rng); got != want {
			t.Errorf("defaultMonitorStep(%s) = %s, want %s", rng, got, want)
		}
	}
}

func TestMonitorDeploymentInvalidRange(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deployments/1?range=nope", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestMonitorSeriesSuccessWithDefaults(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	fake := &fakeMonitorService{series: []dto.MonitorSeries{{Labels: map[string]string{"node": "gpu3"}, Points: []dto.Point{{1000, 5}}}}}
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: fake})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=gpuworks_gpu_utilization", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got []dto.MonitorSeries
	if err := unmarshalList(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Labels["node"] != "gpu3" {
		t.Fatalf("decoded series = %+v", got)
	}
	if len(fake.seriesArgs) != 1 {
		t.Fatalf("series calls = %d", len(fake.seriesArgs))
	}
	args := fake.seriesArgs[0]
	if args.fn != "avg" || args.by != "" || args.rng != time.Hour || args.step != 30*time.Second {
		t.Fatalf("series args = %+v, want fn=avg by=\"\" range=1h step=30s (contract defaults)", args)
	}
}

func TestMonitorSeriesStepDefaultsPerRange(t *testing.T) {
	for _, tc := range []struct {
		rng      string
		wantRng  time.Duration
		wantStep time.Duration
	}{
		{"15m", 15 * time.Minute, 10 * time.Second},
		{"6h", 6 * time.Hour, time.Minute},
		{"24h", 24 * time.Hour, 5 * time.Minute},
		{"168h", 168 * time.Hour, 30 * time.Minute},
	} {
		t.Run(tc.rng, func(t *testing.T) {
			st, signer, token := monitorTestToken(t)
			fake := &fakeMonitorService{}
			r := Router(Deps{Store: st, AuthSigner: signer, Monitor: fake})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=m&range="+tc.rng, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if len(fake.seriesArgs) != 1 || fake.seriesArgs[0].rng != tc.wantRng || fake.seriesArgs[0].step != tc.wantStep {
				t.Fatalf("series args = %+v, want range=%s step=%s", fake.seriesArgs, tc.wantRng, tc.wantStep)
			}
		})
	}
}

func TestMonitorSeriesExplicitStepOverridesDefault(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	fake := &fakeMonitorService{}
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: fake})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=m&step=5m", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.seriesArgs) != 1 || fake.seriesArgs[0].step != 5*time.Minute {
		t.Fatalf("series args = %+v, want explicit step=5m", fake.seriesArgs)
	}
}

// TestMonitorSeriesExplicitStepBelowDefaultIsClamped is SHOULD-FIX #6: an
// explicit step finer than the range's recommended minimum is clamped up to
// that minimum, not honored as-is - a well-behaved default protects the
// series/point caps from ever being approached by an unbounded bucket
// count.
func TestMonitorSeriesExplicitStepBelowDefaultIsClamped(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	fake := &fakeMonitorService{}
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: fake})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=m&range=168h&step=1s", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.seriesArgs) != 1 || fake.seriesArgs[0].step != 30*time.Minute {
		t.Fatalf("series args = %+v, want step clamped up to 168h's 30m default", fake.seriesArgs)
	}
}

// TestMonitorSeriesTooLargeIs400 is SHOULD-FIX #6's other half: when
// MonitorService.Series reports the result would exceed its caps, the
// handler answers 400 with a stable code, never a truncated 200.
func TestMonitorSeriesTooLargeIs400(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{seriesErr: ErrMonitorSeriesTooLarge}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=m", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != ErrorMonitorSeriesTooLarge {
		t.Fatalf("error code = %v, want %s", body["code"], ErrorMonitorSeriesTooLarge)
	}
}

func TestMonitorSeriesInvalidRange(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=m&range=3h", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != ErrorMonitorRangeInvalid {
		t.Fatalf("error code = %v, want %s", body["code"], ErrorMonitorRangeInvalid)
	}
}

func TestMonitorSeriesUpstreamErrorIs500(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{seriesErr: errors.New("tsdb unavailable")}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=m", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", rec.Code, rec.Body.String())
	}
}

func TestMonitorSeriesNilResultNormalizesToEmptyArray(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	// fakeMonitorService.series is left at its zero value (nil slice) with
	// no error, to exercise the handler's own nil->[] normalization
	// independent of the "Monitor not wired at all" path.
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=m", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[],"total":0}` {
		t.Fatalf("body = %q, want the empty list envelope", got)
	}
}

func TestMonitorSeriesMetricRequired(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != ErrorMetricRequired {
		t.Fatalf("error code = %v, want %s", body["code"], ErrorMetricRequired)
	}
}

func TestMonitorSeriesInvalidGroup(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=m&by=cluster", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != ErrorMonitorGroupInvalid {
		t.Fatalf("error code = %v, want %s", body["code"], ErrorMonitorGroupInvalid)
	}
}

func TestMonitorSeriesInvalidAggregation(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=m&fn=median", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != ErrorMonitorAggregationInvalid {
		t.Fatalf("error code = %v, want %s", body["code"], ErrorMonitorAggregationInvalid)
	}
}

func TestMonitorSeriesUnknownMetricReturnsEmptyArray(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	fake := &fakeMonitorService{series: []dto.MonitorSeries{}}
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: fake})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=gpuworks_does_not_exist&range=6h&step=1m&by=node&fn=rate", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[],"total":0}` {
		t.Fatalf("body = %q, want the empty list envelope", got)
	}
	if len(fake.seriesArgs) != 1 {
		t.Fatal("Series was not called")
	}
	args := fake.seriesArgs[0]
	if args.by != "node" || args.fn != "rate" || args.rng != 6*time.Hour || args.step != time.Minute {
		t.Fatalf("series args = %+v, want the explicit range/step/by/fn passed through", args)
	}
}

func TestMonitorSeriesNilServiceReturnsEmptyArray(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	r := Router(Deps{Store: st, AuthSigner: signer})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=m", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var got []dto.MonitorSeries
	if err := unmarshalList(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("body = %+v, want an empty (non-nil) slice", got)
	}
}

// A picked range (from/to) answers from the service as of to, over to-from;
// presets stay as they were; a mix, a future end or a span out of bounds is
// refused before the service.
func TestMonitorPickedRange(t *testing.T) {
	fake := &fakeMonitorService{}
	signer, err := auth.NewTokenSigner("monitor-window", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	session, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "viewer", Role: "viewer"})
	end := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	from := end.Add(-3 * time.Hour)
	q := "from=" + strconv.FormatInt(from.UnixMilli(), 10) + "&to=" + strconv.FormatInt(end.UnixMilli(), 10)
	router := Router(Deps{Store: st, Monitor: fake, AuthSigner: signer})
	call := func(path string) (int, string) {
		rec := telemetryCall(router, http.MethodGet, path, session, "")
		return rec.Code, rec.Body.String()
	}
	for _, path := range []string{"/api/v1/monitor/overview?", "/api/v1/monitor/deployments/7?", "/api/v1/monitor/series?metric=m&", "/api/v1/monitor/deployments/7/latency?", "/api/v1/monitor/distributions?"} {
		before := len(fake.ends)
		// The fake's latency knows no deployment: 404, after At.
		if code, body := call(path + q); code != http.StatusOK && !(strings.Contains(path, "latency") && code == http.StatusNotFound) {
			t.Fatalf("%s = %d %s", path, code, body)
		}
		if len(fake.ends) != before+1 || !fake.ends[before].Equal(end) {
			t.Fatalf("%s ends = %v, want %v", path, fake.ends, end)
		}
	}
	if got := fake.overviewArgs[len(fake.overviewArgs)-1]; got != 3*time.Hour {
		t.Fatalf("overview range = %v", got)
	}
	if got := fake.seriesArgs[len(fake.seriesArgs)-1]; got.rng != 3*time.Hour || got.step != 50*time.Second {
		t.Fatalf("series = %+v", got)
	}
	// A preset does not go through At.
	before := len(fake.ends)
	if code, _ := call("/api/v1/monitor/overview?range=6h"); code != http.StatusOK || len(fake.ends) != before {
		t.Fatal("a preset went through At")
	}
	for _, bad := range []string{
		q + "&range=1h",
		"from=" + strconv.FormatInt(from.UnixMilli(), 10),
		"from=abc&to=1",
		"from=" + strconv.FormatInt(end.UnixMilli(), 10) + "&to=" + strconv.FormatInt(from.UnixMilli(), 10),
		"from=" + strconv.FormatInt(end.Add(-30*time.Second).UnixMilli(), 10) + "&to=" + strconv.FormatInt(end.UnixMilli(), 10),
		"from=" + strconv.FormatInt(end.AddDate(0, 0, -31).UnixMilli(), 10) + "&to=" + strconv.FormatInt(end.UnixMilli(), 10),
		"from=" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "&to=" + strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10),
	} {
		code, body := call("/api/v1/monitor/overview?" + bad)
		if code != http.StatusBadRequest || !strings.Contains(body, ErrorMonitorRangeInvalid) {
			t.Errorf("%s = %d %s", bad, code, body)
		}
	}
	if len(fake.ends) != before {
		t.Fatal("a refused range reached the service")
	}
}

// The metric browser's series name their nodes and deployments, live or
// deleted: a deleted one by the name it left, marked so. A series whose node
// or deployment neither the store nor the deleted records know keeps its
// labels as they were.
func TestMonitorSeriesNamesLiveAndDeletedNodesAndDeployments(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	if _, err := st.RegisterNode("node-live", store.HashToken("a"), "host-live"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-gone", store.HashToken("b"), "host-gone"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteNode("node-gone"); err != nil {
		t.Fatal(err)
	}
	live, err := st.CreateDeployment(&store.Deployment{Name: "live-svc", ServedName: "org/live", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := st.CreateDeployment(&store.Deployment{Name: "gone-svc", ServedName: "org/gone", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteDeployment(gone.ID); err != nil {
		t.Fatal(err)
	}
	dep := func(id uint) string { return strconv.FormatUint(uint64(id), 10) }
	fake := &fakeMonitorService{series: []dto.MonitorSeries{
		{Labels: map[string]string{"node": "node-live"}, Points: []dto.Point{{1000, 1}}},
		{Labels: map[string]string{"node": "node-gone"}, Points: []dto.Point{{1000, 2}}},
		{Labels: map[string]string{"node": "node-unknown"}, Points: []dto.Point{{1000, 3}}},
		{Labels: map[string]string{"deployment": dep(live.ID)}, Points: []dto.Point{{1000, 4}}},
		{Labels: map[string]string{"deployment": dep(gone.ID)}, Points: []dto.Point{{1000, 5}}},
		{Labels: map[string]string{"deployment": "9999"}, Points: []dto.Point{{1000, 6}}},
	}}
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: fake})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/series?metric=gpuworks_gpu_utilization&by=node", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	want := `{"items":[` +
		`{"labels":{"node":"node-live","node_name":"host-live"},"points":[[1000,1]]},` +
		`{"labels":{"node":"node-gone","node_deleted":"true","node_name":"host-gone"},"points":[[1000,2]]},` +
		`{"labels":{"node":"node-unknown"},"points":[[1000,3]]},` +
		`{"labels":{"deployment":"` + dep(live.ID) + `","deployment_name":"live-svc"},"points":[[1000,4]]},` +
		`{"labels":{"deployment":"` + dep(gone.ID) + `","deployment_deleted":"true","deployment_name":"gone-svc"},"points":[[1000,5]]},` +
		`{"labels":{"deployment":"9999"},"points":[[1000,6]]}],"total":6}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || got != want {
		t.Fatalf("series = %d\n got %s\nwant %s", rec.Code, got, want)
	}
}

// A deleted deployment's monitor page answers 404 with the names it left, so
// a link to it reads as what it was; one never known stays a plain 404.
func TestMonitorDeploymentDeletedIs404WithItsNames(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	gone, err := st.CreateDeployment(&store.Deployment{Name: "gone-svc", ServedName: "org/gone", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteDeployment(gone.ID); err != nil {
		t.Fatal(err)
	}
	r := Router(Deps{Store: st, AuthSigner: signer, Monitor: &fakeMonitorService{deploymentErr: ErrMonitorDeploymentNotFound}})
	get := func(id uint) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/deployments/"+strconv.FormatUint(uint64(id), 10), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	rec := get(gone.ID)
	id := strconv.FormatUint(uint64(gone.ID), 10)
	want := `{"code":"MONITOR_DEPLOYMENT_DELETED","error":"deployment gone-svc was deleted","params":{"deployment_id":` + id + `,"name":"gone-svc","served_name":"org/gone"}}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusNotFound || got != want {
		t.Fatalf("deleted = %d\n got %s\nwant %s", rec.Code, got, want)
	}
	unknown := get(gone.ID + 100)
	if unknown.Code != http.StatusNotFound || strings.Contains(unknown.Body.String(), ErrorMonitorDeploymentDeleted) {
		t.Fatalf("unknown = %d %s", unknown.Code, unknown.Body.String())
	}
}
