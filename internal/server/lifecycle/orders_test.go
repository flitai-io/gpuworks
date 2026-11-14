package lifecycle

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The queue a scale-up names is tried in order, one node per replica start;
// a start that tried another node leaves it alone; naming again replaces it;
// forgetting drops it.
func TestPlacementHintsQueueByDeployment(t *testing.T) {
	var o Orders
	if _, ok := o.NextPlacementHint(1); ok {
		t.Fatal("an empty Orders has a hint")
	}
	o.HintPlacement(1, []string{"a", "b"})
	o.HintPlacement(2, []string{"c"})
	o.ConsumePlacementHint(1, "b") // not the first: ignored
	if next, _ := o.NextPlacementHint(1); next != "a" {
		t.Fatalf("next = %q, want a", next)
	}
	o.ConsumePlacementHint(1, "a")
	if next, _ := o.NextPlacementHint(1); next != "b" {
		t.Fatalf("after a, next = %q, want b", next)
	}
	o.ConsumePlacementHint(1, "b")
	if next, ok := o.NextPlacementHint(1); ok {
		t.Fatalf("the queue is spent, yet %q is next", next)
	}
	o.HintPlacement(2, []string{"d"})
	if next, _ := o.NextPlacementHint(2); next != "d" {
		t.Fatalf("naming again did not replace the queue: %q", next)
	}
	o.ForgetPlacementHint(2)
	if _, ok := o.NextPlacementHint(2); ok {
		t.Fatal("a forgotten deployment kept its hint")
	}
}

// The caller's slice is copied: changing it afterwards does not change the
// queue.
func TestHintPlacementCopiesTheNodes(t *testing.T) {
	var o Orders
	nodes := []string{"a"}
	o.HintPlacement(1, nodes)
	nodes[0] = "changed"
	if next, _ := o.NextPlacementHint(1); next != "a" {
		t.Fatalf("next = %q, want a", next)
	}
}

// Two servers in one process (the test harness runs several) keep their own
// hints and their own lock.
func TestTwoOrdersShareNothing(t *testing.T) {
	var first, second Orders
	first.HintPlacement(1, []string{"a"})
	if _, ok := second.NextPlacementHint(1); ok {
		t.Fatal("a hint given to one server reached another")
	}
	unlock := first.Lock()
	defer unlock()
	done := make(chan struct{})
	go func() { second.Lock()(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("one server's order lock held another's")
	}
}

// The lock lets one order in at a time.
func TestOrdersLockExcludes(t *testing.T) {
	var o Orders
	var inside, most atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := o.Lock()
			n := inside.Add(1)
			for {
				m := most.Load()
				if n <= m || most.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
			unlock()
		}()
	}
	wg.Wait()
	if most.Load() != 1 {
		t.Fatalf("%d orders held the lock at once", most.Load())
	}
}
