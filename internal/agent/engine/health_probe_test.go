package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeEngineHealthRetriesHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statuses  []int
		window    time.Duration
		wantCalls int
		wantError bool
	}{
		{"healthy", []int{200}, time.Minute, 1, false},
		{"recovers", []int{503, 500, 200}, time.Minute, 3, false},
		// Failures shorter than the window never decide anything, however many.
		{"recovers after many failures", append(repeatStatus(503, 20), 200), time.Minute, 21, false},
		{"no window", []int{503}, 0, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/custom-health" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				index := min(int(calls.Add(1))-1, len(tc.statuses)-1)
				w.WriteHeader(tc.statuses[index])
				_, _ = w.Write([]byte("ok"))
			}))
			defer server.Close()
			spec := runningSpec("probe", serverPort(t, server))
			spec.HealthPath = "/custom-health"
			resp, err := probeEngineHealth(context.Background(), spec, server.Client(), tc.window, time.Millisecond)
			if (err != nil) != tc.wantError || int(calls.Load()) != tc.wantCalls {
				t.Fatalf("calls=%d error=%v", calls.Load(), err)
			}
			if resp != nil {
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil || string(body) != "ok" {
					t.Fatalf("successful response was consumed: %q %v", body, err)
				}
			}
		})
	}
}

func repeatStatus(status, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = status
	}
	return out
}

// The engine is given up only once it has failed for the whole window: the
// probe keeps retrying until then and names the duration and the last error.
func TestProbeEngineHealthFailsOnlyAfterTheWindow(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	const window = 150 * time.Millisecond
	started := time.Now()
	_, err := probeEngineHealth(context.Background(), runningSpec("window", serverPort(t, server)), server.Client(), window, 10*time.Millisecond)
	if elapsed := time.Since(started); elapsed < window {
		t.Fatalf("gave up after %s, before the %s window", elapsed, window)
	}
	if err == nil || calls.Load() < 3 || !strings.Contains(err.Error(), "failed continuously") || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}

func TestProbeEngineHealthRetriesRequestTimeouts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	spec := runningSpec("timeout", serverPort(t, server))
	_, err := probeEngineHealth(context.Background(), spec, &http.Client{Timeout: 50 * time.Millisecond}, 120*time.Millisecond, time.Millisecond)
	if err == nil || calls.Load() < 2 || !strings.Contains(err.Error(), "failed continuously") {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}

type healthProbeTransport func(*http.Request) (*http.Response, error)

func (f healthProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type healthProbeBody struct {
	io.Reader
	closed func()
}

func (b healthProbeBody) Close() error { b.closed(); return nil }

func TestProbeEngineHealthClosesEachFailedResponse(t *testing.T) {
	var closed int
	client := &http.Client{Transport: healthProbeTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: healthProbeBody{Reader: strings.NewReader("unavailable"), closed: func() { closed++ }}}, nil
	})}
	var calls int
	counting := &http.Client{Transport: healthProbeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return client.Transport.RoundTrip(req)
	})}
	_, err := probeEngineHealth(context.Background(), runningSpec("close", 1), counting, 20*time.Millisecond, time.Millisecond)
	if err == nil || calls < 2 || closed != calls {
		t.Fatalf("calls=%d closed=%d error=%v", calls, closed, err)
	}
}

func TestProbeEngineHealthCancellationIsNotAnEngineFailure(t *testing.T) {
	for _, duringRequest := range []bool{true, false} {
		name := "retry wait"
		if duringRequest {
			name = "request"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			var calls atomic.Int32
			client := &http.Client{Transport: healthProbeTransport(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if duringRequest {
					close(entered)
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return &http.Response{StatusCode: 503, Header: make(http.Header), Body: healthProbeBody{Reader: strings.NewReader("unavailable"), closed: func() { close(entered) }}}, nil
			})}
			done := make(chan error, 1)
			go func() {
				_, err := probeEngineHealth(ctx, runningSpec("cancel", 1), client, time.Hour, time.Minute)
				done <- err
			}()
			<-entered
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
					t.Fatalf("calls=%d error=%v", calls.Load(), err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt the probe")
			}
		})
	}
}
