package controller

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func statesOf(insts []store.Instance) map[string]int {
	out := map[string]int{}
	for _, inst := range insts {
		out[inst.State]++
	}
	return out
}

// A rolling restart takes one replica down, then waits: while its
// replacement is not running, no other replica is drained. Once every
// replica runs a generation from after the request, the record is cleared.
func TestRollingRestartHoldsWhileAReplicaIsUnavailable(t *testing.T) {
	f := newPDFixture(t, "rolling_restart_holds", twoPrefillOneDecode)
	insts := f.reconcile(t)
	if len(insts) != 3 {
		t.Fatalf("instances = %d", len(insts))
	}
	for _, inst := range insts {
		_ = f.st.SetInstanceState(inst.UUID, proto.InstRunning, "")
		if err := f.st.DB().Model(&store.Instance{}).Where("uuid = ?", inst.UUID).Update("created_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
			t.Fatal(err)
		}
	}
	f.ctrl.SetDrainProbe(fakeDrainProbe{})
	if err := f.st.RequestRollingRestart(f.dep.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	after := f.reconcile(t)
	if got := statesOf(after); got[proto.InstDraining] != 1 || got[proto.InstRunning] != 2 {
		t.Fatalf("first pass = %v", got)
	}
	var drained string
	for _, inst := range after {
		if inst.State == proto.InstDraining {
			drained = inst.UUID
		}
	}
	// The drained replica's GPU is still held (the node's other GPU is the
	// pinned replica's), so its replacement waits; meanwhile nothing else
	// goes down.
	for i := 0; i < 3; i++ {
		if got := statesOf(f.reconcile(t)); got[proto.InstDraining] != 1 || got[proto.InstRunning] != 2 {
			t.Fatalf("pass %d took a second replica down: %v", i, got)
		}
	}
	// Drained and confirmed gone: the replacement starts, and while it is not
	// running the roll still holds.
	f.ctrl.drainClocks.startedAt(f.dep.ID, drained, time.Now().Add(-drainMax))
	if got := statesOf(f.reconcile(t)); got[proto.InstRemoving] != 1 {
		t.Fatalf("the drained replica was not retired: %v", got)
	}
	if err := f.st.DeleteInstance(drained); err != nil {
		t.Fatal(err)
	}
	after = f.reconcile(t)
	replacement := ""
	for _, inst := range after {
		if inst.State == proto.InstPending {
			replacement = inst.UUID
		}
	}
	if got := statesOf(after); replacement == "" || got[proto.InstDraining] != 0 || got[proto.InstRunning] != 2 {
		t.Fatalf("after the retire = %v", got)
	}
	if got := statesOf(f.reconcile(t)); got[proto.InstDraining] != 0 {
		t.Fatalf("the roll moved on before the replacement ran: %v", got)
	}
	// The replacement runs: the next oldest replica goes.
	_ = f.st.SetInstanceState(replacement, proto.InstRunning, "")
	if got := statesOf(f.reconcile(t)); got[proto.InstDraining] != 1 {
		t.Fatalf("the roll did not move on: %v", got)
	}
	if dep, _ := f.st.GetDeployment(f.dep.ID); dep.RestartRequestedAt == nil {
		t.Fatal("the restart ended with replicas left to replace")
	}
}

// A replica that spent its retry budget holds the roll; the timeline says so
// once per restart request, however many passes wait on it.
func TestRollingRestartStalledOnAGivenUpReplicaSaysSoOnce(t *testing.T) {
	st, err := store.Open("file:controller_roll_stalled_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	since := time.Now().UTC().Add(-time.Minute)
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: 1, NodeID: 1, ServedName: "roll", State: "active", Replicas: 2, RestartRequestedAt: &since})
	if err != nil {
		t.Fatal(err)
	}
	c := New(st, &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}}, nil)
	old := &store.Instance{UUID: "old", State: proto.InstRunning, CreatedAt: since.Add(-time.Hour)}
	match := planning.UnitMatch{
		Slots:  []planning.UnitSlot{{Key: "r0", Unit: planning.Unit{Index: 0}}, {Key: "r1", Unit: planning.Unit{Index: 1}, Inst: old}},
		Failed: map[string][]store.Instance{"r0": {{UUID: "f1"}, {UUID: "f2"}, {UUID: "f3"}}},
	}
	for pass := 0; pass < 3; pass++ {
		if err := c.rollRestart(dep, []store.Instance{*old}, match); err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.DeploymentEvents(dep.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	stalled := 0
	for _, ev := range events {
		if ev.Code == store.EventDeploymentRollingStalled {
			stalled++
		}
	}
	if stalled != 1 {
		t.Fatalf("stalled events = %d, want 1: %+v", stalled, events)
	}
	if got, _ := st.InstanceByUUID("old"); got != nil && got.State == proto.InstDraining {
		t.Fatal("the roll took the other replica down while one was given up")
	}
	c.q.close()
}

// The roll drains a replica only while it still fills it: an immediate
// restart of that replica that landed after the controller read the
// instances has already removed it, and the roll does not put it back to
// draining.
func TestRollingRestartDoesNotDrainAnInstanceRemovedMeanwhile(t *testing.T) {
	f := newPDFixture(t, "rolling_restart_stale_view", twoPrefillOneDecode)
	insts := f.reconcile(t)
	for _, inst := range insts {
		_ = f.st.SetInstanceState(inst.UUID, proto.InstRunning, "")
		if err := f.st.DB().Model(&store.Instance{}).Where("uuid = ?", inst.UUID).Update("created_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.RequestRollingRestart(f.dep.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	dep, err := f.st.GetDeployment(f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := f.st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	pd, err := planning.PDOf(dep.Placement)
	if err != nil {
		t.Fatal(err)
	}
	deployment, ok, err := f.ctrl.prepareUnits(dep, pd)
	if err != nil || !ok {
		t.Fatalf("prepare units: %v %v", ok, err)
	}
	match := planning.MatchUnits(deployment.units, stale, deployment.nodeUUIDs, "")
	// Every instance is removed after the read, as an immediate restart does.
	for _, inst := range stale {
		if err := f.st.SetInstanceState(inst.UUID, proto.InstRemoving, "restart requested"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.ctrl.rollRestart(dep, stale, match); err != nil {
		t.Fatal(err)
	}
	now, _ := f.st.InstancesForDeployment(dep.ID)
	if got := statesOf(now); got[proto.InstDraining] != 0 || got[proto.InstRemoving] != len(stale) {
		t.Fatalf("states after the roll = %v, want every instance still removing", got)
	}
}
