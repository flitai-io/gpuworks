package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestBenchmarkCorruptMetadataReturnsJSONError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(*store.BenchmarkRun)
	}{
		{"dataset", func(r *store.BenchmarkRun) { r.Dataset = "{" }},
		{"search", func(r *store.BenchmarkRun) { r.Search = "{" }},
		{"target_snapshot", func(r *store.BenchmarkRun) { r.TargetSnapshot = "{" }},
		{"progress", func(r *store.BenchmarkRun) { r.Progress = "{" }},
		{"verdict", func(r *store.BenchmarkRun) { r.Verdict = "{" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := bus.New()
			st, err := store.Open("file:"+filepath.Join(t.TempDir(), "benchmark.db")+"?mode=memory&cache=shared", b)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			healthy, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "healthy", State: "done", Dataset: `{"kind":"random"}`})
			if err != nil {
				t.Fatal(err)
			}
			broken := &store.BenchmarkRun{Name: "broken", State: "done", Dataset: `{}`, Search: `{}`, TargetSnapshot: `{}`, Progress: `{}`, Verdict: `{}`}
			tc.corrupt(broken)
			broken, err = st.CreateBenchmarkRun(broken)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := auth.NewTokenSigner("benchmark-metadata-test", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
			handler := Router(Deps{Store: st, Bus: b, AuthSigner: signer})
			get := func(path string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				return rec
			}
			for _, path := range []string{fmt.Sprintf("/api/v1/benchmarks/%d", broken.ID), "/api/v1/benchmarks"} {
				rec := get(path)
				if rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("%s status=%d content-type=%q body=%s", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
				}
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("%s invalid JSON error: %v; body=%s", path, err, rec.Body.String())
				}
				if body["code"] != "RESPONSE_ENCODING_FAILED" || body["error"] != "response data could not be encoded as JSON" {
					t.Fatalf("%s error=%v", path, body)
				}
			}
			rec := get(fmt.Sprintf("/api/v1/benchmarks/%d", healthy.ID))
			var body map[string]json.RawMessage
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || string(body["name"]) != `"healthy"` || string(body["dataset"]) != `{"kind":"random"}` {
				t.Fatalf("healthy record status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
