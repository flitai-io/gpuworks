package kube

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/podresources"
)

// A deleted pod stays Terminating while the kubelet stops it, and its card
// stays allocated until it is gone. Borrowing takes every free card and
// returns the unused ones, so a second borrow right after the first must not
// see those cards still allocated to the returned hold pods.
func TestSequentialAcquiresOnAnIdleNodeBothSucceed(t *testing.T) {
	r := newRig(t, 8)
	r.srv.TerminationDelay = 120 * time.Millisecond // longer than the retry window: only waiting for the release can pass
	ctx := context.Background()
	for i, name := range []string{"gpuworks-111111111111", "gpuworks-222222222222"} {
		if _, err := r.d.Run(ctx, spec(name, i)); err != nil {
			t.Fatalf("Run #%d = %v", i+1, err)
		}
	}
	// Each borrow gave the seven cards it did not need back and waited for
	// the kubelet to free them, so nothing is left Terminating.
	if holds := r.holdPods(); len(holds) != 2 {
		t.Fatalf("hold pods = %v", holds)
	}
	for _, p := range r.srv.Pods() {
		if p.Metadata.DeletionTimestamp != nil {
			t.Fatalf("pod %s is still terminating", p.Metadata.Name)
		}
	}
}

// returning unused cards waits for the kubelet: a pool whose kubelet frees
// cards slowly still hands the next borrower a free node.
func TestReturnUnusedWaitsUntilTheKubeletFreedTheCards(t *testing.T) {
	r := newRig(t, 4)
	r.srv.TerminationDelay = 80 * time.Millisecond
	started := time.Now()
	if _, err := r.d.cards.acquire(context.Background(), "gpuworks-aaaaaaaaaaaa", []int{0}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took < 80*time.Millisecond {
		t.Fatalf("acquire returned after %s, before the returned cards were freed", took)
	}
	allocs, _ := r.kubelet.List(context.Background())
	if len(allocs) != 1 {
		t.Fatalf("allocations after acquire = %+v, want only the borrowed card", allocs)
	}
}

// A card the kubelet is still freeing (an idle sweep deleted its hold pod
// moments ago) is borrowed once it is free: acquire retries instead of
// failing with "another pod took them first".
func TestAcquireRetriesWhileTheKubeletFreesACard(t *testing.T) {
	r := newRig(t, 2)
	r.srv.TerminationDelay = 150 * time.Millisecond
	r.d.cards.borrowRetry = 5 * time.Second
	ctx := context.Background()
	r.d.SetIdleReturn(time.Minute)
	if _, err := r.d.Run(ctx, spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Remove(ctx, "gpuworks-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(2 * time.Minute)
	r.d.cards.sweep(ctx) // deletes the idle hold pod and does not wait for it
	if _, err := r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 0)); err != nil {
		t.Fatalf("Run right after the idle card was returned = %v", err)
	}
}

// A card allocated to a pod outside the platform is not retried for: the
// error names the pod at once.
func TestAcquireDoesNotRetryForACardAnotherPodHolds(t *testing.T) {
	r := newRig(t, 2)
	r.d.cards.borrowRetry = time.Minute
	r.kubelet.mu.Lock()
	r.kubelet.extra = append(r.kubelet.extra, podresources.Allocation{Namespace: "ml", Pod: "trainer", Resource: "nvidia.com/gpu", DeviceID: "GPU-a"})
	r.kubelet.mu.Unlock()
	started := time.Now()
	_, err := r.d.Run(context.Background(), spec("gpuworks-cccccccccccc", 0))
	if err == nil || !strings.Contains(err.Error(), "Pod ml/trainer") || time.Since(started) > 5*time.Second {
		t.Fatalf("Run = %v after %s", err, time.Since(started))
	}
	if users := r.d.cards.users; len(users) != 0 {
		t.Fatalf("a failed acquire left users behind: %v", users)
	}
}

// A borrow can take a minute (hold pods are admitted one by one). Status
// reporting, and instance bookkeeping, must not wait for it.
func TestStatusReportingIsNotBlockedByASlowBorrow(t *testing.T) {
	r := newRig(t, 2)
	gate := make(chan struct{})
	blocked := make(chan struct{}, 8)
	admit := r.kubelet.admit
	r.srv.Kubelet = func(p *kubeapi.Pod) {
		if strings.HasPrefix(p.Metadata.Name, holdPrefix) {
			blocked <- struct{}{}
			<-gate
		}
		admit(p)
	}
	ctx := context.Background()
	runDone := make(chan error, 1)
	go func() {
		_, err := r.d.Run(ctx, spec("gpuworks-dddddddddddd", 0))
		runDone <- err
	}()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the borrow never started")
	}
	finished := make(chan string, 1)
	go func() {
		if _, err := r.d.ExternalCardHolders(ctx); err != nil {
			finished <- "holders: " + err.Error()
			return
		}
		r.d.cards.release("gpuworks-nobody")
		r.d.cards.sweep(ctx)
		finished <- ""
	}()
	select {
	case msg := <-finished:
		if msg != "" {
			t.Fatal(msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("holders/release/sweep wait for the borrow in progress")
	}
	close(gate)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

// Borrows are serialized: two instances starting together both get a card,
// where two unserialized borrows would each take what the other needs.
func TestConcurrentAcquiresBothSucceed(t *testing.T) {
	r := newRig(t, 4)
	r.srv.TerminationDelay = 20 * time.Millisecond
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.d.Run(context.Background(), spec("gpuworks-"+strings.Repeat(string(rune('a'+i)), 12), i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Run #%d = %v", i, err)
		}
	}
}
