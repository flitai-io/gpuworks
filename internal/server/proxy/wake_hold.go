package proxy

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Bounds on the requests the gateway holds while their deployments wake.
const (
	// wakeHoldPerDeployment is how many requests one deployment may have
	// held; more are refused at once rather than queued without end.
	wakeHoldPerDeployment = 256
	// wakeHoldTotal bounds the held requests of all deployments together.
	wakeHoldTotal = 4096
	// wakeHoldRecheck is how often a held request looks again even when no
	// route change has signalled it, so a missed signal costs at most this.
	wakeHoldRecheck = time.Second
	// restartRetryAfter is the Retry-After of a request refused because the
	// server stops: about what a restart takes (a service manager's 3 s
	// restart delay, then the start), so a client that honours it lands on
	// the next process.
	restartRetryAfter = 10 * time.Second
)

// stoppingMessage tells a client of a request refused because the server
// stops that the request is safe to send again.
const stoppingMessage = "the server is restarting; send the request again"

// wakeHold holds the requests for deployments the platform put to sleep
// while they wake (docs/explanation/service-levels.md). A request waits until
// its deployment has an awake replica, its policy's HoldMax passes, or the
// client goes; every route table rebuild signals the waiters to look again.
type wakeHold struct {
	mu      sync.Mutex
	held    map[uint]map[*heldRequest]struct{}
	total   int
	changed chan struct{}
	// demand is told a deployment has a request waiting, so it is woken.
	demand func(deploymentID uint)
	now    func() time.Time
	// holdMax is how long a request for the deployment waits: its policy's
	// hold_max_sec.
	holdMax func(dep *store.Deployment) time.Duration
	// wakes says, of each deployment last refused for sleep, whether a
	// request wakes it (refuseAsleep), as read from its instance rows on that
	// refusal; holdForWake reads it right after. One entry per deployment
	// ever refused for sleep, so it is bounded by the deployments.
	wakes map[uint]bool
	// draining are the instances about to give their cards up, by
	// deployment (drain.go). Each set is replaced, never changed in place,
	// so a request reads the one it took under the lock after letting go.
	draining map[uint]map[string]bool
	// stopping is closed once the server stops (stop): a request held for a
	// wake then would end without an answer when the connections close, so
	// it is refused at once with a Retry-After instead.
	stopping chan struct{}
	stopped  bool
}

type heldRequest struct{ since time.Time }

func newWakeHold() *wakeHold {
	return &wakeHold{
		held: map[uint]map[*heldRequest]struct{}{}, changed: make(chan struct{}), now: time.Now, draining: map[uint]map[string]bool{}, wakes: map[uint]bool{},
		holdMax:  func(dep *store.Deployment) time.Duration { return dep.Policy().HoldMax() },
		stopping: make(chan struct{}),
	}
}

// stop refuses the held requests and every request held from now on.
func (h *wakeHold) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopped {
		h.stopped = true
		close(h.stopping)
	}
}

// resume holds requests again after stop: an App that runs again on the same
// gateway (the in-process restart) serves as a new process would.
func (h *wakeHold) resume() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		h.stopped = false
		h.stopping = make(chan struct{})
	}
}

// stopSignal is the channel closed when the server stops, read under the
// lock because resume replaces it.
func (h *wakeHold) stopSignal() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopping
}

// signal wakes every held request to look at the routes again.
func (h *wakeHold) signal() {
	h.mu.Lock()
	close(h.changed)
	h.changed = make(chan struct{})
	h.mu.Unlock()
}

// enter registers a held request, false when the bounds are full.
func (h *wakeHold) enter(deploymentID uint) (*heldRequest, <-chan struct{}, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	reqs := h.held[deploymentID]
	if len(reqs) >= wakeHoldPerDeployment || h.total >= wakeHoldTotal {
		return nil, nil, false
	}
	if reqs == nil {
		reqs = map[*heldRequest]struct{}{}
		h.held[deploymentID] = reqs
	}
	req := &heldRequest{since: h.now()}
	reqs[req] = struct{}{}
	h.total++
	return req, h.changed, true
}

func (h *wakeHold) next() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

func (h *wakeHold) leave(deploymentID uint, req *heldRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	reqs := h.held[deploymentID]
	if _, ok := reqs[req]; !ok {
		return
	}
	delete(reqs, req)
	h.total--
	if len(reqs) == 0 {
		delete(h.held, deploymentID)
	}
}

// Held is how many requests the deployment has held and since when the
// oldest has waited.
func (h *wakeHold) Held(deploymentID uint) (int, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var oldest time.Time
	for req := range h.held[deploymentID] {
		if oldest.IsZero() || req.since.Before(oldest) {
			oldest = req.since
		}
	}
	return len(h.held[deploymentID]), oldest
}

// HeldDeployments are the deployments with a request held.
func (h *wakeHold) HeldDeployments() []uint {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]uint, 0, len(h.held))
	for id := range h.held {
		out = append(out, id)
	}
	return out
}

// SetWakeDemand sets what is told a deployment has a request held for its
// wake. Without one, requests for a sleeping deployment are refused as before.
func (p *Proxy) SetWakeDemand(fn func(deploymentID uint)) {
	p.hold.mu.Lock()
	p.hold.demand = fn
	p.hold.mu.Unlock()
}

// StopHolding is the server stopping: the requests held for a wake, and any
// that would be held from now on, are refused at once with a 503 and a
// Retry-After that covers a restart. A held request would otherwise end with
// no answer when the connections close, which a client reads as a broken
// connection rather than a refusal it may retry. Requests already being
// served are not touched.
func (p *Proxy) StopHolding() {
	if p != nil && p.hold != nil {
		p.hold.stop()
	}
}

// HeldRequests is how many requests the gateway holds for the deployment's
// wake and since when the oldest has waited.
func (p *Proxy) HeldRequests(deploymentID uint) (int, time.Time) {
	return p.hold.Held(deploymentID)
}

// HeldDeployments are the deployments with a request held for their wake.
func (p *Proxy) HeldDeployments() []uint {
	return p.hold.HeldDeployments()
}

// noteAsleep records whether a request wakes the deployment, as its latest
// refusal for sleep read it.
func (h *wakeHold) noteAsleep(deploymentID uint, wakes bool) {
	h.mu.Lock()
	h.wakes[deploymentID] = wakes
	h.mu.Unlock()
}

// wakesOnRequest says a request for dep, just refused for sleep, is held for
// a wake: a sleeper of it wakes on request (store.Deployment.WakesOnRequest,
// per instance), as that refusal read its instances (refuseAsleep).
func (p *Proxy) wakesOnRequest(dep *store.Deployment) bool {
	if p.hold == nil || dep == nil {
		return false
	}
	p.hold.mu.Lock()
	defer p.hold.mu.Unlock()
	return p.hold.wakes[dep.ID]
}

// holdForWake holds a request for a sleeping deployment until the deployment
// can serve it, then selects again. It returns the selection's result; a
// request still not served when its time is up gets the asleep refusal with
// a Retry-After.
func (p *Proxy) holdForWake(ctx context.Context, w http.ResponseWriter, modelName, affinity string, dep *store.Deployment) (*store.Deployment, target, int, string, bool) {
	refuse := func() (*store.Deployment, target, int, string, bool) {
		return dep, target{}, http.StatusServiceUnavailable, asleepMessage(dep), true
	}
	if p.hold == nil {
		return refuse()
	}
	p.hold.mu.Lock()
	demand := p.hold.demand
	p.hold.mu.Unlock()
	if demand == nil || !p.wakesOnRequest(dep) {
		return refuse()
	}
	stopping := func() (*store.Deployment, target, int, string, bool) {
		w.Header().Set("Retry-After", strconv.Itoa(int(restartRetryAfter.Seconds())))
		return dep, target{}, http.StatusServiceUnavailable, stoppingMessage, true
	}
	stop := p.hold.stopSignal()
	select {
	case <-stop:
		return stopping()
	default:
	}
	req, changed, ok := p.hold.enter(dep.ID)
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wakeHoldRecheck.Seconds())*30))
		return refuse()
	}
	defer p.leaveHold(dep.ID, req)
	demand(dep.ID)
	hold := p.hold.holdMax(dep)
	deadline := time.NewTimer(hold)
	defer deadline.Stop()
	recheck := time.NewTicker(wakeHoldRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return dep, target{}, http.StatusServiceUnavailable, "the request ended while the model woke", true
		case <-deadline.C:
			w.Header().Set("Retry-After", strconv.Itoa(max(int(hold.Seconds()), 1)))
			return refuse()
		case <-stop:
			return stopping()
		case <-changed:
			changed = p.hold.next()
		case <-recheck.C:
		}
		got, tgt, status, msg, asleep := p.selectForRequest(ctx, modelName, affinity)
		if status == 0 || !asleep {
			return got, tgt, status, msg, asleep
		}
		if got != nil && got.ID == dep.ID && !p.wakesOnRequest(got) {
			// An operator or a schedule put its sleepers to sleep, or the
			// platform's sleepers were woken by hand, meanwhile.
			return got, tgt, status, msg, asleep
		}
	}
}
