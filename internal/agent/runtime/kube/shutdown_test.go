package kube

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// Still selected (the common case: an ordinary restart, rolling update or
// in-place upgrade): nothing is touched.
func TestReleaseCardsIfNodeUnselectedKeepsHoldPodsWhenStillSelected(t *testing.T) {
	r := newRig(t, 1)
	ctx := context.Background()
	if _, err := r.d.cards.acquire(ctx, "inst-1", []int{0}); err != nil {
		t.Fatal(err)
	}
	before := r.holdPods()
	if len(before) != 1 {
		t.Fatalf("hold pods after acquire = %v", before)
	}

	r.d.ReleaseCardsIfNodeUnselected(ctx)

	if after := r.holdPods(); len(after) != 1 || after[0] != before[0] {
		t.Fatalf("a still-selected node's hold pods changed: before %v, after %v", before, after)
	}
}

// No longer selected by a plain nodeSelector: every hold pod of this node
// is deleted, returning the cards to Kubernetes.
func TestReleaseCardsIfNodeUnselectedDeletesHoldPodsWhenDroppedByNodeSelector(t *testing.T) {
	r := newRig(t, 2)
	ctx := context.Background()
	if _, err := r.d.cards.acquire(ctx, "inst-1", []int{0, 1}); err != nil {
		t.Fatal(err)
	}
	if len(r.holdPods()) != 2 {
		t.Fatalf("hold pods after acquire = %v", r.holdPods())
	}

	// A reapplied manifest narrowed the node selector; this node's labels
	// (empty) no longer satisfy it.
	r.srv.SetDaemonSetSelection(map[string]string{"zone": "z9"}, "", nil)

	r.d.ReleaseCardsIfNodeUnselected(ctx)

	if holds := r.holdPods(); len(holds) != 0 {
		t.Fatalf("hold pods after an unselected shutdown = %v, want none", holds)
	}
}

// No longer selected because the cloud entry's node pool narrowed: the
// same cleanup, driven by the pool nodeAffinity term instead of a plain
// nodeSelector.
func TestReleaseCardsIfNodeUnselectedDeletesHoldPodsWhenDroppedByNodePool(t *testing.T) {
	r := newRig(t, 1)
	ctx := context.Background()
	if _, err := r.d.cards.acquire(ctx, "inst-1", []int{0}); err != nil {
		t.Fatal(err)
	}
	r.srv.SetNodeLabel(testNode, "pool", "np-b")
	r.srv.SetDaemonSetSelection(nil, "pool", []string{"np-a"})

	r.d.ReleaseCardsIfNodeUnselected(ctx)

	if holds := r.holdPods(); len(holds) != 0 {
		t.Fatalf("hold pods after a node-pool drop = %v, want none", holds)
	}
}

// Still in the node pool: kept, exactly like the plain-selector case.
func TestReleaseCardsIfNodeUnselectedKeepsHoldPodsWhenStillInThePool(t *testing.T) {
	r := newRig(t, 1)
	ctx := context.Background()
	if _, err := r.d.cards.acquire(ctx, "inst-1", []int{0}); err != nil {
		t.Fatal(err)
	}
	r.srv.SetNodeLabel(testNode, "pool", "np-a")
	r.srv.SetDaemonSetSelection(nil, "pool", []string{"np-a", "np-b"})

	r.d.ReleaseCardsIfNodeUnselected(ctx)

	if holds := r.holdPods(); len(holds) != 1 {
		t.Fatalf("hold pods for a node still in its pool = %v, want one", holds)
	}
}

// An API failure reading the DaemonSet or the node is logged and never
// touches a hold pod: shutdown must not be blocked or guess at the answer.
func TestReleaseCardsIfNodeUnselectedToleratesAPIErrors(t *testing.T) {
	for _, failing := range []string{
		"GET /apis/apps/v1/namespaces/" + testNS + "/daemonsets/",
		"GET /api/v1/nodes/",
	} {
		r := newRig(t, 1)
		ctx := context.Background()
		if _, err := r.d.cards.acquire(ctx, "inst-1", []int{0}); err != nil {
			t.Fatal(err)
		}
		before := r.holdPods()
		r.srv.FailNext[failing] = 500

		r.d.ReleaseCardsIfNodeUnselected(ctx) // must not panic or hang

		if after := r.holdPods(); len(after) != len(before) || after[0] != before[0] {
			t.Fatalf("%s: hold pods changed on an API failure: before %v, after %v", failing, before, after)
		}
	}
}

// A node this build has no rig for (no hold pods ever created) is a no-op,
// not an error.
func TestReleaseCardsIfNodeUnselectedWithoutAnyHoldPods(t *testing.T) {
	r := newRig(t, 1)
	r.srv.SetDaemonSetSelection(map[string]string{"zone": "z9"}, "", nil)
	r.d.ReleaseCardsIfNodeUnselected(context.Background())
	if holds := r.holdPods(); len(holds) != 0 {
		t.Fatalf("hold pods = %v", holds)
	}
}

// daemonSetPoolTerm reads exactly the shape kubemanifest.Render writes, and
// nothing else: no affinity, or one with no matching In expression, is no
// pool restriction.
func TestDaemonSetPoolTermReadsTheRenderedShape(t *testing.T) {
	if label, ids := daemonSetPoolTerm(nil); label != "" || ids != nil {
		t.Fatalf("nil affinity = %q, %v", label, ids)
	}
	a := &kubeapi.Affinity{NodeAffinity: &kubeapi.NodeAffinity{Required: &kubeapi.NodeSelector{
		Terms: []kubeapi.NodeSelectorTerm{{MatchExpressions: []kubeapi.NodeSelectorRequirement{
			{Key: "pool", Operator: "In", Values: []string{"np-a", "np-b"}},
		}}},
	}}}
	label, ids := daemonSetPoolTerm(a)
	if label != "pool" || len(ids) != 2 || ids[0] != "np-a" || ids[1] != "np-b" {
		t.Fatalf("rendered shape = %q, %v", label, ids)
	}
	other := &kubeapi.Affinity{NodeAffinity: &kubeapi.NodeAffinity{Required: &kubeapi.NodeSelector{
		Terms: []kubeapi.NodeSelectorTerm{{MatchFields: []kubeapi.NodeSelectorRequirement{
			{Key: "metadata.name", Operator: "In", Values: []string{"node-a"}},
		}}},
	}}}
	if label, ids := daemonSetPoolTerm(other); label != "" || ids != nil {
		t.Fatalf("a matchFields-only term = %q, %v", label, ids)
	}
}
