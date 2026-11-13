package bus

import (
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestPublishDeliver(t *testing.T) {
	b := New()
	sub := b.Subscribe(8, "node")
	defer sub.Close()

	b.Publish(Event{Topic: "node", Key: "n1", Reason: "registered"})
	select {
	case ev := <-sub.C():
		if ev.Key != "n1" {
			t.Errorf("key = %q", ev.Key)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered")
	}
}

func TestTopicFilter(t *testing.T) {
	b := New()
	sub := b.Subscribe(8, "node")
	defer sub.Close()

	b.Publish(Event{Topic: "gpu", Key: "x"}) // filtered out
	b.Publish(Event{Topic: "node", Key: "y"})
	select {
	case ev := <-sub.C():
		if ev.Key != "y" {
			t.Errorf("expected filtered stream to yield y, got %q", ev.Key)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

func TestCoalesceOnBackpressure(t *testing.T) {
	b := New()
	sub := b.Subscribe(1) // tiny buffer forces coalescing
	defer sub.Close()

	// Fill the buffer, then push many same-key events; they must coalesce.
	for i := 0; i < 50; i++ {
		b.Publish(Event{Topic: "node", Key: "same", Reason: "r"})
	}
	// Drain what the pump delivers.
	drained := 0
	timeout := time.After(200 * time.Millisecond)
loop:
	for {
		select {
		case <-sub.C():
			drained++
		case <-timeout:
			break loop
		}
	}
	if drained == 0 {
		t.Fatal("expected at least one delivery")
	}
	if sub.CoalescedCount() == 0 {
		t.Error("expected coalescing under backpressure")
	}
	if drained > 5 {
		t.Errorf("same-key events should coalesce, drained %d", drained)
	}
}

// A progress notice never displaces a held state change: the state change is
// always delivered, whatever progress arrives after it. Delivery order is not
// asserted — a progress publish that lands after the pump claimed the state
// change may well be delivered after it — only that the state change is not
// swallowed.
func TestCoalescingKeepsAStateChangeAheadOfLaterProgress(t *testing.T) {
	b := New()
	sub := b.Subscribe(1)
	defer sub.Close()

	b.Publish(Event{Topic: "instance", Key: "filler", Reason: "status"}) // occupies the buffer
	b.Publish(Event{Topic: "instance", Key: "i1", Reason: ReasonProgress})
	b.Publish(Event{Topic: "instance", Key: "i1", Reason: "status"})
	for i := 0; i < 20; i++ {
		b.Publish(Event{Topic: "instance", Key: "i1", Reason: ReasonProgress})
	}
	// A key that only ever saw progress still reports the newest progress.
	b.Publish(Event{Topic: "instance", Key: "i2", Reason: ReasonProgress + ":1"})
	b.Publish(Event{Topic: "instance", Key: "i2", Reason: ReasonProgress + ":2"})

	seen := map[string]bool{}
	timeout := time.After(time.Second)
	for !seen["i1/status"] || !seen["i2/"+ReasonProgress+":2"] {
		select {
		case ev := <-sub.C():
			seen[ev.Key+"/"+ev.Reason] = true
		case <-timeout:
			t.Fatalf("timed out; delivered so far: %v", seen)
		}
	}
}

// Generation moves with a topic's state changes and stays put for progress
// notices and other topics: a cache keyed on it rebuilds when its objects
// changed and only then.
func TestGenerationCountsATopicsStateChanges(t *testing.T) {
	b := New()
	if got := b.Generation("instance"); got != 0 {
		t.Fatalf("fresh generation = %d", got)
	}
	b.PublishChange("instance", "i1", "state:running")
	b.PublishChange("instance", "i1", ReasonProgress)
	b.PublishChange("node", "n1", "status")
	b.PublishChange("instance", "i2", "created")
	if got := b.Generation("instance"); got != 2 {
		t.Fatalf("instance generation = %d, want 2 (progress and other topics excluded)", got)
	}
	if got := b.Generation("node"); got != 1 {
		t.Fatalf("node generation = %d, want 1", got)
	}
}

// A burst larger than the buffer leaves events in pending. Once the consumer
// drains the channel, those events must keep flowing with no further publish
// and no test-side flush: the flusher owns moving them.
func TestPendingFlushesWhenConsumerDrainsTheBuffer(t *testing.T) {
	b := New()
	sub := b.Subscribe(2)
	defer sub.Close()

	const keys = 10
	for i := 0; i < keys; i++ {
		b.Publish(Event{Topic: "node", Key: "k" + strconv.Itoa(i), Reason: "status"})
	}

	got := map[string]string{}
	timeout := time.After(2 * time.Second)
	for len(got) < keys {
		select {
		case ev := <-sub.C():
			got[ev.Key] = ev.Reason
		case <-timeout:
			t.Fatalf("delivered %d/%d keys after one burst; stranded pending was never flushed: %v", len(got), keys, got)
		}
	}
}

// The last state change for a key must still win after a progress storm that
// overflows the buffer: the flusher delivers the coalesced newest event, not
// the oldest one it happened to hold when it started.
func TestFlusherDeliversTheLastStateChangeAfterAProgressStorm(t *testing.T) {
	b := New()
	sub := b.Subscribe(1)
	defer sub.Close()

	b.Publish(Event{Topic: "instance", Key: "filler", Reason: "status"}) // fills the buffer
	for i := 0; i < 100; i++ {
		b.Publish(Event{Topic: "instance", Key: "i1", Reason: ReasonProgress})
	}
	b.Publish(Event{Topic: "instance", Key: "i1", Reason: "state:running"})

	got := map[string]string{}
	timeout := time.After(2 * time.Second)
	for got["i1"] != "state:running" {
		select {
		case ev := <-sub.C():
			got[ev.Key] = ev.Reason
		case <-timeout:
			t.Fatalf("last state change never delivered without a manual flush; reasons=%v", got)
		}
	}
}

// A full buffer and a stalled consumer must not block the publisher: the
// flusher waits on the channel, the publishing path never does.
func TestPublishNeverBlocksWhenTheConsumerStalls(t *testing.T) {
	b := New()
	sub := b.Subscribe(1)
	defer sub.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			b.Publish(Event{Topic: "instance", Key: "k" + strconv.Itoa(i%5), Reason: "status"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishing blocked behind a full subscriber buffer")
	}
}

// Close stops a flusher that is parked on a send nobody will ever receive,
// and stays safe when called twice.
func TestCloseStopsAStalledFlusher(t *testing.T) {
	b := New()
	sub := b.Subscribe(1)
	b.Publish(Event{Topic: "node", Key: "fills", Reason: "status"})
	b.Publish(Event{Topic: "node", Key: "pending", Reason: "status"})
	sub.Close()
	sub.Close() // idempotent: closing twice must not panic
	b.Publish(Event{Topic: "node", Key: "after-close", Reason: "status"})
}

// Per-key publish order survives coalescing. While a backlog exists the pump is
// the sole writer to the channel: a publisher that took the fast path then could
// overtake an event the pump has already claimed, and the consumer would see a
// newer reason before an older one for the same key. Several paced streams give
// thousands of chances to hit that window.
func TestCoalescedDeliveryKeepsPerKeyPublishOrder(t *testing.T) {
	const rounds, count = 4, 1500
	for round := 0; round < rounds; round++ {
		b := New()
		sub := b.Subscribe(1) // a full buffer leaves the pump holding events
		newest := "r" + strconv.Itoa(count-1)

		got := make(chan []string, 1)
		go func() {
			delivered := make([]string, 0, count)
			for {
				select {
				case ev := <-sub.C():
					delivered = append(delivered, ev.Reason)
					// The newest reason always survives coalescing, so its
					// arrival means the backlog for this key is drained.
					if ev.Reason == newest {
						got <- delivered
						return
					}
				case <-time.After(5 * time.Second):
					got <- delivered
					return
				}
				// A slow consumer leaves the channel empty between reads,
				// opening the window a bypassing publisher would need.
				runtime.Gosched()
				time.Sleep(20 * time.Microsecond)
			}
		}()
		for i := 0; i < count; i++ {
			runtime.Gosched()
			b.Publish(Event{Topic: "instance", Key: "k", Reason: "r" + strconv.Itoa(i)})
			time.Sleep(20 * time.Microsecond)
		}

		var delivered []string
		select {
		case delivered = <-got:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: consumer did not settle", round)
		}
		sub.Close()

		// Coalescing drops intermediate events, so what arrives is a subsequence
		// of what was published — but never out of order.
		last := -1
		for _, reason := range delivered {
			if len(reason) < 2 || reason[0] != 'r' {
				t.Fatalf("round %d: unexpected reason %q", round, reason)
			}
			i, err := strconv.Atoi(reason[1:])
			if err != nil {
				t.Fatalf("round %d: unexpected reason %q: %v", round, reason, err)
			}
			if i <= last {
				t.Fatalf("round %d: delivered out of publish order: r%d after r%d (%v)", round, i, last, delivered)
			}
			last = i
		}
		if len(delivered) == 0 || delivered[len(delivered)-1] != newest {
			t.Fatalf("round %d: delivered %v, want it to end with %q", round, delivered, newest)
		}
	}
}
