package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func TestBenchmarkCurveAPIAndHistoricalRunFallback(t *testing.T) {
	st, err := store.Open("file:benchmark_curve_api?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	withCurve, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "with curve", State: "done"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	if err := st.AppendBenchmarkCurveSample(withCurve.ID, 1, at, metrics.ProcessSample{OutputTput: 12.5, TTFTP50: 42}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBenchmarkPoint(&store.BenchmarkPoint{RunID: withCurve.ID, Seq: 1, State: "done", Concurrency: 8}); err != nil {
		t.Fatal(err)
	}
	historical, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "historical", State: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBenchmarkPoint(&store.BenchmarkPoint{RunID: historical.ID, Seq: 1, State: "done", Concurrency: 1}); err != nil {
		t.Fatal(err)
	}

	signer, _ := auth.NewTokenSigner("benchmark-curve-api", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	handler := Router(Deps{Store: st, Bus: bus.New(), AuthSigner: signer})
	get := func(id uint) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/benchmarks/"+strconv.FormatUint(uint64(id), 10)+"/curve", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := get(withCurve.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("curve status=%d body=%s", rec.Code, rec.Body.String())
	}
	var curve benchmarkCurveDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &curve); err != nil {
		t.Fatal(err)
	}
	if len(curve.Points) != 1 || curve.Points[0].Concurrency != 8 || len(curve.Points[0].Samples) != 1 || curve.Points[0].Samples[0].OutputTput == nil || *curve.Points[0].Samples[0].OutputTput != 12.5 {
		t.Fatalf("curve response = %+v", curve)
	}

	rec = get(historical.ID)
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"points\":[]}\n" {
		t.Fatalf("historical curve status=%d body=%s", rec.Code, rec.Body.String())
	}
	detailReq := httptest.NewRequest(http.MethodGet, "/api/v1/benchmarks/"+strconv.FormatUint(uint64(historical.ID), 10), nil)
	detailReq.Header.Set("Authorization", "Bearer "+token)
	detailRec := httptest.NewRecorder()
	handler.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("historical detail status=%d body=%s", detailRec.Code, detailRec.Body.String())
	}
	var detail benchmarkDTO
	if err := json.Unmarshal(detailRec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.TargetSnapshot) != 0 || len(detail.Points) != 1 || detail.Points[0].Seq != 1 {
		t.Fatalf("historical detail did not degrade gracefully: %+v", detail)
	}
}
