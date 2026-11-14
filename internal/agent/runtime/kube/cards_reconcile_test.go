package kube

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// deleteHoldPod removes a hold pod directly through the API, as an
// operator's own kubectl delete would, bypassing cardPool entirely (its
// deletePods/returning bookkeeping never runs).
func deleteHoldPod(t *testing.T, r *rig, name string) {
	t.Helper()
	zero := int64(0)
	if err := r.srv.Client().Delete(context.Background(), kubeapi.PodPath(testNS, name), &zero); err != nil {
		t.Fatal(err)
	}
}

// Deleting the hold pod behind the agent's back must not leave the card
// "held" forever: the next acquire of that card notices it is gone and
// borrows it again, with no double-borrow (still exactly one hold pod for
// it afterward).
func TestClaimRecoversAfterAnOperatorDeletesTheHoldPodBehindItsBack(t *testing.T) {
	r := newRig(t, 2)
	ctx := context.Background()

	got, err := r.d.cards.acquire(ctx, "inst-1", []int{0})
	if err != nil || len(got) != 1 || got[0] != "GPU-a" {
		t.Fatalf("acquire(inst-1) = %v, %v", got, err)
	}
	before := r.holdPods()
	if len(before) != 1 {
		t.Fatalf("hold pods after the first acquire = %v, want exactly one", before)
	}
	r.d.cards.release("inst-1")

	// An operator deletes the hold pod directly; cardPool's own bookkeeping
	// (deletePods/returning) never ran, so without reconcileHeld p.held
	// would still say GPU-a is borrowed.
	deleteHoldPod(t, r, before[0])
	if holds := r.holdPods(); len(holds) != 0 {
		t.Fatalf("hold pods right after the manual delete = %v, want none", holds)
	}

	got, err = r.d.cards.acquire(ctx, "inst-2", []int{0})
	if err != nil || len(got) != 1 || got[0] != "GPU-a" {
		t.Fatalf("acquire(inst-2) after the hold pod was deleted = %v, %v", got, err)
	}
	after := r.holdPods()
	if len(after) != 1 {
		t.Fatalf("hold pods after the recovering acquire = %v, want exactly one (no double-borrow)", after)
	}
	if after[0] == before[0] {
		t.Fatalf("the recovering acquire reused the deleted pod's name %q, which no longer exists", before[0])
	}
}

// A card whose hold pod is still there is left alone: reconcileHeld must
// not churn hold pods that were never touched.
func TestClaimLeavesAnIntactHoldPodAlone(t *testing.T) {
	r := newRig(t, 2)
	ctx := context.Background()

	if _, err := r.d.cards.acquire(ctx, "inst-1", []int{0}); err != nil {
		t.Fatal(err)
	}
	before := r.holdPods()
	if len(before) != 1 {
		t.Fatalf("hold pods after acquire = %v", before)
	}

	// A second claim of the same card, with its hold pod untouched, must
	// not recreate anything.
	if _, _, err := r.d.cards.claim(ctx, "inst-2", []int{0}); err != nil {
		t.Fatal(err)
	}
	after := r.holdPods()
	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("an intact hold pod changed: before %v, after %v", before, after)
	}
}

// reconcileHeld's own API failure (the hold pods list call itself erroring)
// must not fail the claim: the stale belief is kept rather than guessed at,
// exactly as claim behaved before this reconciliation existed.
func TestClaimToleratesAReconcileHeldListError(t *testing.T) {
	r := newRig(t, 1)
	ctx := context.Background()
	if _, err := r.d.cards.acquire(ctx, "inst-1", []int{0}); err != nil {
		t.Fatal(err)
	}
	r.srv.FailNext["GET /api/v1/namespaces/"+testNS+"/pods"] = 500

	got, _, err := r.d.cards.claim(ctx, "inst-2", []int{0})
	if err != nil || len(got) != 1 || got[0] != "GPU-a" {
		t.Fatalf("claim during a hold-pod list failure = %v, %v", got, err)
	}
}
