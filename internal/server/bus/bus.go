// Package bus is a tiny in-process event bus. Publishers (store methods, after
// commit) fan out to subscribers over per-subscriber bounded channels. When a
// subscriber's buffer is full, events for the same (topic,key) are coalesced
// rather than blocking the publisher — a slow SSE client must never stall a DB
// write. A per-subscriber pump goroutine drains that coalesced backlog as soon
// as the consumer makes room, so a burst's last state change per key lands
// without waiting for another overflow. Coalesced counts are tracked for
// observability.
package bus

import (
	"sync"
	"sync/atomic"
)

// ReasonProgress marks a change that only moved an object's progress counters
// forward: no state, phase, or presence changed. Subscribers that reconcile on
// state (the controller) skip it; subscribers that render it (SSE clients) do
// not. It is the difference between a redeploy storm re-planning every
// deployment on each downloaded megabyte and not.
const ReasonProgress = "progress"

// Event is a change notification. Payload is intentionally absent: subscribers
// re-read current state from the store, which keeps events idempotent and small.
type Event struct {
	Topic  string // e.g. "node"
	Key    string // e.g. node UUID
	Reason string // human hint: "registered", "status", "state:offline", "progress"
}

type subscriber struct {
	topics map[string]bool
	ch     chan Event
	// pending holds the latest event per coalesce-key while ch is full; the
	// pump moves them onto ch as the consumer drains it.
	mu      sync.Mutex
	pending map[string]Event
	order   []string
	// inflight counts the keys in pending plus the one event the pump has
	// claimed but not yet handed to ch. A publisher that reads 0 may deliver
	// straight to ch; any other value means the pump is (or is about to be)
	// writing, and events join pending to keep per-key publish order.
	inflight atomic.Int64
	// wake nudges the pump that pending is non-empty; stop ends it. Neither is
	// ever blocked on, so the publishing path stays non-blocking.
	wake chan struct{}
	stop chan struct{}
	// done closes when the pump has returned, letting Close wait it out.
	done      chan struct{}
	closeOnce sync.Once
	coalesce  atomic.Int64
}

// Bus routes events to subscribers.
type Bus struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
	// generations counts each topic's non-progress events (topic ->
	// *atomic.Uint64), for a reader that only asks whether anything changed
	// since it last looked and has no use for a subscription's events.
	generations sync.Map
}

// New creates an empty bus.
func New() *Bus {
	return &Bus{subs: make(map[*subscriber]struct{})}
}

// Subscription is the consumer handle returned by Subscribe.
type Subscription struct {
	bus *Bus
	sub *subscriber
}

// Subscribe registers interest in one or more topics (empty = all topics).
// buffer is the per-subscriber channel depth. The subscription owns a pump
// goroutine that runs until Close.
func (b *Bus) Subscribe(buffer int, topics ...string) *Subscription {
	if buffer <= 0 {
		buffer = 64
	}
	s := &subscriber{
		topics:  make(map[string]bool, len(topics)),
		ch:      make(chan Event, buffer),
		pending: make(map[string]Event),
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	for _, t := range topics {
		s.topics[t] = true
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	go s.pump()
	return &Subscription{bus: b, sub: s}
}

// C returns the receive channel. The pump drains coalesced events onto it, so a
// consumer only has to keep reading; it never flushes the backlog itself.
func (sub *Subscription) C() <-chan Event { return sub.sub.ch }

// Close removes the subscription and stops its pump. It is idempotent.
func (sub *Subscription) Close() {
	sub.bus.mu.Lock()
	delete(sub.bus.subs, sub.sub)
	sub.bus.mu.Unlock()
	sub.sub.closeOnce.Do(func() { close(sub.sub.stop) })
	<-sub.sub.done
}

// CoalescedCount reports how many events were merged due to backpressure.
func (sub *Subscription) CoalescedCount() int64 { return sub.sub.coalesce.Load() }

// PublishChange is a convenience for the common (topic, key, reason) publish.
func (b *Bus) PublishChange(topic, key, reason string) {
	b.Publish(Event{Topic: topic, Key: key, Reason: reason})
}

// Generation is how many non-progress events topic has seen. A cache derived
// from a topic's objects keeps the generation it was built at and is stale
// once the bus moves past it; reading it costs one atomic load and never a
// subscription that would have to be drained.
func (b *Bus) Generation(topic string) uint64 {
	if counter, ok := b.generations.Load(topic); ok {
		return counter.(*atomic.Uint64).Load()
	}
	return 0
}

// Publish delivers an event to all interested subscribers. Never blocks.
func (b *Bus) Publish(e Event) {
	if e.Reason != ReasonProgress {
		counter, ok := b.generations.Load(e.Topic)
		if !ok {
			counter, _ = b.generations.LoadOrStore(e.Topic, new(atomic.Uint64))
		}
		counter.(*atomic.Uint64).Add(1)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if len(s.topics) > 0 && !s.topics[e.Topic] {
			continue
		}
		s.deliver(e)
	}
}

func (s *subscriber) deliver(e Event) {
	// The pump is the only other writer, and it is busy only while inflight is
	// non-zero. With nothing queued, hand the event straight to the consumer.
	// Once anything is queued, this event joins pending too: a later state
	// change must not overtake an earlier one the pump still holds.
	if s.inflight.Load() == 0 {
		select {
		case s.ch <- e:
			return
		default:
		}
	}
	key := e.Topic + "\x00" + e.Key
	// Buffer full (or the pump is behind): coalesce by key. Newest wins, except
	// that progress never displaces a state change.
	s.mu.Lock()
	held, exists := s.pending[key]
	if exists {
		s.coalesce.Add(1)
	} else {
		s.order = append(s.order, key)
		s.inflight.Add(1)
	}
	// A progress notice must not replace a held state change: subscribers that
	// skip progress would then never learn the state moved.
	if !exists || e.Reason != ReasonProgress || held.Reason == ReasonProgress {
		s.pending[key] = e
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
		// A wake is already pending; the pump will see this event on its pass.
	}
}

// pump moves coalesced events onto the channel as the consumer makes room. The
// blocking send lives only here, so a stalled consumer parks this goroutine and
// never a publisher. It is the only writer while a backlog exists, which is why
// deliver must not bypass it then.
func (s *subscriber) pump() {
	defer close(s.done)
	for {
		select {
		case <-s.wake:
		case <-s.stop:
			return
		}
		for {
			e, ok := s.nextPending()
			if !ok {
				break
			}
			select {
			case s.ch <- e:
				// Delivered: only now may a publisher bypass the pump again.
				s.inflight.Add(-1)
			case <-s.stop:
				return
			}
		}
	}
}

// nextPending takes the oldest queued event out of pending. Its key still
// counts toward inflight until the event is on the channel, so publishers keep
// routing through pending while it is in flight.
func (s *subscriber) nextPending() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.order) == 0 {
		return Event{}, false
	}
	key := s.order[0]
	e := s.pending[key]
	delete(s.pending, key)
	s.order = s.order[1:]
	return e, true
}
