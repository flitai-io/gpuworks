package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// newSwitchRig is a node with a plain device plugin that holds card GPU-a for
// a running instance, with a scheduler hook ready to take over the node.
func newSwitchRig(t *testing.T) (*rig, *fakeSched, string) {
	t.Helper()
	r := newRig(t, 2)
	s := installSched(r)
	r.d.cards.podsTTL, r.d.cards.scheduleWait = 0, 300*time.Millisecond
	if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatal(err)
	}
	holds := r.holdPods()
	if len(holds) != 1 {
		t.Fatalf("hold pods = %v", holds)
	}
	return r, s, holds[0]
}

// backToPlainPlugin undoes installACK / installHAMi.
func backToPlainPlugin(r *rig, s *fakeSched) {
	s.ack.on, s.hami.on = false, false
	r.srv.UpdateNode(testNode, func(n *kubeapi.Node) {
		delete(n.Metadata.Labels, ackScheduleLabel)
		delete(n.Metadata.Labels, ackMemLabel)
		delete(n.Metadata.Annotations, "hami.io/node-nvidia-register")
		delete(n.Status.Capacity, resACKMem)
		delete(n.Status.Allocatable, resACKMem)
		delete(n.Status.Capacity, resACKCount)
		delete(n.Status.Allocatable, resACKCount)
	})
	r.srv.SetCapacity(testNode, "nvidia.com/gpu", "2", "2")
}

// The cluster switches the node to ACK GPU sharing while the platform holds a
// card with a plain plugin's hold pod: ACK's scheduler has never heard of
// nvidia.com/gpu and may give the card to anyone. The platform must not go on
// as if the card were its own: it says so, and borrows nothing in the old
// mode.
func TestSwitchToACKWhileHoldingReportsTheCardAndStopsBorrowing(t *testing.T) {
	r, s, hold := newSwitchRig(t)
	ctx := context.Background()
	installACK(r, s)
	created := len(s.holdCreated())

	holders, err := r.d.ExternalCardHolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders["GPU-b"]) != 0 || len(holders["GPU-a"]) != 1 {
		t.Fatalf("holders = %+v; want only the card held for the old mode", holders)
	}
	reason := holders["GPU-a"][0].Name
	for _, want := range []string{"the device plugin (nvidia.com/gpu)", "ACK GPU sharing (aliyun.com/gpu-mem)", "may give the card to another pod", hold} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q does not say %q", reason, want)
		}
	}
	// No second instance on the card, and the pool did not take it up.
	_, err = r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 0))
	if err == nil || !strings.Contains(err.Error(), "card GPU-a cannot be used") || !strings.Contains(err.Error(), "ACK GPU sharing") {
		t.Fatalf("Run on the stale card = %v", err)
	}
	if rel, why := r.d.HoldCardForCheck(ctx, "GPU-a"); rel != nil || !strings.Contains(why, "ACK GPU sharing") {
		t.Fatalf("HoldCardForCheck on the stale card = %v, %q", rel != nil, why)
	}
	if users := r.d.cards.users["GPU-a"]; len(users) != 1 || !users["gpuworks-aaaaaaaaaaaa"] {
		t.Fatalf("users of the stale card = %v", users)
	}
	if got := len(s.holdCreated()); got != created {
		t.Fatalf("hold pods created after the switch: %d, want none (the refused instance asked for one)", got-created)
	}
	if len(r.holdPods()) != 1 {
		t.Fatalf("hold pods = %v; the old hold pod stays while an instance runs on its card", r.holdPods())
	}
	// A sweep does not return it from under the running instance.
	r.now = r.now.Add(time.Hour)
	r.d.cards.sweep(ctx)
	if len(r.holdPods()) != 1 {
		t.Fatalf("a sweep returned a card an instance runs on: %v", r.holdPods())
	}
	// When the instance is gone the old hold pod is of no use and goes; the
	// card is then an ordinary card of the new mode.
	if err := r.d.Remove(ctx, "gpuworks-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	r.d.cards.sweep(ctx)
	if len(r.holdPods()) != 0 {
		t.Fatalf("the old hold pod outlived its instance: %v", r.holdPods())
	}
	if holders, _ := r.d.ExternalCardHolders(ctx); len(holders) != 0 {
		t.Fatalf("holders after the cleanup = %+v", holders)
	}
	if _, err := r.d.Run(ctx, spec("gpuworks-cccccccccccc", 0)); err != nil {
		t.Fatalf("Run on the card in the new mode = %v", err)
	}
}

// The same when the node is switched to HAMi: its scheduler does not know the
// plugin's hold pod either. Cards that are not held are borrowed through HAMi
// meanwhile.
func TestSwitchToHAMiWhileHoldingReportsTheCard(t *testing.T) {
	r, s, _ := newSwitchRig(t)
	ctx := context.Background()
	installHAMi(t, r, s, DefaultHAMiScheduler)

	holders, err := r.d.ExternalCardHolders(ctx)
	if err != nil || len(holders["GPU-a"]) != 1 || !strings.Contains(holders["GPU-a"][0].Name, "HAMi (nvidia.com/gpu)") {
		t.Fatalf("holders = %+v, %v", holders, err)
	}
	if _, err := r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 0)); err == nil || !strings.Contains(err.Error(), "HAMi") {
		t.Fatalf("Run on the stale card = %v", err)
	}
	if _, err := r.d.Run(ctx, spec("gpuworks-cccccccccccc", 1)); err != nil {
		t.Fatalf("Run on the other card through HAMi = %v", err)
	}
	created := s.holdCreated()
	if last := created[len(created)-1]; last.Spec.SchedulerName != "hami-scheduler" {
		t.Fatalf("the new hold pod = %+v", last.Spec)
	}
	if holders, _ := r.d.ExternalCardHolders(ctx); len(holders["GPU-b"]) != 0 || len(holders["GPU-a"]) != 1 {
		t.Fatalf("holders after borrowing GPU-b = %+v", holders)
	}
}

// A node that goes back to the mode its hold pods were made for takes its
// cards back: nothing was lost meanwhile.
func TestSwitchBackRestoresTheHeldCard(t *testing.T) {
	r, s, hold := newSwitchRig(t)
	ctx := context.Background()
	installACK(r, s)
	if holders, _ := r.d.ExternalCardHolders(ctx); len(holders["GPU-a"]) != 1 {
		t.Fatalf("holders on ACK = %+v", holders)
	}
	backToPlainPlugin(r, s)
	if holders, err := r.d.ExternalCardHolders(ctx); err != nil || len(holders) != 0 {
		t.Fatalf("holders after the switch back = %+v, %v", holders, err)
	}
	if got := r.d.cards.held["GPU-a"]; got != hold {
		t.Fatalf("held[GPU-a] = %q, want the original hold pod %q", got, hold)
	}
	created := len(s.holdCreated())
	if _, err := r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 0)); err != nil {
		t.Fatalf("Run on the restored card = %v", err)
	}
	if len(s.holdCreated()) != created {
		t.Fatal("the restored card was borrowed again")
	}
}

// An agent that restarts after the switch finds the old hold pods and does not
// take their cards for its own.
func TestSwitchIsNoticedByAnAgentThatRestartedAfterIt(t *testing.T) {
	r, s, _ := newSwitchRig(t)
	installACK(r, s)
	d2 := New(r.d.o)
	d2.cards.podsTTL = 0
	ctx := context.Background()
	if ok, err := d2.Detect(ctx); !ok || err != nil {
		t.Fatal(err)
	}
	holders, err := d2.ExternalCardHolders(ctx)
	if err != nil || len(holders["GPU-a"]) != 1 || !strings.Contains(holders["GPU-a"][0].Name, "ACK GPU sharing") {
		t.Fatalf("holders after the restart = %+v, %v", holders, err)
	}
	if len(d2.cards.held) != 0 {
		t.Fatalf("the restarted agent took the old hold pods as its own: %v", d2.cards.held)
	}
	if _, err := d2.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 0)); err == nil {
		t.Fatal("the restarted agent started an instance on the stale card")
	}
}

// A node that turns to a mode the platform cannot borrow in reports every
// card with that mode's reason, and gives the old hold pods back once no
// instance uses their cards.
func TestSwitchToFractionalAllocationReportsEveryCard(t *testing.T) {
	r, _, _ := newSwitchRig(t)
	ctx := context.Background()
	r.srv.SetAllocatable(testNode, "nvidia.com/mig-1g.10gb", "14")
	holders, err := r.d.ExternalCardHolders(ctx)
	if err != nil || len(holders) != 2 || !strings.Contains(holders["GPU-a"][0].Name, "MIG partitions") {
		t.Fatalf("holders = %+v, %v", holders, err)
	}
	if err := r.d.Remove(ctx, "gpuworks-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	r.d.cards.sweep(ctx)
	if len(r.holdPods()) != 0 {
		t.Fatalf("the old hold pod outlived its instance: %v", r.holdPods())
	}
}

// ACK's scheduler counts a card to whoever it gives it to: a borrow in the
// new mode that is given the stale card makes it the platform's, in ACK's
// books, and the card stops being reported.
func TestACKBorrowAfterASwitchTakesTheStaleCardIntoTheSchedulersBooks(t *testing.T) {
	r, s, _ := newSwitchRig(t)
	ctx := context.Background()
	installACK(r, s)
	if _, err := r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 1)); err != nil {
		t.Fatalf("Run on the other card = %v", err)
	}
	if holders, _ := r.d.ExternalCardHolders(ctx); len(holders) != 0 {
		t.Fatalf("holders = %+v; ACK counts both cards to the platform's hold pods now", holders)
	}
	if len(r.d.cards.held) != 2 {
		t.Fatalf("held = %v", r.d.cards.held)
	}
	// And the card is still not returned to the old mode's books from under
	// the instance: the old hold pod goes only when the instance does.
	pods := 0
	for _, h := range r.holdPods() {
		if _, ok := r.srv.Pod(testNS, h).Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; ok {
			pods++
		}
	}
	if pods != 1 {
		t.Fatalf("old-mode hold pods = %d", pods)
	}
}
