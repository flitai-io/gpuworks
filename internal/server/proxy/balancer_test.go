package proxy

import (
	"sync"
	"testing"
	"time"
)

func TestBalancerLeastRequestSpreadsConcurrentLeases(t *testing.T) {
	b := newBalancer(nil)
	candidates := []candidate{
		{instanceID: 1, instanceUUID: "a", nodeUUID: "node-a", modelName: "model"},
		{instanceID: 2, instanceUUID: "b", nodeUUID: "node-b", modelName: "model"},
		{instanceID: 3, instanceUUID: "c", nodeUUID: "node-c", modelName: "model"},
	}
	leases := make(chan *lease, 30)
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, _ := b.pick(candidates, nil, func(string) bool { return true }, admission{})
			if lease == nil {
				t.Error("pick returned nil")
				return
			}
			leases <- lease
		}()
	}
	wg.Wait()
	close(leases)
	allLeases := make([]*lease, 0, 30)
	for lease := range leases {
		allLeases = append(allLeases, lease)
	}
	for _, lease := range allLeases {
		lease.release(outcomeOK)
	}
	for _, uuid := range []string{"a", "b", "c"} {
		if got := b.states[uuid].inflight.total; got != 0 {
			t.Fatalf("instance %s inflight=%d after release, want 0", uuid, got)
		}
	}
	counts := map[string]int{}
	for _, lease := range allLeases {
		counts[lease.c.instanceUUID]++
	}
	for _, uuid := range []string{"a", "b", "c"} {
		if counts[uuid] < 8 || counts[uuid] > 12 {
			t.Fatalf("instance %s received %d concurrent leases, want near-even distribution: %v", uuid, counts[uuid], counts)
		}
	}
}

func TestBalancerFailoverHonorsCandidatePriorityAndCooldown(t *testing.T) {
	b := newBalancer(nil)
	b.now = func() time.Time { return time.Unix(100, 0) }
	candidates := []candidate{
		{instanceID: 9, instanceUUID: "new", nodeUUID: "node", modelName: "model"},
		{instanceID: 8, instanceUUID: "old", nodeUUID: "node", modelName: "model"},
	}
	first, _ := b.pickFailover(candidates, nil, func(string) bool { return true }, admission{})
	if first == nil || first.c.instanceUUID != "new" {
		t.Fatalf("first failover target=%v, want new", first)
	}
	first.release(outcomeConnFailure)
	second, _ := b.pickFailover(candidates, nil, func(string) bool { return true }, admission{})
	if second == nil || second.c.instanceUUID != "old" {
		t.Fatalf("cooldown failover target=%v, want old", second)
	}
	second.release(outcomeOK)
}

func TestBalancerAffinityIsDeterministic(t *testing.T) {
	candidates := []candidate{
		{instanceUUID: "a", nodeUUID: "node-a", modelName: "model"},
		{instanceUUID: "b", nodeUUID: "node-b", modelName: "model"},
		{instanceUUID: "c", nodeUUID: "node-c", modelName: "model"},
	}
	left, right := newBalancer(nil), newBalancer(nil)
	a, _ := left.pickAffinity(candidates, "conversation-1", nil, func(string) bool { return true }, admission{})
	b, _ := right.pickAffinity(candidates, "conversation-1", nil, func(string) bool { return true }, admission{})
	if a == nil || b == nil || a.c.instanceUUID != b.c.instanceUUID {
		t.Fatalf("affinity choices differ: %v and %v", a, b)
	}
	a.release(outcomeOK)
	b.release(outcomeOK)
}
