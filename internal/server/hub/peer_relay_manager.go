package hub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

const maxPeerObservations = 8192

// peerRelayQueue is how admission waits. queueWait bounds how long an
// admission waits for a relay slot before returning busy: the receiver listens
// for ten minutes and nothing on either agent is blocked meanwhile, so waiting
// here replaces a retry loop that redialed the source and minted new
// credentials every two seconds. A probe answers an interactive request and
// gives up sooner. ticketTTL keeps a task's place across the coordinator's
// retries. retryHold keeps a slot for an earlier task between its retries:
// after its wait ends it re-listens and redials (a black-holed direct dial
// takes about ten seconds) before asking again, and without the hold a task
// arriving in that gap takes the slot ahead of it.
type peerRelayQueue struct {
	queueWait, probeWait, ticketTTL, retryHold time.Duration
}

func defaultPeerRelayQueue() peerRelayQueue {
	return peerRelayQueue{queueWait: time.Minute, probeWait: 5 * time.Second, ticketTTL: 3 * time.Minute, retryHold: 15 * time.Second}
}

// peerRelayTicket is one task's place in the admission queue. Its key outlives
// a transfer ID (each retry mints a new one), so a retry keeps its arrival
// order; active counts admissions currently waiting on it.
type peerRelayTicket struct {
	source, target string
	since, seen    time.Time
	active         int
}

type peerRateSample struct {
	second int64
	bytes  int64
}
type peerTransfer struct {
	observation    prepobs.PeerTransferObservation
	source, target *Session
	ctx            context.Context
	cancel         context.CancelCauseFunc
	lease          *peerRelayLease
	samples        [5]peerRateSample
}

type peerRelayManager struct {
	mu       sync.Mutex
	policy   prepobs.PeerRelayPolicy
	started  time.Time
	now      func() time.Time
	entries  map[string]*peerTransfer
	leases   map[string]*peerRelayLease
	nodes    map[string]int
	totals   prepobs.PeerTransferTotals
	bucket   peerTokenBucket
	reserved int64
	order    []*peerRelayLease
	cursor   int
	wake     chan struct{}
	running  bool
	// tickets and released implement FIFO admission: released is closed and
	// replaced whenever a slot frees, waking every waiting acquire.
	tickets  map[string]*peerRelayTicket
	released chan struct{}
	queue    peerRelayQueue
}

func newPeerRelayManager(p prepobs.PeerRelayPolicy, clocks ...func() time.Time) *peerRelayManager {
	clock := time.Now
	if len(clocks) > 0 && clocks[0] != nil {
		clock = clocks[0]
	}
	now := clock()
	return &peerRelayManager{policy: p, started: now, now: clock, entries: make(map[string]*peerTransfer), leases: make(map[string]*peerRelayLease), nodes: make(map[string]int), bucket: newPeerTokenBucket(p.RateLimitBytesPerSecond, p.BurstBytes, now), wake: make(chan struct{}, 1), tickets: make(map[string]*peerRelayTicket), released: make(chan struct{}), queue: defaultPeerRelayQueue()}
}

func (h *Hub) peerManager() *peerRelayManager {
	h.peerRelayMu.Lock()
	defer h.peerRelayMu.Unlock()
	if h.peerRelayManager == nil {
		h.peerRelayManager = newPeerRelayManager(prepobs.DefaultPeerRelayPolicy(), h.now)
	}
	return h.peerRelayManager
}

// SetPeerRelayPolicy is a startup-only policy change. An active relay keeps its
// complete policy until release; callers cannot reset a live token bucket.
func (h *Hub) SetPeerRelayPolicy(p prepobs.PeerRelayPolicy) error {
	if p.RateLimitBytesPerSecond <= 0 || p.BurstBytes <= 0 || p.QuantumBytes <= 0 || p.MaxActive <= 0 || p.MaxPerNode <= 0 || p.NoProgressTimeout <= 0 || p.ConfirmationTimeout <= 0 {
		return errors.New("peer relay policy requires positive limits and timeouts")
	}
	if p.QuantumBytes > prepobs.DefaultPeerRelayPolicy().QuantumBytes || p.QuantumBytes > p.BurstBytes/int64(p.MaxActive)/2 {
		return errors.New("peer relay quantum exceeds fair burst reservation")
	}
	m := h.peerManager()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.leases) > 0 {
		return errors.New("cannot change peer relay policy while active")
	}
	m.policy = p
	m.bucket = newPeerTokenBucket(p.RateLimitBytesPerSecond, p.BurstBytes, m.now())
	return nil
}

// beginPeerTransfer registers an operation owned by its coordinator, including
// direct transfers. A full observation registry must not prevent real work.
func (h *Hub) beginPeerTransfer(ctx context.Context, spec prepobs.PeerTransferSpec) (context.Context, func(string, string)) {
	m := h.peerManager()
	source, target := h.session(spec.SourceNodeUUID), h.session(spec.TargetNodeUUID)
	ctx, cancel := context.WithCancelCause(ctx)
	now := m.now()
	e := &peerTransfer{observation: prepobs.PeerTransferObservation{PeerTransferSpec: spec, Phase: "preparing", StartedAt: now, UpdatedAt: now, RateWindowSeconds: 5}, source: source, target: target, ctx: ctx, cancel: cancel}
	m.mu.Lock()
	if existing := m.entries[spec.TransferID]; existing != nil {
		m.mu.Unlock()
		cancel(context.Canceled)
		return existing.ctx, func(string, string) {}
	}
	if len(m.entries) < maxPeerObservations {
		m.entries[spec.TransferID] = e
	}
	if spec.Operation == "probe" {
		m.totals.ProbeStarted++
	} else {
		m.totals.Started++
	}
	m.mu.Unlock()
	var once sync.Once
	return ctx, func(outcome, reason string) {
		once.Do(func() {
			m.mu.Lock()
			if m.entries[spec.TransferID] == e {
				delete(m.entries, spec.TransferID)
			}
			if spec.Operation == "probe" {
				if outcome == "probe_ok" || outcome == "peer_confirmed" {
					m.totals.ProbeOK++
				} else {
					m.totals.ProbeFailed++
				}
			} else {
				switch outcome {
				case "peer_confirmed":
					m.totals.PeerConfirmed++
				case "canceled":
					m.totals.Canceled++
				case "busy":
					m.totals.Busy++
				case "unconfirmed":
					// The target already had an attempt running; nothing
					// failed, so it is not counted as a failure.
					m.totals.Unconfirmed++
				default:
					m.totals.Failed++
				}
			}
			m.mu.Unlock()
			cancel(context.Canceled)
		})
	}
}
func (h *Hub) CancelPeerTransfer(id string) {
	m := h.peerManager()
	m.mu.Lock()
	e := m.entries[id]
	l := m.leases[id]
	m.mu.Unlock()
	if e != nil {
		e.cancel(context.Canceled)
	} else if l != nil {
		l.cancel(context.Canceled)
	}
}
func (h *Hub) updatePeerTransfer(id, transport, phase, reason, fallbackReason string) {
	m := h.peerManager()
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[id]; e != nil {
		if transport != "" {
			e.observation.Transport = transport
		}
		if phase != "" {
			e.observation.Phase = phase
		}
		if reason != "" {
			e.observation.Reason = reason
		}
		if fallbackReason != "" {
			e.observation.FallbackReason = fallbackReason
		}
		e.observation.UpdatedAt = m.now()
	}
}
func (h *Hub) recordPeerTransferPhase(source *Session, frame proto.PeerTransferPhase) bool {
	if source == nil || (frame.Phase != "connected" && frame.Phase != "confirming") || (frame.Transport != "direct" && frame.Transport != "relay") {
		return false
	}
	current := h.session(source.NodeUUID)
	m := h.peerManager()
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[frame.TransferID]
	if e == nil || current != source || e.source != source || e.ctx.Err() != nil {
		return false
	}
	if (e.lease != nil) != (frame.Transport == "relay") {
		return false
	}
	e.observation.Transport = frame.Transport
	// The source may finish before the server drains the encrypted output. Only
	// that actual EOF starts a relay confirmation deadline.
	if (e.lease == nil || frame.Phase != "confirming") && e.observation.Phase != "confirming" {
		e.observation.Phase = frame.Phase
	}
	e.observation.UpdatedAt = m.now()
	return true
}

func (m *peerRelayManager) fitsLocked(source, target string) bool {
	return len(m.leases) < m.policy.MaxActive && m.nodes[source] < m.policy.MaxPerNode && m.nodes[target] < m.policy.MaxPerNode
}

// queueKey is the task a transfer belongs to: the same resource copied to the
// same target by the same operation, whatever its current transfer ID.
func (m *peerRelayManager) queueKeyLocked(id string) string {
	if e := m.entries[id]; e != nil && e.observation.ResourceKey != "" {
		o := e.observation
		return o.Kind + "\x00" + o.Operation + "\x00" + o.ResourceKey + "\x00" + o.TargetNodeUUID
	}
	return "id\x00" + id
}

// firstLocked reports whether key may take a free slot now: no earlier ticket
// (waiting, or between retries within queue.retryHold) would be left
// without room by it. A waiter whose nodes are busy does not hold back one
// whose nodes are free.
func (m *peerRelayManager) firstLocked(key string, t *peerRelayTicket, now time.Time) bool {
	for other, w := range m.tickets {
		if other == key || !w.since.Before(t.since) || (w.active == 0 && now.Sub(w.seen) >= m.queue.retryHold) {
			continue
		}
		// Stand aside only if taking a slot now would leave none for the
		// earlier task; when both fit, both go.
		if m.fitsLocked(w.source, w.target) && !m.fitsAfterLocked(w.source, w.target, t.source, t.target) {
			return false
		}
	}
	return true
}

// fitsAfterLocked reports whether source→target still fits once a relay
// between admittedSource and admittedTarget has been admitted.
func (m *peerRelayManager) fitsAfterLocked(source, target, admittedSource, admittedTarget string) bool {
	count := func(node string) int {
		n := m.nodes[node]
		if node == admittedSource {
			n++
		}
		if node == admittedTarget {
			n++
		}
		return n
	}
	return len(m.leases)+1 < m.policy.MaxActive && count(source) < m.policy.MaxPerNode && count(target) < m.policy.MaxPerNode
}

// retriesLocked reports whether the transfer's coordinator retries after busy:
// copies do; probes answer their caller with busy and are done.
func (m *peerRelayManager) retriesLocked(id string) bool {
	e := m.entries[id]
	return e != nil && e.observation.ResourceKey != "" && e.observation.Operation != "probe"
}

// queueWaitLocked is how long an admission waits. A probe answers an
// interactive request, so it gives up sooner than a copy.
func (m *peerRelayManager) queueWaitLocked(id string) time.Duration {
	if e := m.entries[id]; e != nil && e.observation.Operation == "probe" {
		return m.queue.probeWait
	}
	return m.queue.queueWait
}

func (m *peerRelayManager) setWaitingLocked(id string, waiting bool, now time.Time) {
	e := m.entries[id]
	if e == nil {
		return
	}
	if waiting {
		e.observation.Phase = "waiting"
		e.observation.Reason = "capacity_busy"
	} else if e.observation.Reason == "capacity_busy" {
		e.observation.Reason = ""
	}
	e.observation.UpdatedAt = now
}

// acquire admits a relay in arrival order, waiting up to queue.queueWait
// for a slot. Busy means the wait ran out; the caller retries and keeps its
// place through its ticket.
func (m *peerRelayManager) acquire(ctx context.Context, id, source, target string, userRate int64) (*peerRelayLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leases[id] != nil {
		return nil, errors.New("peer relay transfer is already active")
	}
	key := m.queueKeyLocked(id)
	now := m.now()
	for k, w := range m.tickets {
		if w.active == 0 && now.Sub(w.seen) > m.queue.ticketTTL {
			delete(m.tickets, k)
		}
	}
	ticket := m.tickets[key]
	if ticket == nil {
		ticket = &peerRelayTicket{source: source, target: target, since: now}
		m.tickets[key] = ticket
	}
	ticket.source, ticket.target, ticket.seen = source, target, now
	ticket.active++
	deadline := time.NewTimer(m.queueWaitLocked(id))
	defer deadline.Stop()
	for !m.fitsLocked(source, target) || !m.firstLocked(key, ticket, now) {
		m.setWaitingLocked(id, true, now)
		released := m.released
		m.mu.Unlock()
		// A held place lapses on its own; recheck then even if nothing is
		// released, or a waiter behind it sleeps until its deadline.
		hold := time.NewTimer(m.queue.retryHold)
		var err error
		select {
		case <-released:
		case <-hold.C:
		case <-ctx.Done():
			err = context.Cause(ctx)
		case <-deadline.C:
			err = messages.PeerRelayBusy()
		}
		hold.Stop()
		m.mu.Lock()
		now = m.now()
		if err != nil {
			ticket.active--
			ticket.seen = now
			// Only a copy that ran out of waiting comes back for its place;
			// a probe, a cancel or an untracked transfer does not.
			if ticket.active == 0 && (!errors.Is(err, messages.ErrPeerRelayBusy) || !m.retriesLocked(id)) {
				delete(m.tickets, key)
			}
			m.setWaitingLocked(id, false, now)
			// Another waiter may have been holding back for this one.
			m.releaseLocked()
			return nil, err
		}
	}
	ticket.active--
	if ticket.active == 0 {
		delete(m.tickets, key)
	}
	m.setWaitingLocked(id, false, now)
	// A waiter that stood aside for this one must recheck now, not at its
	// deadline: it may fit in a slot that is still free.
	m.releaseLocked()
	if m.leases[id] != nil {
		return nil, errors.New("peer relay transfer is already active")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	rate := m.policy.RateLimitBytesPerSecond
	if userRate > 0 && userRate < rate {
		rate = userRate
	}
	l := &peerRelayLease{manager: m, id: id, source: source, target: target, ctx: ctx, cancel: cancel, rate: rate, bucket: newPeerTokenBucket(rate, m.policy.BurstBytes, now), clock: now, wake: make(chan struct{}, 1), done: make(chan struct{})}
	m.leases[id] = l
	m.nodes[source]++
	m.nodes[target]++
	m.order = append(m.order, l)
	if e := m.entries[id]; e != nil {
		e.lease = l
		e.observation.Transport = "relay"
		e.observation.Phase = "connecting"
		e.observation.EffectiveRateLimitBytesPerSecond = rate
		e.observation.UpdatedAt = now
	}
	go l.watch()
	return l, nil
}
func (l *peerRelayLease) release() {
	l.cancel(context.Canceled)
	<-l.done
	m := l.manager
	m.mu.Lock()
	if m.leases[l.id] == l {
		delete(m.leases, l.id)
		m.nodes[l.source]--
		if m.nodes[l.source] == 0 {
			delete(m.nodes, l.source)
		}
		m.nodes[l.target]--
		if m.nodes[l.target] == 0 {
			delete(m.nodes, l.target)
		}
		for i, v := range m.order {
			if v == l {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
		if e := m.entries[l.id]; e != nil && e.lease == l {
			e.lease = nil
		}
	}
	m.releaseLocked()
	m.signalLocked()
	m.mu.Unlock()
}

// releaseLocked wakes every waiting acquire to recheck admission.
func (m *peerRelayManager) releaseLocked() {
	close(m.released)
	m.released = make(chan struct{})
}
func (l *peerRelayLease) timeout(reason string) {
	m := l.manager
	m.mu.Lock()
	if e := m.entries[l.id]; e != nil {
		e.observation.Reason = reason
		e.observation.UpdatedAt = m.now()
	}
	m.mu.Unlock()
	l.cancel(fmt.Errorf("peer relay %s", reason))
}
