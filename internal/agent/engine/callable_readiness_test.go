package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestCallableStartupFailureBoundsUntrustedError(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header)}
	resp.Header.Set("Server", "GPUWorksCallable/1 Python/3.10.12")
	for _, tc := range []struct{ body, want string }{
		{`{"status":"failed","error":null}`, "callable initialization failed"},
		{`{"status":"failed","error":{"message":"unrecognized"}}`, "callable initialization failed"},
		{`{"status":"failed","error":"line one\n\tline two"}`, "callable initialization failed: line one line two"},
		{`{"status":"failed","error":"` + strings.Repeat("错", 1100) + `"}`, "callable initialization failed: " + strings.Repeat("错", 1024) + "..."},
	} {
		var state engineReadiness
		if err := json.Unmarshal([]byte(tc.body), &state); err != nil {
			t.Fatal(err)
		}
		if got := callableStartupFailure(resp, state); got != tc.want {
			t.Fatalf("message=%q want=%q", got, tc.want)
		}
	}
}

// The catalog wrapper keeps serving HTTP after a loader exception. Its real
// /health contract is HTTP 503 with status=failed and the exception string.
func TestCallableStartupFailureReportsCauseAndReclaimsContainer(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanup_failure_%v", cleanupFails), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Server", "GPUWorksCallable/1 Python/3.10.12")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"status":"failed","error":"ModuleNotFoundError: No module named 'diffusers'"}`)
			}))
			defer server.Close()
			driver := newLivenessDriver()
			e, log := newLivenessEngine(t, driver)
			spec := runningSpec("callable-failed", serverPort(t, server))
			spec.Engine = engines.Callable
			id, err := driver.Run(context.Background(), runtime.WorkloadSpec{Name: containerName(spec.UUID)})
			if err != nil {
				t.Fatal(err)
			}
			if cleanupFails {
				driver.removeErrs = 1
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if e.probeAndReport(ctx, spec, id) {
				t.Fatal("failed loader reported ready")
			}
			states := log.snapshot()
			if len(states) != 1 {
				t.Fatalf("want one terminal report before startup timeout; got %+v", states)
			}
			wantState := proto.InstFailed
			if cleanupFails {
				wantState = proto.InstRemoving
			}
			if states[0].State != wantState || !strings.Contains(states[0].Message, "ModuleNotFoundError: No module named 'diffusers'") {
				t.Fatalf("loader failure was lost: %+v", states[0])
			}
			if driver.exists(id) != cleanupFails {
				t.Fatalf("container existence=%v; cleanup failure=%v", driver.exists(id), cleanupFails)
			}
		})
	}
}

func TestCallableStartupDoesNotFailOnUnprovenLoaderError(t *testing.T) {
	for _, tc := range []struct {
		name, server, body, engine string
	}{
		{"loading", "GPUWorksCallable/1 Python/3.10.12", `{"status":"starting","error":null}`, engines.Callable},
		{"unknown_service", "other", `{"status":"failed","error":"temporary"}`, engines.Callable},
		{"other_engine", "GPUWorksCallable/1 Python/3.10.12", `{"status":"failed","error":"wrong port"}`, engines.VLLM},
		{"partial_json", "GPUWorksCallable/1 Python/3.10.12", `{"status":"failed","error":"truncated`, engines.Callable},
		{"oversized_json", "GPUWorksCallable/1 Python/3.10.12", `{"status":"failed","error":"` + strings.Repeat("x", 64<<10) + `"}`, engines.Callable},
		{"non_json", "GPUWorksCallable/1 Python/3.10.12", "Service Unavailable", engines.Callable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Server", tc.server)
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			driver := newLivenessDriver()
			e, log := newLivenessEngine(t, driver)
			spec := runningSpec("callable-loading", serverPort(t, server))
			spec.Engine = tc.engine
			id, err := driver.Run(context.Background(), runtime.WorkloadSpec{Name: containerName(spec.UUID)})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if e.probeAndReport(ctx, spec, id) || len(log.snapshot()) != 0 || !driver.exists(id) {
				t.Fatalf("an inconclusive probe changed runtime state: %+v", log.snapshot())
			}
		})
	}
}
