package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/lifecycle"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// heldFixture is the automation fixture with three replicas, auto-1..3, each
// on a card of its own, behind the real automation and residency adapters.
func heldFixture(t *testing.T) (*automationAdapter, *residencyAdapter, *store.Store, *store.Deployment) {
	t.Helper()
	auto, st, dep := automationFixture(t)
	for i := 1; i <= 2; i++ {
		if err := st.DB().Create(&store.GPU{NodeID: dep.NodeID, Index: i, UUID: fmt.Sprintf("gpu-%d", i), Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 24 << 30}).Error; err != nil {
			t.Fatal(err)
		}
		uuid := fmt.Sprintf("auto-%d", i+1)
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true})
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: dep.NodeID, State: proto.InstRunning, GPUIndexes: fmt.Sprintf("[%d]", i), NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
	}
	px := proxy.New(st, noServingNetworks{}, nil, nil, 0)
	return auto, &residencyAdapter{store: st, proxy: px, orders: &lifecycle.Orders{}}, st, dep
}

// asleepAt reports each instance's sleep to the store, as its agent would.
func asleepAt(t *testing.T, st *store.Store, level int, uuids ...string) {
	t.Helper()
	for _, uuid := range uuids {
		if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: uuid, State: proto.InstRunning, SleepLevel: level}); err != nil {
			t.Fatal(err)
		}
	}
}

// holdsOf is each instance's {held, sleep intent}.
func holdsOf(t *testing.T, st *store.Store, uuids ...string) string {
	t.Helper()
	var out []string
	for _, uuid := range uuids {
		in := mustInstance(t, st, uuid)
		out = append(out, fmt.Sprintf("%s:%v/%d", uuid, in.SleepHeld, in.SleepIntent()))
	}
	return fmt.Sprint(out)
}

// An operator sleeps auto-1 while the arbiter puts auto-2 to sleep: a
// request (or a resume) wakes auto-2 and leaves the operator's auto-1
// asleep and held. Then the operator wakes auto-1 alone: auto-2 is as it
// was, and auto-3, which the operator also put to sleep, stays held.
func TestARequestWakesOnlyThePlatformsSleepers(t *testing.T) {
	auto, res, st, dep := heldFixture(t)
	if err := st.OrderInstancesSleepHeld(dep.ID, []string{"auto-1", "auto-3"}, 2, proto.SleepModeWait, true); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Yield(dep.ID, []string{"auto-2"}, false); err != nil {
		t.Fatal(err)
	}
	asleepAt(t, st, 2, "auto-1", "auto-3")
	asleepAt(t, st, 1, "auto-2")
	if got, want := holdsOf(t, st, "auto-1", "auto-2", "auto-3"), "[auto-1:true/2 auto-2:false/1 auto-3:true/2]"; got != want {
		t.Fatalf("holds = %s, want %s", got, want)
	}
	if _, ok := res.Wakeable(dep.ID); !ok {
		t.Fatal("a deployment with a sleeper the platform put to sleep is not wakeable, so the arbiter never resumes it")
	}

	result, err := auto.Wake(dep.ID, false)
	if err != nil || !slices.Equal(result.Woken, []string{"auto-2"}) || len(result.Waiting) != 0 {
		t.Fatalf("a request's wake = %+v err=%v, want auto-2 alone", result, err)
	}
	if got, want := holdsOf(t, st, "auto-1", "auto-2", "auto-3"), "[auto-1:true/2 auto-2:false/0 auto-3:true/2]"; got != want {
		t.Fatalf("after a request's wake holds = %s, want %s", got, want)
	}
	asleepAt(t, st, 0, "auto-2")
	if _, ok := res.Wakeable(dep.ID); ok {
		t.Fatal("only held sleepers left, and the deployment still reads wakeable")
	}
	// A second request's wake leaves the held sleepers be.
	if result, err := auto.Wake(dep.ID, false); err != nil || slices.Contains(result.Woken, "auto-1") || slices.Contains(result.Woken, "auto-3") || len(result.Waiting) != 0 {
		t.Fatalf("a request's wake with only held sleepers = %+v err=%v", result, err)
	}
	if got, want := holdsOf(t, st, "auto-1", "auto-3"), "[auto-1:true/2 auto-3:true/2]"; got != want {
		t.Fatalf("after a second request's wake holds = %s, want %s", got, want)
	}

	// The operator wakes auto-1 only, as its instance wake does.
	insts, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	match, err := planning.DeploymentMatch(st, dep, insts)
	if err != nil {
		t.Fatal(err)
	}
	if woken, refused, err := planning.OrderWake(st, dep, match, []store.Instance{*mustInstance(t, st, "auto-1")}, false, false); err != nil || len(woken) != 1 || len(refused) != 0 {
		t.Fatalf("the operator's wake of auto-1 = %v refused %v err=%v", woken, refused, err)
	}
	if got, want := holdsOf(t, st, "auto-1", "auto-2", "auto-3"), "[auto-1:false/0 auto-2:false/0 auto-3:true/2]"; got != want {
		t.Fatalf("after the operator's wake of auto-1 holds = %s, want %s", got, want)
	}
}

// A schedule's sleep holds every replica, a request wakes none of them, and
// the schedule's wake wakes them all and lifts every hold.
func TestAScheduledSleepAndWakeHoldAndReleaseEveryReplica(t *testing.T) {
	auto, res, st, dep := heldFixture(t)
	if _, err := auto.Sleep(dep.ID, 2, true); err != nil {
		t.Fatal(err)
	}
	asleepAt(t, st, 2, "auto-1", "auto-2", "auto-3")
	if got, want := holdsOf(t, st, "auto-1", "auto-2", "auto-3"), "[auto-1:true/2 auto-2:true/2 auto-3:true/2]"; got != want {
		t.Fatalf("after the scheduled sleep holds = %s, want %s", got, want)
	}
	if _, ok := res.Wakeable(dep.ID); ok {
		t.Fatal("a scheduled sleep reads wakeable by a request")
	}
	if result, err := auto.Wake(dep.ID, false); err != nil || len(result.Woken) != 0 {
		t.Fatalf("a request woke a scheduled sleep: %+v err=%v", result, err)
	}
	result, err := auto.Wake(dep.ID, true)
	if err != nil || len(result.Woken) != 3 {
		t.Fatalf("the scheduled wake = %+v err=%v, want all three woken", result, err)
	}
	if got, want := holdsOf(t, st, "auto-1", "auto-2", "auto-3"), "[auto-1:false/0 auto-2:false/0 auto-3:false/0]"; got != want {
		t.Fatalf("after the scheduled wake holds = %s, want %s", got, want)
	}
	if got, _ := st.GetDeployment(dep.ID); got.DesiredSleep != 0 {
		t.Fatalf("the deployment-wide order outlived the scheduled wake: %d", got.DesiredSleep)
	}
}
