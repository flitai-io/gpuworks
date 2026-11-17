package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/nodepolicy"
)

// The policy is written as a whole through the caller's patch, only its own
// columns change, and the change is announced on the node topic so waiting
// deployments are retried (controller routes.noteNode).
func TestSetNodePlacementPolicyWritesAndAnnounces(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RegisterNode("policy-node", HashToken("policy"), "policy"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeScheduling("policy-node", NodeCordoned); err != nil {
		t.Fatal(err)
	}
	before, _ := st.NodeByUUID("policy-node")
	if before.PlacementPolicy.ShareWithForeign {
		t.Fatal("a new node allows sharing with processes outside the platform")
	}
	sub := st.Bus.Subscribe(8, "node")
	defer sub.Close()
	old, updated, node, err := st.SetNodePlacementPolicy("policy-node", func(p nodepolicy.Policy) (nodepolicy.Policy, error) {
		p.ShareWithForeign = true
		return p, nil
	})
	if err != nil || old.ShareWithForeign || !updated.ShareWithForeign || node == nil || !node.PlacementPolicy.ShareWithForeign {
		t.Fatalf("set = old %+v new %+v node %+v err %v", old, updated, node, err)
	}
	if ev := <-sub.C(); ev.Key != "policy-node" || ev.Reason != NodeChangePlacementPolicy {
		t.Fatalf("event = %+v", ev)
	}
	again, _ := st.NodeByUUID("policy-node")
	if !again.PlacementPolicy.ShareWithForeign || again.Scheduling != NodeCordoned || again.Hostname != before.Hostname {
		t.Fatalf("stored %+v scheduling %q: the write touched more than the policy", again.PlacementPolicy, again.Scheduling)
	}
	// Switching it off again is written too: a false is not a zero to skip.
	if _, updated, _, err := st.SetNodePlacementPolicy("policy-node", func(p nodepolicy.Policy) (nodepolicy.Policy, error) {
		p.ShareWithForeign = false
		return p, nil
	}); err != nil || updated.ShareWithForeign {
		t.Fatalf("switch off = %+v %v", updated, err)
	}
	if again, _ := st.NodeByUUID("policy-node"); again.PlacementPolicy.ShareWithForeign {
		t.Fatal("switching off was not stored")
	}
}

func TestSetNodePlacementPolicyRefusals(t *testing.T) {
	st := newTestStore(t)
	if _, _, _, err := st.SetNodePlacementPolicy("missing", func(p nodepolicy.Policy) (nodepolicy.Policy, error) { return p, nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node = %v, want ErrNotFound", err)
	}
	if _, err := st.RegisterNode("policy-node", HashToken("policy"), "policy"); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	if _, _, _, err := st.SetNodePlacementPolicy("policy-node", func(p nodepolicy.Policy) (nodepolicy.Policy, error) {
		p.ShareWithForeign = true
		return p, refused
	}); !errors.Is(err, refused) {
		t.Fatalf("a refused patch = %v", err)
	}
	if node, _ := st.NodeByUUID("policy-node"); node.PlacementPolicy.ShareWithForeign {
		t.Fatal("a refused patch was stored")
	}
}
