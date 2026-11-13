package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func benchmarkCurveHTTPFixture(t *testing.T, log *slog.Logger, curves ...string) (http.Handler, string, string) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: t.Name(), State: "done"})
	if err != nil {
		t.Fatal(err)
	}
	for i, curve := range curves {
		if err := st.UpsertBenchmarkPoint(&store.BenchmarkPoint{RunID: run.ID, Seq: i + 1, State: "done", Concurrency: i + 1, Curve: curve}); err != nil {
			t.Fatal(err)
		}
	}
	signer, err := auth.NewTokenSigner("benchmark-curve-regression", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	return Router(Deps{Store: st, Bus: bus.New(), AuthSigner: signer, Log: log}), fmt.Sprintf("/api/v1/benchmarks/%d/curve", run.ID), token
}

func readBenchmarkCurveHTTP(t *testing.T, handler http.Handler, path, token string) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestBenchmarkCurvePreservesUnknownMetricsAndObservedZero(t *testing.T) {
	handler, path, token := benchmarkCurveHTTPFixture(t, nil, `[
  {"t":1000},
  {"t":2000,"output_tput":null,"total_tput":null,"request_tput":null,"ttft_p50":null,"tpot_p50":null,"latency_p99":null},
  {"t":3000,"output_tput":0,"total_tput":0,"request_tput":0,"ttft_p50":0,"tpot_p50":0,"latency_p99":0},
  {"t":4000,"output_tput":1.5,"total_tput":2.5,"request_tput":3.5,"ttft_p50":4.5,"tpot_p50":5.5,"latency_p99":6.5}
 ]`)
	wire := readBenchmarkCurveHTTP(t, handler, path, token)
	var points []struct {
		Samples []map[string]json.RawMessage `json:"samples"`
	}
	if err := json.Unmarshal(wire["points"], &points); err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || len(points[0].Samples) != 4 {
		t.Fatalf("points=%s", wire["points"])
	}
	fields := []string{"output_tput", "total_tput", "request_tput", "ttft_p50", "tpot_p50", "latency_p99"}
	for i, field := range fields {
		for sample := 0; sample < 2; sample++ {
			value := points[0].Samples[sample][field]
			if len(value) != 0 && string(value) != "null" {
				t.Errorf("sample %d unknown %s=%s, want null or omitted", sample, field, value)
			}
		}
		if got := string(points[0].Samples[2][field]); got != "0" {
			t.Errorf("observed zero %s=%s", field, got)
		}
		if got, want := string(points[0].Samples[3][field]), fmt.Sprintf("%d.5", i+1); got != want {
			t.Errorf("observed %s=%s, want %s", field, got, want)
		}
	}
}
