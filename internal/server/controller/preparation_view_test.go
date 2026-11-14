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
	got := c.PreparationNodes(dep)
	if !reflect.DeepEqual(got, []uint{3, 4}) {
		t.Fatalf("nodes = %v", got)
	}
	got[0] = 99
	if c.PreparationNodes(dep)[0] != 3 {
		t.Fatal("reader mutated the placement")
	}
	edited := *dep
	edited.UpdatedAt = dep.UpdatedAt.Add(time.Second)
	if c.PreparationNodes(&edited) != nil {
		t.Fatal("an edited deployment shows the placement of its old version")
	}
	// Placement does not lapse with time: nothing has to poll to keep it.
	if c.PreparationNodes(dep) == nil {
		t.Fatal("placement lapsed")
	}
	c.forgetDeployment(dep.ID, false)
	if c.PreparationNodes(dep) != nil {
		t.Fatal("a forgotten deployment keeps its placement")
	}
	c.placements.set(dep, []uint{5})
	c.placements.set(dep, nil)
	if c.PreparationNodes(dep) != nil {
		t.Fatal("an empty pass did not clear the placement")
	}
}
