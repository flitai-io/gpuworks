package controller

import (
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPreparationNodesFollowTheDeploymentVersion(t *testing.T) {
	c := New(nil, nil, nil)
	dep := &store.Deployment{ID: 7}
	dep.UpdatedAt = time.Unix(100, 0)
	c.placements.set(dep, []uint{3, 4})
	got := c.Placements().Nodes(dep)
	if !reflect.DeepEqual(got, []uint{3, 4}) {
		t.Fatalf("nodes = %v", got)
	}
	got[0] = 99
	if c.Placements().Nodes(dep)[0] != 3 {
		t.Fatal("reader mutated the placement")
	}
	edited := *dep
	edited.UpdatedAt = dep.UpdatedAt.Add(time.Second)
	if c.Placements().Nodes(&edited) != nil {
		t.Fatal("an edited deployment shows the placement of its old version")
	}
	// Placement does not lapse with time: nothing has to poll to keep it.
	if c.Placements().Nodes(dep) == nil {
		t.Fatal("placement lapsed")
	}
	c.forgetDeployment(dep.ID, false)
	if c.Placements().Nodes(dep) != nil {
		t.Fatal("a forgotten deployment keeps its placement")
	}
	c.placements.set(dep, []uint{5})
	c.placements.set(dep, nil)
	if c.Placements().Nodes(dep) != nil {
		t.Fatal("an empty pass did not clear the placement")
	}
}

// Every pass sets its choice again; the generation readers compare moves
// only when what Placements().Nodes answers changes, so a snapshot is not
// rebuilt for a reconcile that chose the same nodes.
func TestPreparationGenerationCountsOnlyChangedChoices(t *testing.T) {
	c := New(nil, nil, nil)
	dep := &store.Deployment{ID: 7}
	dep.UpdatedAt = time.Unix(100, 0)
	step := func(what string, change func(), moves bool) {
		t.Helper()
		before := c.Placements().Generation()
		change()
		if got := c.Placements().Generation() != before; got != moves {
			t.Fatalf("%s: generation moved %v, want %v", what, got, moves)
		}
	}
	step("nothing chosen, nothing chosen again", func() { c.placements.set(dep, nil) }, false)
	step("a first choice", func() { c.placements.set(dep, []uint{3}) }, true)
	step("the same choice", func() { c.placements.set(dep, []uint{3}) }, false)
	step("a further node", func() { c.placements.set(dep, []uint{3, 4}) }, true)
	step("the same nodes again", func() { c.placements.set(dep, []uint{3, 4}) }, false)
	step("another node", func() { c.placements.set(dep, []uint{5, 4}) }, true)
	edited := *dep
	edited.UpdatedAt = dep.UpdatedAt.Add(time.Second)
	// The edit hid the old entry; the same nodes under the new version show
	// again.
	step("the same nodes for an edited deployment", func() { c.placements.set(&edited, []uint{5, 4}) }, true)
	step("an empty pass", func() { c.placements.set(&edited, nil) }, true)
	step("an empty pass again", func() { c.placements.set(&edited, nil) }, false)
	c.placements.set(&edited, []uint{6})
	step("forgetting the deployment", func() { c.forgetDeployment(dep.ID, false) }, true)
	step("forgetting it again", func() { c.forgetDeployment(dep.ID, false) }, false)
}
