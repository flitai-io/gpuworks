package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
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

// A manual command that states no API port starts an engine on its own
// default port: the start that then never answers on the assigned port says
// so, in its failure code and its message. Without the server's word that
// the command states no port, a refused port stays a plain timeout.
//
// go test ./internal/agent/engine -run '^TestStartupHealth(NamesAnUnstatedPort|KeepsTheTimeoutWhenThePortAnswers)|^TestAStalledStartOnAnUnstatedPort|^TestHealthWaitMessage|^TestUnstatedPortFailure' -count=1
func TestStartupHealthNamesAnUnstatedPortWhenNothingListens(t *testing.T) {
	for _, unstated := range []bool{true, false} {
		t.Run(fmt.Sprintf("unstated=%t", unstated), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			driver := newLivenessDriver()
			e, log := newLivenessEngine(t, driver)
			spec := runningSpec("health-unstated-port", port)
			spec.StartTimeout, spec.APIPortUnstated = 1, unstated
			name := containerName(spec.UUID)
			if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
				t.Fatal(err)
			}
			if e.probeAndReport(t.Context(), spec, name) {
				t.Fatal("closed port declared the engine ready")
			}
			failed := log.states(proto.InstFailed)
			if len(failed) != 1 || failed[0].RuntimeFailure == nil {
				t.Fatalf("reports = %+v", log.snapshot())
			}
			f, message := failed[0].RuntimeFailure, failed[0].Message
			note := fmt.Sprintf("the manual command states no API port, so the engine is not listening on the assigned port %d", port)
			if !unstated {
				if f.Code != proto.ErrorEngineHealthTimeout || f.Port != 0 || strings.Contains(message, note) {
					t.Fatalf("failure = %+v, message %q", f, message)
				}
				return
			}
			if f.Code != proto.ErrorEnginePortUnstated || f.Port != port || f.Phase != proto.PhaseHealthCheck || !strings.Contains(f.Detail, "result=connection_refused") {
				t.Fatalf("failure = %+v", f)
			}
			if !strings.HasPrefix(message, "health check timed out: ") || !strings.Contains(message, note) {
				t.Fatalf("message = %q", message)
			}
		})
	}
}

// Something answers on the assigned port (the engine's default port may be
// the one assigned): the command's missing port is not why the start failed.
func TestStartupHealthKeepsTheTimeoutWhenThePortAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("health-unstated-port-answers", serverPort(t, server))
	spec.StartTimeout, spec.APIPortUnstated = 1, true
	name := containerName(spec.UUID)
	if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
		t.Fatal(err)
	}
	if e.probeAndReport(t.Context(), spec, name) {
		t.Fatal("HTTP 503 declared the engine ready")
	}
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || failed[0].RuntimeFailure == nil || failed[0].RuntimeFailure.Code != proto.ErrorEngineHealthTimeout ||
		strings.Contains(failed[0].Message, "states no API port") {
		t.Fatalf("reports = %+v", log.snapshot())
	}
}

// An engine idle on its own default port looks stalled to the start watch;
// the stall it is failed for names the missing port and keeps what the watch
// saw and what the engine printed.
func TestAStalledStartOnAnUnstatedPortNamesThePort(t *testing.T) {
	driver := &stallDriver{livenessDriver: newLivenessDriver(), root: 100}
	driver.logs = hungStartOutput
	e, log, spec, name := stalledEngine(t, driver, stallProber{metrics: idleCards(0, 1, 2, 3)})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	spec.APIPort = listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	spec.APIPortUnstated = true
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if e.probeAndReportStartup(ctx, spec, name, true, true) {
		t.Fatal("a stalled engine was declared ready")
	}
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || failed[0].RuntimeFailure == nil {
		t.Fatalf("reports = %+v", log.snapshot())
	}
	f := failed[0].RuntimeFailure
	if f.Code != proto.ErrorEnginePortUnstated || f.Port != spec.APIPort || !strings.Contains(f.Detail, "no progress: output flat ") || !slices.Equal(f.Evidence, hungStartEvidence) {
		t.Fatalf("failure = %+v", f)
	}
	if message := failed[0].Message; !strings.HasPrefix(message, "engine start stalled: ") || !strings.Contains(message, "states no API port") {
		t.Fatalf("message = %q", message)
	}
}

// The progress names the missing port only while nothing listens on the API
// port of a launch the server marked, and stays the same text every time.
func TestHealthWaitMessage(t *testing.T) {
	marked := proto.InstanceDesiredState{APIPort: 30001}
	marked.APIPortUnstated = true
	target := "http://127.0.0.1:30001/health"
	note := "the manual command states no API port, so the engine is not listening on the assigned port 30001"
	for _, tc := range []struct {
		name   string
		spec   proto.InstanceDesiredState
		result string
		named  bool
	}{
		{"refused on a marked launch", marked, healthResultRefused, true},
		{"an answer on a marked launch", marked, "HTTP 503", false},
		{"timeout on a marked launch", marked, "timeout", false},
		{"refused on an unmarked launch", proto.InstanceDesiredState{APIPort: 30001}, healthResultRefused, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := healthWaitMessage(tc.spec, target, tc.result)
			if !strings.HasPrefix(message, "waiting for engine health: from=node requested_target="+target+" result="+tc.result) {
				t.Fatalf("message = %q", message)
			}
			if strings.HasSuffix(message, "; "+note) != tc.named || message != healthWaitMessage(tc.spec, target, tc.result) {
				t.Fatalf("message = %q, want the port named %t", message, tc.named)
			}
		})
	}
}

// Only a start that ended for not answering is put down to the missing port;
// an engine that exited or said it gave up failed for its own reason.
func TestUnstatedPortFailure(t *testing.T) {
	spec := proto.InstanceDesiredState{APIPort: 30001}
	spec.APIPortUnstated = true
	for _, tc := range []struct {
		code     string
		replaced bool
	}{
		{proto.ErrorEngineHealthTimeout, true},
		{proto.ErrorEngineStartStalled, true},
		{proto.ErrorEngineStartAborted, false},
		{proto.ErrorContainerExited, false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			in := proto.RuntimeFailure{Code: tc.code, Phase: proto.PhaseHealthCheck, Detail: "d", Evidence: []string{"e"}}
			got, message := unstatedPortFailure(spec, healthResultRefused, in, "m")
			want, wantMessage := in, "m"
			if tc.replaced {
				want.Code, want.Port = proto.ErrorEnginePortUnstated, 30001
				wantMessage = "m; the manual command states no API port, so the engine is not listening on the assigned port 30001"
			}
			if !reflect.DeepEqual(got, want) || message != wantMessage {
				t.Fatalf("failure = %+v, message %q", got, message)
			}
		})
	}
}
