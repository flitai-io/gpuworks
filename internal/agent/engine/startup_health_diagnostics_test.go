package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// go test ./internal/agent/engine -run '^TestStartupHealth(Diagnostic|Waiting)' -count=1
func TestStartupHealthDiagnosticKeepsLastHTTPObservation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusServiceUnavailable
		if calls.Add(1) > 1 {
			code = http.StatusBadGateway
		}
		w.WriteHeader(code)
		fmt.Fprint(w, "secret-engine-response")
	}))
	defer server.Close()
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("health-evidence-http", serverPort(t, server))
	spec.StartTimeout, spec.HealthPath = 3, "/health?api_key=secret-health-query"
	name := containerName(spec.UUID)
	if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if e.probeAndReport(t.Context(), spec, name) {
		t.Fatal("HTTP failure declared the engine ready")
	}
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || calls.Load() != 2 {
		t.Fatalf("calls=%d reports=%+v", calls.Load(), log.snapshot())
	}
	message := failed[0].Message
	if f := failed[0].RuntimeFailure; f == nil || f.Code != proto.ErrorEngineHealthTimeout || f.Phase != proto.PhaseHealthCheck ||
		!strings.Contains(f.Detail, "HTTP 502") || strings.Contains(f.Detail, "observed_at") || strings.Contains(f.Detail, "secret-") {
		t.Fatalf("runtime failure = %+v", f)
	}
	for _, want := range []string{"health check timed out", "from=node", fmt.Sprintf("http://127.0.0.1:%d/health", spec.APIPort), "HTTP 502", "observed_at="} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q lacks %q", message, want)
		}
	}
	if strings.Contains(message, "secret-") || strings.Contains(message, "HTTP 503") {
		t.Fatalf("body/query secret or old result leaked: %s", message)
	}
	_, stamp, ok := strings.Cut(message, "observed_at=")
	if !ok {
		t.Fatal("missing sampled time")
	}
	stamp = strings.Fields(stamp)[0]
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || at.Before(started) || at.After(time.Now()) {
		t.Fatalf("last observation time %q: %v", stamp, err)
	}
}

func TestStartupHealthDiagnosticIdentifiesRefusedLocalConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("health-evidence-refused", port)
	spec.StartTimeout = 1
	name := containerName(spec.UUID)
	if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
		t.Fatal(err)
	}
	if e.probeAndReport(t.Context(), spec, name) {
		t.Fatal("closed port declared the engine ready")
	}
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || !strings.Contains(failed[0].Message, "connection_refused") || !strings.Contains(failed[0].Message, "from=node") {
		t.Fatalf("lost local transport failure: %+v", log.snapshot())
	}
}

func TestStartupHealthDiagnosticKeepsRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("health-evidence-timeout", serverPort(t, server))
	spec.StartTimeout = 1
	name := containerName(spec.UUID)
	if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
		t.Fatal(err)
	}
	if e.probeAndReport(t.Context(), spec, name) {
		t.Fatal("non-answering endpoint declared the engine ready")
	}
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || !strings.Contains(failed[0].Message, "result=timeout") || !strings.Contains(failed[0].Message, "observed_at=") {
		t.Fatalf("request timeout lost: %+v", log.snapshot())
	}
}

func TestStartupHealthWaitingUsesStableLastResultAndCancellationDoesNotFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("health-evidence-waiting", serverPort(t, server))
	spec.StartTimeout = 60
	name := containerName(spec.UUID)
	if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- e.probeAndReport(ctx, spec, name) }()
	deadline := time.NewTimer(26 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waits []string
		for _, s := range log.snapshot() {
			if s.Phase == proto.PhaseHealthCheck {
				waits = append(waits, s.Message)
			}
		}
		if len(waits) >= 2 {
			cancel()
			if <-done {
				t.Fatal("cancelled startup declared ready")
			}
			if waits[0] != waits[1] || !strings.Contains(waits[0], "HTTP 503") || !strings.Contains(waits[0], "from=node") || strings.Contains(waits[0], "observed_at=") {
				t.Fatalf("waiting messages lose context or change for clock ticks: %q", waits)
			}
			if len(log.states(proto.InstFailed)) != 0 {
				t.Fatalf("cancellation reported engine failure: %+v", log.snapshot())
			}
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("did not receive two bounded-cadence health reports")
		case <-ticker.C:
		}
	}
}
