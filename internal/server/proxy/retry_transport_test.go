package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/proxy/plane"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRetryTransportClientCancelDoesNotCoolHealthyInstance(t *testing.T) {
	b := newBalancer(nil)
	c := candidate{instanceUUID: "healthy", nodeUUID: "node", modelName: "model"}
	lease, _ := b.pick([]candidate{c}, nil, func(string) bool { return true }, admission{})
	if lease == nil {
		t.Fatal("initial route did not produce a lease")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://proxy/v1/chat", strings.NewReader(`{"model":"model"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{"model":"model"}`)), nil }
	attempt := &routeAttempt{
		candidates: []candidate{c}, current: lease,
		tried: map[string]bool{"healthy": true}, attempts: 1, modelName: "model",
	}
	transport := &retryTransport{
		base:     roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() }),
		balancer: b,
		hub:      readinessTestHub{online: map[string]bool{"node": true}},
	}
	req = req.WithContext(context.WithValue(ctx, attemptKey, attempt))
	if _, err := transport.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip error = %v, want context.Canceled", err)
	}
	state := b.states["healthy"]
	if state == nil {
		t.Fatal("healthy instance state was removed unexpectedly")
	}
	if state.failStreak != 0 || !state.cooldownUntil.IsZero() {
		t.Fatalf("client cancellation changed health state: failStreak=%d cooldown=%s", state.failStreak, state.cooldownUntil)
	}
}

func TestRetryTransportRetriesTunnelFailureUsingFailoverPolicy(t *testing.T) {
	b := newBalancer(nil)
	candidates := []candidate{
		{instanceUUID: "primary", nodeUUID: "node-primary", modelName: "model"},
		{instanceUUID: "backup", nodeUUID: "node-backup", modelName: "model"},
	}
	first, _ := b.pickFailover(candidates, nil, func(string) bool { return true }, admission{})
	if first == nil || first.c.instanceUUID != "primary" {
		t.Fatalf("initial target=%v, want primary", first)
	}
	attempt := &routeAttempt{
		candidates: candidates, current: first, tried: map[string]bool{"primary": true},
		attempts: 1, modelName: "model", policy: "failover", started: time.Now(),
	}
	calls := 0
	transport := &retryTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return nil, &tunnelOpenError{err: errors.New("dial failed")}
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}),
		balancer: b,
		hub:      readinessTestHub{online: map[string]bool{"node-primary": true, "node-backup": true}},
	}
	req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat", strings.NewReader(`{"model":"model"}`))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{"model":"model"}`)), nil }
	req = req.WithContext(context.WithValue(req.Context(), attemptKey, attempt))
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls != 2 || attempt.current == nil || attempt.current.c.instanceUUID != "backup" {
		t.Fatalf("calls=%d current=%v, want two calls and backup", calls, attempt.current)
	}
	attempt.current.release(outcomeOK)
}

// TestRetryTransportRetriesDataLaneUnavailableUsingFailoverPolicy is the
// multi-replica counterpart of TestProxyDataLaneUnavailableReturns503WithRetryAfter
// (accounting_e2e_test.go): when the tunnel open fails specifically because
// the primary's data lane is detached (hub.ErrDataLaneUnavailable, wrapped in
// tunnelOpenError exactly as proxy.go's DialContext wraps every OpenTunnel
// error), the retry transport must still treat it as an ordinary
// connection-level failure and switch to the backup instance — the sentinel
// type is invisible to canRetry, which only ever asks "is this a
// tunnelOpenError", so no special case was needed in retry_transport.go
// itself; this test is what proves that.
func TestRetryTransportRetriesDataLaneUnavailableUsingFailoverPolicy(t *testing.T) {
	b := newBalancer(nil)
	candidates := []candidate{
		{instanceUUID: "primary", nodeUUID: "node-primary", modelName: "model"},
		{instanceUUID: "backup", nodeUUID: "node-backup", modelName: "model"},
	}
	first, _ := b.pickFailover(candidates, nil, func(string) bool { return true }, admission{})
	if first == nil || first.c.instanceUUID != "primary" {
		t.Fatalf("initial target=%v, want primary", first)
	}
	attempt := &routeAttempt{
		candidates: candidates, current: first, tried: map[string]bool{"primary": true},
		attempts: 1, modelName: "model", policy: "failover", started: time.Now(),
	}
	calls := 0
	transport := &retryTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return nil, &tunnelOpenError{err: fmt.Errorf("%w: node node-primary did not reattach within 5s", plane.ErrDataLaneUnavailable)}
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}),
		balancer: b,
		hub:      readinessTestHub{online: map[string]bool{"node-primary": true, "node-backup": true}},
	}
	req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat", strings.NewReader(`{"model":"model"}`))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{"model":"model"}`)), nil }
	req = req.WithContext(context.WithValue(req.Context(), attemptKey, attempt))
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls != 2 || attempt.current == nil || attempt.current.c.instanceUUID != "backup" {
		t.Fatalf("calls=%d current=%v, want two calls and backup (data-lane-unavailable on the primary must not exhaust the retry budget)", calls, attempt.current)
	}
	attempt.current.release(outcomeOK)
}

// Both retryable failures must replay bytes from offset zero, even after the
// failed transport consumed the entire disk-backed upload.
func TestRetryTransportReplaysConsumedBodyAndSkipsDifferentModel(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"tunnel", &tunnelOpenError{err: errors.New("offline")}},
		{"no-response", io.ErrUnexpectedEOF},
	} {
		t.Run(failure.name, func(t *testing.T) {
			payload := strings.Repeat("upload", bodyMemoryThreshold)
			body, err := newReplayBodyFromReader(strings.NewReader(payload), DefaultMaxBodyBytes)
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			if body.file == nil {
				t.Fatal("expected disk-backed replay")
			}
			b := newBalancer(nil)
			candidates := []candidate{
				{instanceUUID: "a", nodeUUID: "node", modelName: "model"},
				{instanceUUID: "b", nodeUUID: "node", modelName: "other"},
				{instanceUUID: "c", nodeUUID: "node", modelName: "model"},
			}
			a := &routeAttempt{candidates: candidates, current: leaseOf(b.pickFailover(candidates, nil, nil, admission{})), tried: map[string]bool{"a": true}, attempts: 1, policy: "failover", modelName: "model"}
			calls := 0
			transport := &retryTransport{balancer: b, hub: readinessTestHub{online: map[string]bool{"node": true}}, base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				got, readErr := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if readErr != nil || string(got) != payload {
					t.Fatalf("attempt %d: incomplete body, err=%v", calls, readErr)
				}
				if calls == 1 {
					return nil, failure.err
				}
				if a.current.c.instanceUUID != "c" {
					t.Fatalf("retried target=%s", a.current.c.instanceUUID)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", body.NewReader())
			req.GetBody = func() (io.ReadCloser, error) { return body.NewReader(), nil }
			req = req.WithContext(context.WithValue(req.Context(), attemptKey, a))
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			a.current.release(outcomeOK)
			if calls != 2 {
				t.Fatalf("attempts=%d", calls)
			}
			for id, state := range b.states {
				if state.inflight.total != 0 {
					t.Fatalf("instance %s leaked %d leases", id, state.inflight.total)
				}
			}
			if b.states["b"].failStreak != 0 {
				t.Fatal("model mismatch penalized healthy candidate")
			}
		})
	}
}

// A retry passes over a candidate ordered to sleep since the request was
// routed, as the first pick does; with none awake there is no retry target.
func TestRetryPickPassesOverAnInstanceOrderedToSleep(t *testing.T) {
	b := newBalancer(nil)
	candidates := []candidate{
		{instanceUUID: "tried", nodeUUID: "n", modelName: "model"},
		{instanceUUID: "asleep", nodeUUID: "n", modelName: "model"},
		{instanceUUID: "awake", nodeUUID: "n", modelName: "model"},
	}
	ordered := map[string]bool{"asleep": true}
	transport := &retryTransport{balancer: b, hub: readinessTestHub{online: map[string]bool{"n": true}}, awake: func(l *lease) bool { return !ordered[l.c.instanceUUID] }}
	attempt := &routeAttempt{candidates: candidates, tried: map[string]bool{"tried": true}, modelName: "model", policy: "failover"}
	for i := 0; i < 8; i++ {
		next := transport.pickNext(attempt, false)
		if next == nil || next.c.instanceUUID != "awake" {
			t.Fatalf("retry pick %d = %+v, want awake", i, next)
		}
		next.release(outcomeCanceled)
	}
	if len(attempt.tried) != 1 {
		t.Fatalf("the pick changed the attempt's tried set: %v", attempt.tried)
	}
	ordered["awake"] = true
	if next := transport.pickNext(attempt, false); next != nil {
		t.Fatalf("with every untried candidate asleep the retry = %+v", next.c)
	}
}
