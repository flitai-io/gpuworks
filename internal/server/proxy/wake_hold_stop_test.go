package proxy

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// stoppingBody is the literal wire body of a request refused because the
// server stops.
const stoppingBody = `{"error":{"message":"the server is restarting; send the request again","type":"gpuworks_proxy"}}` + "\n"

// waitHeld waits until the gateway holds n requests for the deployment.
func waitHeld(t *testing.T, p *Proxy, depID uint, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got, _ := p.HeldRequests(depID); got == n {
			return
		}
		if time.Now().After(deadline) {
			got, _ := p.HeldRequests(depID)
			t.Fatalf("held = %d, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertStoppingRefusal checks the response is the refusal of a stopping
// server: 503, a Retry-After that covers a restart, and the literal body.
func assertStoppingRefusal(t *testing.T, resp *http.Response) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "10" || string(body) != stoppingBody {
		t.Fatalf("status = %d Retry-After = %q body = %q", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}

// A request held for a wake when the server stops is answered at once with a
// refusal it may retry, not left to lose its connection when the server
// closes them; the hold's own limit does not keep it. That limit is longer
// than the answer may take after the stop, and short enough that a
// regression ends the request (and the test) within seconds.
func TestAHeldRequestIsRefusedForRetryWhenTheServerStops(t *testing.T) {
	p, _, key, depID, front := newSleepingProxy(t, false)
	p.hold.holdMax = func(*store.Deployment) time.Duration { return 5 * time.Second }
	p.SetWakeDemand(func(uint) {})
	got := make(chan *http.Response, 1)
	go func() { got <- postChat(t, front, key) }()
	waitHeld(t, p, depID, 1)

	stoppedAt := time.Now()
	p.StopHolding()
	select {
	case resp := <-got:
		assertStoppingRefusal(t, resp)
	case <-time.After(2 * time.Second):
		t.Fatal("the held request was not answered when the server stopped")
	}
	if took := time.Since(stoppedAt); took > time.Second {
		t.Fatalf("answered %v after the stop", took)
	}
	waitHeld(t, p, depID, 0)
}

// Once the server stops, a request for a sleeping deployment is refused for
// retry without being held, and without asking for the wake the next process
// would have to carry out.
func TestARequestAfterTheStopIsNotHeld(t *testing.T) {
	p, _, key, depID, front := newSleepingProxy(t, false)
	// A hold that outlives the test's patience shows a regression as a
	// timeout refusal within seconds, not as a hung test.
	p.hold.holdMax = func(*store.Deployment) time.Duration { return 2 * time.Second }
	var asked atomic.Int32
	p.SetWakeDemand(func(uint) { asked.Add(1) })
	p.StopHolding()
	p.StopHolding() // a second stop changes nothing
	assertStoppingRefusal(t, postChat(t, front, key))
	if n := asked.Load(); n != 0 {
		t.Fatalf("wake asked %d times after the stop", n)
	}
	if n, _ := p.HeldRequests(depID); n != 0 {
		t.Fatalf("held = %d after the stop", n)
	}
}

// An operator's held sleep keeps its own refusal after the stop: the request
// would not be held anyway, so it is not told a retry will succeed.
func TestAHeldSleepKeepsItsRefusalAfterTheStop(t *testing.T) {
	p, _, key, _, front := newSleepingProxy(t, true)
	p.SetWakeDemand(func(uint) {})
	p.StopHolding()
	resp := postChat(t, front, key)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "" || string(body) == stoppingBody {
		t.Fatalf("status = %d Retry-After = %q body = %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}

// The stop leaves requests to an awake deployment alone: only held requests
// are refused.
func TestTheStopLeavesAnAwakeDeploymentServing(t *testing.T) {
	p, st, key, _, front := newSleepingProxy(t, false)
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", "omni-instance").Update("sleep_level", 0).Error; err != nil {
		t.Fatal(err)
	}
	p.SetWakeDemand(func(uint) {})
	p.StopHolding()
	resp := postChat(t, front, key)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}
}

// The same gateway run again (an in-process restart starts it) holds
// requests for a wake once more.
func TestStartHoldsRequestsAgainAfterAStop(t *testing.T) {
	p, st, key, depID, front := newSleepingProxy(t, false)
	p.StopHolding()
	p.Start()
	p.SetWakeDemand(func(uint) {
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = st.DB().Model(&store.Instance{}).Where("uuid = ?", "omni-instance").Update("sleep_level", 0).Error
		}()
	})
	resp := postChat(t, front, key)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("status = %d body = %s, want the request held and answered after the wake", resp.StatusCode, body)
	}
	if n, _ := p.HeldRequests(depID); n != 0 {
		t.Fatalf("held = %d", n)
	}
}
