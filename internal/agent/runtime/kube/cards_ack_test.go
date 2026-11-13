package kube

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// newACKRig is a node of gpus 44 GiB cards under ACK GPU sharing (cgpu): the
// node counts aliyun.com/gpu-mem and the nvidia.com/gpu plugin gives nothing.
func newACKRig(t *testing.T, gpus int) (*rig, *fakeSched) {
	t.Helper()
	r := newRig(t, gpus)
	r.kubelet.devices = nil
	s := installSched(r)
	installACK(r, s)
	r.d.cards.podsTTL, r.d.cards.scheduleWait = 0, 300*time.Millisecond
	return r, s
}

func ackExternal(t *testing.T, r *rig, name, gib string) {
	t.Helper()
	r.externalPod(t, "ml", name, "", nil, map[string]string{"aliyun.com/gpu-mem": gib})
}

// The hold pod asks the default scheduler for a whole card's memory; which
// card it got is read from the scheduler's annotation and the unwanted ones
// are given back, as with a plain device plugin that cannot be told which.
func TestACKBorrowReadsWhichCardTheSchedulerGaveAndReturnsTheRest(t *testing.T) {
	r, s := newACKRig(t, 2)
	if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 1)); err != nil {
		t.Fatal(err)
	}
	holds := s.holdCreated()
	if len(holds) != 2 {
		t.Fatalf("hold pods created = %d, want one per free card", len(holds))
	}
	for _, h := range holds {
		res := h.Spec.Containers[0].Resources
		if h.Spec.NodeName != "" || h.Spec.SchedulerName != "" || !maps.Equal(h.Spec.NodeSelector, map[string]string{"kubernetes.io/hostname": testNode}) ||
			!maps.Equal(res.Limits, map[string]string{"aliyun.com/gpu-mem": "44"}) {
			t.Fatalf("hold pod = nodeName %q scheduler %q selector %v limits %v", h.Spec.NodeName, h.Spec.SchedulerName, h.Spec.NodeSelector, res.Limits)
		}
	}
	remaining := r.holdPods()
	if len(remaining) != 1 || !reflect.DeepEqual(r.d.cards.held, map[string]string{"GPU-b": remaining[0]}) {
		t.Fatalf("hold pods %v, held %v; want the pod that got card 1 only", remaining, r.d.cards.held)
	}
	pod := r.srv.Pod(testNS, remaining[0])
	if got := pod.Metadata.Annotations["scheduler.framework.gpushare.allocation"]; got != `{"0":{"1":44}}` {
		t.Fatalf("allocation = %q", got)
	}
	allocs, _ := r.kubelet.List(context.Background())
	if len(allocs) != 44 || allocs[0].Resource != "aliyun.com/gpu-mem" || !strings.HasPrefix(allocs[0].DeviceID, "gpu1_mem_") {
		t.Fatalf("kubelet allocations = %d, first %+v", len(allocs), allocs[0])
	}
	// A restarted agent reads the same ownership from the annotation.
	d2 := New(r.d.o)
	if ok, err := d2.Detect(context.Background()); !ok || err != nil {
		t.Fatal(err)
	}
	if err := d2.cards.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d2.cards.held, map[string]string{"GPU-b": remaining[0]}) {
		t.Fatalf("recovered held = %v", d2.cards.held)
	}
}

func TestACKBorrowOfEveryCard(t *testing.T) {
	r, _ := newACKRig(t, 2)
	ctx := context.Background()
	if _, err := r.d.Run(ctx, spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatal(err)
	}
	// Card 0 is held; the next borrow finds one free card and the scheduler
	// gives it.
	if _, err := r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 1)); err != nil {
		t.Fatal(err)
	}
	if len(r.d.cards.held) != 2 || len(r.holdPods()) != 2 {
		t.Fatalf("held %v, hold pods %v", r.d.cards.held, r.holdPods())
	}
}

// A card another pod uses part of cannot be given whole. The scheduler gives
// the other card instead, which is returned, and the error names the pod.
func TestACKBorrowNamesThePodsHoldingTheWantedCard(t *testing.T) {
	r, _ := newACKRig(t, 2)
	r.d.cards.borrowRetry = time.Minute
	ackExternal(t, r, "small", "12") // lowest-numbered card with room: card 0
	if got := r.srv.Pod("ml", "small").Metadata.Annotations["scheduler.framework.gpushare.allocation"]; got != `{"0":{"0":12}}` {
		t.Fatalf("the fake's allocation = %q", got)
	}
	started := time.Now()
	_, err := r.d.Run(context.Background(), spec("gpuworks-cccccccccccc", 0))
	if err == nil || !strings.Contains(err.Error(), "card GPU-a") || !strings.Contains(err.Error(), "Pod ml/small (12 GiB)") || time.Since(started) > 5*time.Second {
		t.Fatalf("Run = %v after %s", err, time.Since(started))
	}
	if len(r.holdPods()) != 0 || len(r.d.cards.users) != 0 || len(r.d.cards.held) != 0 {
		t.Fatalf("the unwanted card's hold pod or a user was left: %v %v %v", r.holdPods(), r.d.cards.users, r.d.cards.held)
	}
	// The card nobody uses is borrowable.
	if _, err := r.d.Run(context.Background(), spec("gpuworks-dddddddddddd", 1)); err != nil {
		t.Fatalf("Run on the free card = %v", err)
	}
	holders, err := r.d.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h := holders["GPU-a"]; len(h) != 1 || h[0].Name != "Pod ml/small (12 GiB)" || h[0].MemoryBytes != 12<<30 {
		t.Fatalf("GPU-a holders = %+v", h)
	}
	if len(holders["GPU-b"]) != 0 {
		t.Fatalf("the platform's own hold pod is an outsider: %+v", holders["GPU-b"])
	}
}

func TestACKBorrowWithNoCardFreeNamesTheHolders(t *testing.T) {
	r, _ := newACKRig(t, 2)
	ackExternal(t, r, "one", "10")
	ackExternal(t, r, "two", "44") // card 0 has 34 free: card 1
	_, err := r.d.Run(context.Background(), spec("gpuworks-eeeeeeeeeeee", 1))
	if err == nil || !strings.Contains(err.Error(), "Pod ml/two (44 GiB)") {
		t.Fatalf("Run = %v", err)
	}
	if got := len(r.srv.Pods()); got != 3 { // the agent, and the two outsiders: no hold pod was even tried
		t.Fatalf("pods = %d", got)
	}
}

// The scheduler says no (its books have a pod this agent cannot see): the
// reason is the scheduler's own; a shortage that is gone on the next try is
// borrowed on then.
func TestACKBorrowReportsTheSchedulersReasonAndRetries(t *testing.T) {
	r, s := newACKRig(t, 1)
	r.d.cards.borrowRetry = 5 * time.Second
	s.ack.failNext = 1
	if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatalf("Run with one refusal = %v", err)
	}
	r2, s2 := newACKRig(t, 1)
	s2.ack.failNext = 1000
	_, err := r2.d.Run(context.Background(), spec("gpuworks-bbbbbbbbbbbb", 0))
	if err == nil || !strings.Contains(err.Error(), "Insufficient aliyun.com/gpu-mem") || !strings.Contains(err.Error(), "the scheduler could not place the hold pod") {
		t.Fatalf("Run with a scheduler that refuses = %v", err)
	}
	if len(r2.holdPods()) != 0 {
		t.Fatalf("Pending hold pods left: %v", r2.holdPods())
	}
}

func TestACKKeepAllBorrowsEveryCard(t *testing.T) {
	r, _ := newACKRig(t, 3)
	r.d.SetIdleReturn(-1)
	if err := r.d.cards.keepAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.holdPods()) != 3 || len(r.d.cards.held) != 3 {
		t.Fatalf("hold pods %v held %v", r.holdPods(), r.d.cards.held)
	}
}

func TestACKIdleCardIsReturnedAndWaitedFor(t *testing.T) {
	r, _ := newACKRig(t, 2)
	r.srv.TerminationDelay = 60 * time.Millisecond
	r.d.cards.borrowRetry = 5 * time.Second // the scheduler counts the returned pod until it is gone
	ctx := context.Background()
	r.d.SetIdleReturn(time.Minute)
	if _, err := r.d.Run(ctx, spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatal(err)
	}
	// The unwanted card's pod was waited for: nothing is left Terminating.
	for _, p := range r.srv.Pods() {
		if p.Metadata.DeletionTimestamp != nil {
			t.Fatalf("pod %s is still terminating after the borrow", p.Metadata.Name)
		}
	}
	if err := r.d.Remove(ctx, "gpuworks-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(2 * time.Minute)
	r.d.cards.sweep(ctx)
	if _, err := r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 0)); err != nil {
		t.Fatalf("Run right after the card was returned = %v", err)
	}
}

// What ACK counts to a card whole is the card; anything less is not.
func TestACKHoldCardMustBeAWholeCard(t *testing.T) {
	a := &ackNode{cardGiB: 44, cards: 2}
	cards := map[int]string{0: "GPU-a", 1: "GPU-b"}
	pod := func(alloc string) kubeapi.Pod {
		return kubeapi.Pod{Metadata: kubeapi.ObjectMeta{Annotations: map[string]string{ackAllocationAnnotation: alloc}}}
	}
	if card, err := a.holdCard(pod(`{"0":{"1":44}}`), cards); err != nil || card != "GPU-b" {
		t.Fatalf("whole card = %q, %v", card, err)
	}
	for name, tc := range map[string]struct{ alloc, want string }{
		"part of a card": {`{"0":{"1":10}}`, "only 10 of card 1's 44 GiB"},
		"two cards":      {`{"0":{"0":44,"1":44}}`, "2 cards instead of one"},
		"unknown card":   {`{"0":{"5":44}}`, "card 5, which the node's probe does not know"},
		"not a number":   {`{"0":{"x":44}}`, "is not a number"},
		"not json":       {`oops`, ackAllocationAnnotation},
		"none":           {``, "0 cards instead of one"},
	} {
		if _, err := a.holdCard(pod(tc.alloc), cards); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: holdCard = %v, want %q", name, err, tc.want)
		}
	}
}

// A service account that cannot list pods cluster-wide still sees the
// kubelet's allocations of the sharing pods, by name.
func TestACKExternalHoldersWithoutPodListPermission(t *testing.T) {
	r, _ := newACKRig(t, 2)
	ackExternal(t, r, "small", "12")
	r.srv.DenyClusterPods = true
	holders, err := r.d.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h := holders["GPU-a"]; len(h) != 1 || h[0].Name != "Pod ml/small" {
		t.Fatalf("GPU-a holders = %+v", h)
	}
}
