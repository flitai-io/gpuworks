package controller

import (
	"encoding/json"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// restartController is the server starting again over the same store and
// hub: the controller's memory is gone, its settings and clock as before.
func (f *failoverFixture) restartController() {
	f.c = New(f.st, f.hub, nil)
	f.c.now = func() time.Time { return f.at }
	f.c.SetRetryPolicy(0, 3)
	f.c.SetOfflineGrace(time.Minute)
}

// movedOff loses the multi-node deployment's rank 1 node (nodes[1]) long
// enough to be replaced, and returns once the rank sits on nodes[2].
func movedOff(t *testing.T, f *failoverFixture, name string) *store.Deployment {
	t.Helper()
	dep := f.multiNode(t, name)
	f.reconcile(t, dep.ID)
	f.reconcile(t, dep.ID)
	f.lose(t, f.nodes[1], 11*time.Minute)
	f.reconcile(t, dep.ID)
	f.reconcile(t, dep.ID)
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[2].UUID {
		t.Fatalf("not moved: %v", got)
	}
	return dep
}

// A server restart between the move and the lost node's return keeps the
// chance to undo the move: the lost node takes its rank back before the moved
// unit ran on its new node, as without the restart
// (TestAReturningNodeTakesItsRankBackOnlyBeforeTheMoveRan).
func TestARestartKeepsTheChanceToUndoAMoveThatHasNotRun(t *testing.T) {
	f := newFailoverFixture(t, "restore-revert", 3)
	dep := movedOff(t, f, "restore-revert")
	f.restartController()
	f.restore(t, f.nodes[1])
	f.reconcile(t, dep.ID)
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[1].UUID {
		t.Fatalf("after a restart the returning node did not take its rank back: %v", got)
	}
	if events := eventCodes(t, f.st, dep.ID, EventNodeReplaceReverted); len(events) != 1 {
		t.Fatalf("NODE_REPLACE_REVERTED events = %v", events)
	}
	// Read back once: the moves are not read again on later passes.
	f.reconcile(t, dep.ID)
	if events := eventCodes(t, f.st, dep.ID, EventNodeReplaceReverted); len(events) != 1 {
		t.Fatalf("NODE_REPLACE_REVERTED events after another pass = %v", events)
	}
}

// A move whose unit ran on its new node is not read back: the record in
// memory is dropped once the unit runs there, and a unit that ran must not be
// moved back after a restart either.
func TestARestartDoesNotUndoAMoveThatRan(t *testing.T) {
	f := newFailoverFixture(t, "restore-ran", 3)
	dep := movedOff(t, f, "restore-ran")
	f.settle(t, dep.ID, 8)
	insts, _ := f.st.InstancesForDeployment(dep.ID)
	ran := 0
	for _, inst := range insts {
		if inst.State != proto.InstStopped && inst.State != proto.InstFailed && (inst.NodeID == f.nodes[2].ID || inst.NodeID == f.nodes[0].ID) {
			if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning}); err != nil {
				t.Fatal(err)
			}
			ran++
		}
	}
	if ran == 0 {
		t.Fatal("no instance of the moved unit to run")
	}
	// The moved unit then stops - every rank of it, so no running unit is
	// what keeps it where it is: having run on its new node, it must still
	// not be moved back.
	insts, _ = f.st.InstancesForDeployment(dep.ID)
	for _, inst := range insts {
		if inst.State == proto.InstRunning {
			if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstFailed}); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.restartController()
	f.restore(t, f.nodes[1])
	// Passes enough for the stopped unit's retry to reach the revert check.
	f.settle(t, dep.ID, 8)
	if !f.c.failover.restored[dep.ID] {
		t.Fatal("the restarted controller never looked at the deployment's moves")
	}
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[2].UUID {
		t.Fatalf("a move that ran was undone after a restart: %v", got)
	}
	if events := eventCodes(t, f.st, dep.ID, EventNodeReplaceReverted); len(events) != 0 {
		t.Fatalf("NODE_REPLACE_REVERTED events = %v", events)
	}
}

// A move the events say was undone already is not read back, and neither is
// one whose rank the placement no longer has on the node it moved to.
func TestARestartReadsBackOnlyMovesStillStanding(t *testing.T) {
	f := newFailoverFixture(t, "restore-settled", 3)
	dep := movedOff(t, f, "restore-settled")
	// The event log says it was undone (as a revert writes it), though the
	// placement still has the rank on nodes[2].
	f.st.AddDeploymentEventCode(dep.ID, nodeReplaceEventKind, EventNodeReplaceReverted, map[string]any{
		"unit": 1, "rank": 1, store.EventParamNodeUUIDs: []string{f.nodes[2].UUID, f.nodes[1].UUID},
	})
	f.restartController()
	f.restore(t, f.nodes[1])
	f.reconcile(t, dep.ID)
	if !f.c.failover.restored[dep.ID] {
		t.Fatal("the restarted controller never looked at the deployment's moves")
	}
	if got := failoverUnits(t, f.st, dep.ID); got[0][1] != f.nodes[2].UUID {
		t.Fatalf("a move the events say was undone was read back: %v", got)
	}

	// Another deployment moved off nodes[1]; while the controller is down an
	// operator places the rank by hand on nodes[1], which then comes back:
	// the move is history, as revertFailovers treats an edited unit, and
	// nothing is undone. Two guards say so here: the rank is no longer on the
	// node it moved to, and that node is no longer one of the deployment's.
	g := newFailoverFixture(t, "restore-edited", 3)
	edited := movedOff(t, g, "restore-edited")
	ids, _ := json.Marshal([]uint{g.nodes[0].ID, g.nodes[1].ID})
	placement := `{"tp":2,"units":[{"node_uuid":"` + g.nodes[0].UUID + `","followers":[{"node_uuid":"` + g.nodes[1].UUID + `"}]}]}`
	if err := g.st.DB().Model(&store.Deployment{}).Where("id = ?", edited.ID).
		Updates(map[string]any{"placement": placement, "node_ids": string(ids)}).Error; err != nil {
		t.Fatal(err)
	}
	if got := failoverUnits(t, g.st, edited.ID); got[0][1] != g.nodes[1].UUID {
		t.Fatalf("the operator's edit did not take: %v", got)
	}
	g.restartController()
	g.restore(t, g.nodes[1]) // back online: the edited rank is not moved again
	g.settle(t, edited.ID, 4)
	if !g.c.failover.restored[edited.ID] {
		t.Fatal("the restarted controller never looked at the deployment's moves")
	}
	if moves := g.c.failover.get(edited.ID); len(moves) != 0 {
		t.Fatalf("a move whose rank was edited away was read back: %v", moves)
	}
	if events := eventCodes(t, g.st, edited.ID, EventNodeReplaceReverted); len(events) != 0 {
		t.Fatalf("NODE_REPLACE_REVERTED events after an operator's edit = %v", events)
	}
}
