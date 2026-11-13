package kube

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// The engine only calls Remove, which releases an instance's cards, for a
// container Status still reports. A pod deleted by someone else leaves no
// container to report, so the periodic sweep finds such instances itself and
// returns their cards to Kubernetes after the idle time.
func TestSweepReleasesTheCardsOfAPodDeletedBehindTheAgent(t *testing.T) {
	r := newRig(t, 2)
	r.d.SetIdleReturn(time.Minute)
	ctx := context.Background()
	if _, err := r.d.Run(ctx, spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 1)); err != nil {
		t.Fatal(err)
	}
	if err := r.srv.Client().Delete(ctx, kubeapi.PodPath(testNS, "gpuworks-aaaaaaaaaaaa"), nil); err != nil { // kubectl delete pod
		t.Fatal(err)
	}
	r.now = r.now.Add(2 * time.Minute)
	r.d.cards.sweep(ctx) // notices the pod is gone: its card starts its idle time
	if holds := r.holdPods(); len(holds) != 2 {
		t.Fatalf("hold pods right after the pod vanished = %v", holds)
	}
	r.now = r.now.Add(2 * time.Minute)
	r.d.cards.sweep(ctx)
	holds := r.holdPods()
	if len(holds) != 1 || r.kubelet.given[testNS+"/"+holds[0]] != "GPU-b" {
		t.Fatalf("hold pods after the idle time = %v (given %v); want only the one of the instance still running", holds, r.kubelet.given)
	}
	// The card is borrowable again by a new instance.
	if _, err := r.d.Run(ctx, spec("gpuworks-cccccccccccc", 0)); err != nil {
		t.Fatalf("Run on the released card = %v", err)
	}
}

// Instances whose pod exists, is terminating, or is not created yet keep
// their cards.
func TestSweepKeepsTheCardsOfInstancesThatHaveAPodOrAreStarting(t *testing.T) {
	r := newRig(t, 3)
	r.d.SetIdleReturn(time.Minute)
	ctx := context.Background()
	for i, name := range []string{"gpuworks-aaaaaaaaaaaa", "gpuworks-bbbbbbbbbbbb"} {
		if _, err := r.d.Run(ctx, spec(name, i)); err != nil {
			t.Fatal(err)
		}
	}
	// Terminating: deleted, but still there.
	r.srv.TerminationDelay = time.Hour
	if err := r.srv.Client().Delete(ctx, kubeapi.PodPath(testNS, "gpuworks-bbbbbbbbbbbb"), nil); err != nil {
		t.Fatal(err)
	}
	// Starting: the cards are claimed, Run has not created the pod yet.
	if _, _, err := r.d.cards.claim(ctx, "gpuworks-cccccccccccc", []int{2}); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(time.Hour)
	r.d.cards.sweep(ctx)
	r.now = r.now.Add(time.Hour)
	r.d.cards.sweep(ctx)
	r.d.cards.mu.Lock()
	defer r.d.cards.mu.Unlock()
	for _, card := range []string{"GPU-a", "GPU-b", "GPU-c"} {
		if len(r.d.cards.users[card]) != 1 {
			t.Fatalf("users of %s = %v, want its instance kept", card, r.d.cards.users[card])
		}
	}
}

// A restarted agent finds the instance pods of the node; one that is gone
// again by the next sweep releases its cards like any other.
func TestSweepAfterRestartReleasesAVanishedPodsCards(t *testing.T) {
	r := newRig(t, 1)
	r.d.SetIdleReturn(time.Minute)
	ctx := context.Background()
	if _, err := r.d.Run(ctx, spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatal(err)
	}
	// A new agent process: the pool is rebuilt from the cluster.
	r.d.cards.mu.Lock()
	r.d.cards.loaded = false
	r.d.cards.mu.Unlock()
	if err := r.d.cards.keepAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.srv.Client().Delete(ctx, kubeapi.PodPath(testNS, "gpuworks-aaaaaaaaaaaa"), nil); err != nil {
		t.Fatal(err)
	}
	r.d.cards.sweep(ctx)
	r.now = r.now.Add(2 * time.Minute)
	r.d.cards.sweep(ctx)
	if holds := r.holdPods(); len(holds) != 0 {
		t.Fatalf("hold pods = %v", holds)
	}
}
