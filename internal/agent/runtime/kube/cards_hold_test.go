package kube

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The runtime check asks a card only while the platform holds it. A card
// Kubernetes may give to another pod (nobody holds it) is refused with the
// reason, and one the platform holds is kept from being returned meanwhile.
func TestHoldForCheckOnlyAdmitsCardsThePlatformHolds(t *testing.T) {
	r := newRig(t, 3)
	r.d.SetIdleReturn(time.Minute)
	ctx := context.Background()
	if _, err := r.d.Run(ctx, spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Remove(ctx, "gpuworks-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}

	// GPU-a is held (idle, in its grace period); GPU-b and GPU-c are free in
	// Kubernetes.
	release, why := r.d.HoldCardForCheck(ctx, "GPU-a")
	if why != "" || release == nil {
		t.Fatalf("HoldCardForCheck(held card) = %q", why)
	}
	for _, free := range []string{"GPU-b", "GPU-c"} {
		if rel, why := r.d.HoldCardForCheck(ctx, free); rel != nil || !strings.Contains(why, "Kubernetes may give to another pod") || !strings.Contains(why, free) {
			t.Fatalf("HoldCardForCheck(%s) = %v, %q; want a refusal that says why", free, rel != nil, why)
		}
	}

	// While the check holds the card its idle time running out does not
	// return it; afterwards it does.
	r.now = r.now.Add(5 * time.Minute)
	r.d.cards.sweep(ctx)
	if holds := r.holdPods(); len(holds) != 1 {
		t.Fatalf("hold pods while the card is in use by a check = %v", holds)
	}
	release()
	release2, why := r.d.HoldCardForCheck(ctx, "GPU-a")
	if why != "" {
		t.Fatalf("a second hold = %q", why)
	}
	release2()
	r.d.cards.sweep(ctx)
	if holds := r.holdPods(); len(holds) != 0 {
		t.Fatalf("the idle card was not returned after the check = %v", holds)
	}
	// Now nothing holds it.
	if rel, why := r.d.HoldCardForCheck(ctx, "GPU-a"); rel != nil || why == "" {
		t.Fatalf("HoldCardForCheck(returned card) = %v, %q", rel != nil, why)
	}
}

// A card an instance runs on is held; so is one the platform keeps for good.
func TestHoldForCheckAdmitsACardAnInstanceUses(t *testing.T) {
	r := newRig(t, 2)
	ctx := context.Background()
	if _, err := r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 1)); err != nil {
		t.Fatal(err)
	}
	if rel, why := r.d.HoldCardForCheck(ctx, "GPU-b"); rel == nil || why != "" {
		t.Fatalf("card used by an instance = %q", why)
	}
	// An instance that is still being created has not been given its card.
	if _, _, err := r.d.cards.claim(ctx, "gpuworks-cccccccccccc", []int{0}); err != nil {
		t.Fatal(err)
	}
	if rel, why := r.d.HoldCardForCheck(ctx, "GPU-a"); rel != nil || why == "" {
		t.Fatalf("a card that is only claimed, not yet borrowed = %v, %q", rel != nil, why)
	}
}

// Without a device plugin no card is Kubernetes's: they are all the
// platform's. With one that shares cards, none is.
func TestHoldForCheckWithoutAPluginAndWithSharedCards(t *testing.T) {
	ctx := context.Background()
	plain := newRig(t, 0)
	plain.r0Cards()
	if rel, why := plain.d.HoldCardForCheck(ctx, "GPU-a"); rel == nil || why != "" {
		t.Fatalf("no device plugin: %q", why)
	}
	rel, _ := plain.d.HoldCardForCheck(ctx, "GPU-a")
	rel()

	shared := newRig(t, 2)
	shared.srv.SetAllocatable(testNode, "nvidia.com/gpu", "8")
	if rel, why := shared.d.HoldCardForCheck(ctx, "GPU-a"); rel != nil || !strings.Contains(why, "whole cards only") {
		t.Fatalf("time-sliced cards = %v, %q", rel != nil, why)
	}
}

// r0Cards gives the no-plugin rig two cards, as a node with cards and no
// device plugin has.
func (r *rig) r0Cards() {
	r.d.cards.cards = func(context.Context) (map[int]string, error) { return map[int]string{0: "GPU-a", 1: "GPU-b"}, nil }
}
