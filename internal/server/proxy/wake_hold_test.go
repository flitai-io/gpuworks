package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// newSleepingProxy is a gateway in front of one sleepable deployment whose
// only instance is asleep, with the upstream answering "ok".
func newSleepingProxy(t *testing.T, held bool) (*Proxy, *store.Store, string, uint, *httptest.Server) {
	t.Helper()
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"ok":true}`) })
	p, _, key, depID, closeUpstream := newAccountingProxyFor(t, upstream, &store.Deployment{Engine: "vllm", Sleepable: true})
	t.Cleanup(closeUpstream)
	st := p.store
	if err := updateInstanceRow(st, "omni-instance", map[string]any{"sleep_level": 1}); err != nil {
		t.Fatal(err)
	}
	if held {
		if err := updateInstanceRow(st, "omni-instance", map[string]any{"sleep_held": true}); err != nil {
			t.Fatal(err)
		}
	}
	front := httptest.NewServer(p.Handler())
	t.Cleanup(front.Close)
	return p, st, key, depID, front
}

func postChat(t *testing.T, front *httptest.Server, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestARequestForAPlatformSleepWaitsForTheWake(t *testing.T) {
	p, st, key, depID, front := newSleepingProxy(t, false)
	var asked atomic.Int32
	p.SetWakeDemand(func(id uint) {
		if id != depID {
			t.Errorf("demand for %d, want %d", id, depID)
		}
		if asked.Add(1) != 1 {
			return
		}
		go func() {
			// The wake lands a little later, as the engine reports it.
			time.Sleep(200 * time.Millisecond)
			if n, _ := p.HeldRequests(depID); n != 1 {
				t.Errorf("held while waking = %d, want 1", n)
			}
			if err := updateInstanceRow(st, "omni-instance", map[string]any{"sleep_level": 0}); err != nil {
				t.Error(err)
			}
		}()
	})
	start := time.Now()
	resp := postChat(t, front, key)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}
	if waited := time.Since(start); waited < 200*time.Millisecond {
		t.Fatalf("answered after %v, before the wake", waited)
	}
	if n, _ := p.HeldRequests(depID); n != 0 {
		t.Fatalf("still held after the answer: %d", n)
	}
	if len(p.HeldDeployments()) != 0 {
		t.Fatalf("held deployments = %v", p.HeldDeployments())
	}
}

func TestAHeldSleepIsRefusedAtOnce(t *testing.T) {
	p, _, key, _, front := newSleepingProxy(t, true)
	p.SetWakeDemand(func(uint) { t.Error("an operator's sleep was woken by a request") })
	resp := postChat(t, front, key)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "asleep") {
		t.Fatalf("body = %s", body)
	}
}

func TestWithoutAWakerASleepingDeploymentIsRefusedAsBefore(t *testing.T) {
	_, _, key, _, front := newSleepingProxy(t, false)
	if resp := postChat(t, front, key); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestAHeldRequestLeavesWhenItsClientGoes(t *testing.T) {
	p, _, key, depID, front := newSleepingProxy(t, false)
	p.SetWakeDemand(func(uint) {})
	client := &http.Client{Timeout: 300 * time.Millisecond}
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	if _, err := client.Do(req); err == nil {
		t.Fatal("the request was answered while the model slept")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n, _ := p.HeldRequests(depID); n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a request whose client went is still held")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWakeHoldBoundsAndTimeout(t *testing.T) {
	h := newWakeHold()
	var reqs []*heldRequest
	for i := 0; i < wakeHoldPerDeployment; i++ {
		req, _, ok := h.enter(7)
		if !ok {
			t.Fatalf("refused at %d", i)
		}
		reqs = append(reqs, req)
	}
	if _, _, ok := h.enter(7); ok {
		t.Fatal("a deployment held more than its bound")
	}
	if _, _, ok := h.enter(8); !ok {
		t.Fatal("another deployment was refused")
	}
	if n, oldest := h.Held(7); n != wakeHoldPerDeployment || oldest.IsZero() {
		t.Fatalf("held = %d since %v", n, oldest)
	}
	for _, req := range reqs {
		h.leave(7, req)
		h.leave(7, req) // leaving twice is harmless
	}
	if n, _ := h.Held(7); n != 0 || h.total != 1 {
		t.Fatalf("after leaving: held=%d total=%d", n, h.total)
	}

}

func TestAWakeSignalReachesEveryHeldRequest(t *testing.T) {
	h := newWakeHold()
	_, first, _ := h.enter(1)
	_, second, _ := h.enter(2)
	h.signal()
	for _, ch := range []<-chan struct{}{first, second} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("a held request missed the signal")
		}
	}
	select {
	case <-h.next():
		t.Fatal("the next signal fired early")
	default:
	}
}

func TestAHeldRequestTimesOutWithRetryAfter(t *testing.T) {
	p, _, key, depID, front := newSleepingProxy(t, false)
	p.hold.holdMax = func(*store.Deployment) time.Duration { return 150 * time.Millisecond }
	p.SetWakeDemand(func(uint) {})
	start := time.Now()
	resp := postChat(t, front, key)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("status = %d Retry-After = %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Fatalf("gave up after %v, before its hold", waited)
	}
	if n, _ := p.HeldRequests(depID); n != 0 {
		t.Fatalf("still held: %d", n)
	}
}

// The gateway holds a request for as long as the deployment's own policy says
// (hold_max_sec), not for a time the level it once had stood for.
func TestAHeldRequestWaitsForTheDeploymentsHoldMaxSec(t *testing.T) {
	p, st, key, depID, front := newSleepingProxy(t, false)
	p.SetWakeDemand(func(uint) {})
	if err := st.SetDeploymentYieldPolicy(depID, yieldpolicy.Policy{Yield: yieldpolicy.Anytime, SleepLevel: yieldpolicy.LevelAuto, Unfinished: yieldpolicy.UnfinishedAbort, HoldMaxSec: 1}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp := postChat(t, front, key)
	waited := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("status = %d Retry-After = %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if waited < time.Second || waited > 20*time.Second {
		t.Fatalf("held for %v, want the 1 s hold_max_sec", waited)
	}
	dep, _ := st.GetDeployment(depID)
	if got := p.hold.holdMax(dep); got != time.Second {
		t.Fatalf("holdMax = %v", got)
	}
	// A deployment with no hold stated is held for the default.
	if got := p.hold.holdMax(&store.Deployment{}); got != yieldpolicy.DefaultHoldMax {
		t.Fatalf("default holdMax = %v", got)
	}
}

func TestASleepHeldWhileWaitingEndsTheHold(t *testing.T) {
	p, st, key, _, front := newSleepingProxy(t, false)
	p.SetWakeDemand(func(uint) {
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = updateInstanceRow(st, "omni-instance", map[string]any{"sleep_held": true})
		}()
	})
	start := time.Now()
	if resp := postChat(t, front, key); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if waited := time.Since(start); waited > 30*time.Second {
		t.Fatalf("held for %v after an operator held the sleep", waited)
	}
}

func TestADrainingDeploymentHoldsNewRequestsUntilItResumes(t *testing.T) {
	p, st, key, depID, front := newSleepingProxy(t, false)
	// Awake, but draining: a new request waits rather than starting.
	if err := updateInstanceRow(st, "omni-instance", map[string]any{"sleep_level": 0}); err != nil {
		t.Fatal(err)
	}
	p.SetDraining(depID, []string{"omni-instance"}, true)
	p.SetWakeDemand(func(uint) {
		go func() {
			time.Sleep(150 * time.Millisecond)
			p.SetDraining(depID, []string{"omni-instance"}, false)
		}()
	})
	start := time.Now()
	resp := postChat(t, front, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Fatalf("a draining deployment took a new request after %v", waited)
	}
	if p.hold.drainingOf(depID) != nil {
		t.Fatal("still draining")
	}
}

func TestInFlightCountsTheDeploymentsServedRequests(t *testing.T) {
	var a *Accountant
	if a.InFlight(1) != 0 {
		t.Fatal("a nil accountant counts requests")
	}
	acct := newAccountant(nil, 4, nil)
	acct.Dispatch(5)
	acct.Dispatch(5)
	if got := acct.InFlight(5); got != 2 {
		t.Fatalf("in flight = %d, want 2", got)
	}
	if got := acct.InFlight(6); got != 0 {
		t.Fatalf("another deployment's in flight = %d", got)
	}
}
