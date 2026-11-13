package api

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type timelineHTTPFixture struct {
	handler http.Handler
	store   *store.Store
	run     *store.BenchmarkRun
	token   string
	path    string
}

func newTimelineHTTPFixture(t *testing.T, keep bool, raw string) timelineHTTPFixture {
	t.Helper()
	b := bus.New()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "timeline.db"), b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "timeline", State: "done", Mode: "grid", Model: "fixture-model", KeepDetail: keep, DetailCapMB: 1})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "details.jsonl.gz")
	if raw != "" {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		if _, err = gz.Write([]byte(strings.ReplaceAll(raw, "RUN_ID", fmt.Sprint(run.ID)))); err != nil {
			t.Fatal(err)
		}
		if err = gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.UpsertBenchmarkPoint(&store.BenchmarkPoint{RunID: run.ID, Seq: 1, State: "done", DetailPath: path, Errors: 1}); err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("timeline-api-test", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	return timelineHTTPFixture{handler: Router(Deps{Store: st, Bus: b, AuthSigner: signer, BenchmarkArtifactDir: root}), store: st, run: run, token: token, path: fmt.Sprintf("/api/v1/benchmarks/%d/points/1/timeline", run.ID)}
}

func (f timelineHTTPFixture) request(method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

const timelineWireRecord = `{"version":1,"run_id":RUN_ID,"point_seq":1,"session_id":"session-a","turn":1,"planned_turns":2,"attempt":1,"metrics":{"started_at":"2026-11-09T10:00:00Z","first_output_at":null,"first_content_at":null,"last_output_at":null,"finished_at":"2026-11-09T10:00:00.42Z","ttft_ms":null,"tpot_ms":null,"latency_ms":null,"completion_ms":420,"input_tokens":0,"output_tokens":0},"error_text":"fixture failure","error_stage":"http","http_status":503,"input_source":"","output_source":"","output_budget":220,"text":"BODY_MUST_NOT_APPEAR","usage":{"private":"USAGE_MUST_NOT_APPEAR"},"local_input":null,"local_output":null,"stream":true}`

func TestBenchmarkRequestTimelineWireAndPrivacy(t *testing.T) {
	f := newTimelineHTTPFixture(t, true, timelineWireRecord+"\n")
	rec := f.request(http.MethodGet, f.path, f.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("timeline response must not be cached")
	}
	expected := fmt.Sprintf(`{"status":"complete","records":[{"version":1,"run_id":%d,"point_seq":1,"session_id":"session-a","turn":1,"planned_turns":2,"attempt":1,"error_text":"fixture failure","error_stage":"http","http_status":503,"input_source":"","output_source":"","output_budget":220,"metrics":{"started_at":"2026-11-09T10:00:00Z","first_output_at":null,"first_content_at":null,"last_output_at":null,"finished_at":"2026-11-09T10:00:00.42Z","ttft_ms":null,"tpot_ms":null,"latency_ms":null,"completion_ms":420,"input_tokens":0,"output_tokens":0}}]}`, f.run.ID)
	var got, want any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire mismatch\ngot %s\nwant %s", rec.Body.String(), expected)
	}
	for _, secret := range []string{"BODY_MUST_NOT_APPEAR", "USAGE_MUST_NOT_APPEAR", "\"text\"", "\"usage\"", "\"chunk_intervals_ms\""} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("unexpected payload %q in %s", secret, rec.Body.String())
		}
	}
}

func TestBenchmarkRequestTimelineAuthenticationAndRouting(t *testing.T) {
	f := newTimelineHTTPFixture(t, true, timelineWireRecord+"\n")
	cases := []struct {
		method, path, token string
		status              int
	}{
		{http.MethodGet, f.path, "", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/benchmarks/bad/points/1/timeline", f.token, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/benchmarks/0/points/1/timeline", f.token, http.StatusBadRequest},
		{http.MethodGet, strings.Replace(f.path, "/points/1/", "/points/-1/", 1), f.token, http.StatusBadRequest},
		{http.MethodGet, strings.Replace(f.path, "/points/1/", "/points/bad/", 1), f.token, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/benchmarks/999999/points/1/timeline", f.token, http.StatusNotFound},
		{http.MethodGet, strings.Replace(f.path, "/points/1/", "/points/999/", 1), f.token, http.StatusNotFound},
		{http.MethodPost, f.path, f.token, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			rec := f.request(tc.method, tc.path, tc.token)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

func TestBenchmarkRequestTimelineRetentionStatus(t *testing.T) {
	for _, tc := range []struct {
		name        string
		keep        bool
		raw, status string
		records     int
	}{
		{"disabled", false, timelineWireRecord + "\n", "disabled", 0},
		{"missing file", true, "", "unavailable", 0},
		{"marker", true, timelineWireRecord + "\n{\"truncated\":true}\n", "truncated", 1},
		{"malformed", true, "invalid-json\n", "truncated", 0},
		{"legacy identity", true, "{\"version\":0}\n", "unavailable", 0},
		{"mismatched identity", true, strings.Replace(timelineWireRecord, "\"point_seq\":1", "\"point_seq\":2", 1) + "\n", "unavailable", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTimelineHTTPFixture(t, tc.keep, tc.raw)
			rec := f.request(http.MethodGet, f.path, f.token)
			var payload struct {
				Status  string            `json:"status"`
				Records []json.RawMessage `json:"records"`
			}
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Status != tc.status || payload.Records == nil || len(payload.Records) != tc.records {
				t.Fatalf("response=%s", rec.Body.String())
			}
		})
	}
}

func TestBenchmarkRequestTimelineRedactsHistoricalErrorsAndRejectsEscapedPaths(t *testing.T) {
	raw := strings.Replace(timelineWireRecord, "fixture failure", "upstream https://timeline-user:TIMELINE_PASSWORD@host.test/?access_token=TIMELINE_QUERY", 1)
	f := newTimelineHTTPFixture(t, true, raw+"\n")
	rec := f.request(http.MethodGet, f.path, f.token)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{"timeline-user", "TIMELINE_PASSWORD", "TIMELINE_QUERY"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("historical credential leaked: %s", rec.Body.String())
		}
	}
	point, err := f.store.GetBenchmarkPoint(f.run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "outside.gz")
	if err = os.WriteFile(external, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	point.DetailPath = external
	if err = f.store.UpsertBenchmarkPoint(point); err != nil {
		t.Fatal(err)
	}
	rec = f.request(http.MethodGet, f.path, f.token)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"unavailable"`) || !strings.Contains(rec.Body.String(), `"records":[]`) {
		t.Fatalf("escaped path response=%d %s", rec.Code, rec.Body.String())
	}
}
