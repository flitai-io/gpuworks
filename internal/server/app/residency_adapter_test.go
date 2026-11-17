package app

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/lifecycle"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/placewait"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// residencyFixture is the automation fixture's deployment asleep at L2 by
// the platform's choice (a request wakes it), and a sleepable offline
// deployment awake on its card.
func residencyFixture(t *testing.T) (*residencyAdapter, *automationAdapter, *store.Store, *store.Deployment, *store.Deployment) {
	t.Helper()
	auto, st, dep := automationFixture(t)
	if err := st.OrderInstancesSleep(dep.ID, []string{"auto-1"}, 2, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "auto-1", State: proto.InstRunning, SleepLevel: 2}); err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateDeployment(&store.Deployment{Name: "batch", ModelID: dep.ModelID, NodeID: dep.NodeID, ServedName: "batch", Engine: "vllm", Sleepable: true, State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	offline, _ := yieldpolicy.Parse(yieldpolicy.Anytime, yieldpolicy.Overrides{})
	if err := st.SetDeploymentYieldPolicy(other.ID, offline); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "batch-1", DesiredState: "running", Sleepable: true})
	if _, err := st.CreateInstance(&store.Instance{UUID: "batch-1", DeploymentID: other.ID, NodeID: dep.NodeID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	px := proxy.New(st, noServingNetworks{}, nil, nil, 0)
	return &residencyAdapter{store: st, proxy: px, acct: nil, orders: &lifecycle.Orders{}}, auto, st, dep, other
}

// noServingNetworks is a cluster with no serving network: the gateway reaches
// every node through its agent.
type noServingNetworks struct{}

func (noServingNetworks) ServingEndpoints() (map[string]network.ServingEndpoint, error) {
	return nil, nil
}

func TestResidencyAdapterFindsTheHolderAndPutsItToSleep(t *testing.T) {
	res, auto, st, dep, other := residencyFixture(t)
	if policy, ok := res.Wakeable(dep.ID); !ok || policy.Yield != yieldpolicy.Never {
		t.Fatalf("an auto-slept deployment is not wakeable: %+v %v", policy, ok)
	}
	if _, ok := res.Wakeable(other.ID); ok {
		t.Fatal("an awake deployment reads wakeable")
	}
	if b, err := res.Blocking(dep.ID); err != nil || len(b.Holders) != 0 || b.Blocked {
		t.Fatalf("blocking before its wake waits = %+v err=%v", b, err)
	}
	result, err := auto.Wake(dep.ID, false)
	if err != nil || len(result.Waiting) != 1 {
		t.Fatalf("wake = %+v err=%v", result, err)
	}
	b, err := res.Blocking(dep.ID)
	if err != nil || b.Blocked || len(b.Holders) != 1 {
		t.Fatalf("blocking = %+v err=%v", b, err)
	}
	h := b.Holders[0]
	if h.DeploymentID != other.ID || h.Name != "batch" || h.Policy.Yield != yieldpolicy.Anytime || !slices.Equal(h.Instances, []string{"batch-1"}) {
		t.Fatalf("holder = %+v", h)
	}

	level, err := res.Yield(other.ID, h.Instances, true)
	if err != nil || level != proto.SleepLevelL1 || intentOf(t, st, "batch-1") != proto.SleepLevelL1 {
		t.Fatalf("yield = %d err=%v intent=%d", level, err, intentOf(t, st, "batch-1"))
	}
	in, _ := st.InstanceByUUID("batch-1")
	var desired proto.InstanceDesiredState
	_ = json.Unmarshal([]byte(in.DesiredStateJSON), &desired)
	if desired.SleepMode != proto.SleepModeAbort {
		t.Fatalf("an offline holder's sleep mode = %q, want abort", desired.SleepMode)
	}
	if mustInstance(t, st, "batch-1").SleepHeld {
		t.Fatal("the arbiter's sleep is held: a request would not wake the holder again")
	}
	// Once the holder reports asleep, the waiting wake fits.
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "batch-1", State: proto.InstRunning, SleepLevel: 2}); err != nil {
		t.Fatal(err)
	}
	if woken, err := auto.WakeWaiting(dep.ID, result.Waiting); err != nil || len(woken) != 1 {
		t.Fatalf("waiting wake after the yield = %v err=%v", woken, err)
	}
	if policy, ok := res.Wakeable(other.ID); !ok || policy.Yield != yieldpolicy.Anytime {
		t.Fatalf("the holder put to sleep for another is not wakeable: %+v %v", policy, ok)
	}
}

// Whether a holder is starting is its own replica's: batch scaling to two,
// its second replica starting, still has its running replica named as a
// holder that is not starting; the deployment as a whole is settling.
func TestResidencyAdapterHolderStartingIsItsOwnReplicas(t *testing.T) {
	res, auto, st, dep, other := residencyFixture(t)
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", other.ID).Update("replicas", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "batch-2", DeploymentID: other.ID, NodeID: dep.NodeID, State: proto.InstPending, NNodes: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := auto.Wake(dep.ID, false); err != nil {
		t.Fatal(err)
	}
	b, err := res.Blocking(dep.ID)
	if err != nil || len(b.Holders) != 1 || !slices.Equal(b.Holders[0].Instances, []string{"batch-1"}) || b.Holders[0].Starting {
		t.Fatalf("blocking = %+v err=%v, want batch-1 running, not starting", b, err)
	}
	if !res.Settling(other.ID) {
		t.Fatal("a deployment with a replica starting does not read settling")
	}
	// The holder's own replica starting is.
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "batch-1", State: proto.InstStarting}); err != nil {
		t.Fatal(err)
	}
	if b, err := res.Blocking(dep.ID); err != nil || len(b.Holders) != 1 || !b.Holders[0].Starting {
		t.Fatalf("blocking = %+v err=%v, want batch-1 starting", b, err)
	}
}

// A replica waiting to be placed is blocked by the holders it waits for that
// are still awake with no sleep ordered.
func TestResidencyAdapterPlaceBlockingNamesTheAwakeHolders(t *testing.T) {
	res, _, st, dep, other := residencyFixture(t)
	res.waits, res.now = placewait.New(), time.Now
	res.waits.Wait(dep.ID, placewait.Cards{NodeID: dep.NodeID, GPUIndexes: []int{0}}, []string{"batch-1"}, time.Now())
	b, err := res.Blocking(dep.ID)
	if err != nil || len(b.Holders) != 1 || b.Holders[0].DeploymentID != other.ID || !slices.Equal(b.Holders[0].Instances, []string{"batch-1"}) {
		t.Fatalf("blocking = %+v err=%v", b, err)
	}
	if !res.Settling(dep.ID) {
		t.Fatal("a deployment waiting to be placed does not read settling")
	}
	if err := st.OrderInstancesSleepHeld(other.ID, []string{"batch-1"}, 1, proto.SleepModeWait, false); err != nil {
		t.Fatal(err)
	}
	if b, err := res.Blocking(dep.ID); err != nil || len(b.Holders) != 0 {
		t.Fatalf("blocking once the holder's sleep is ordered = %+v err=%v", b, err)
	}
}

// A holder a placement waits for that is still starting is still a holder,
// as a wake's occupant is (planning.YieldTargets): it reads starting, so the
// arbiter asks it nothing until it serves. One stopped is no holder.
func TestResidencyAdapterPlaceBlockingCountsAStartingHolder(t *testing.T) {
	res, _, st, dep, other := residencyFixture(t)
	res.waits, res.now = placewait.New(), time.Now
	res.waits.Wait(dep.ID, placewait.Cards{NodeID: dep.NodeID, GPUIndexes: []int{0}}, []string{"batch-1"}, time.Now())
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "batch-1", State: proto.InstStarting}); err != nil {
		t.Fatal(err)
	}
	b, err := res.Blocking(dep.ID)
	if err != nil || len(b.Holders) != 1 || b.Holders[0].DeploymentID != other.ID || !slices.Equal(b.Holders[0].Instances, []string{"batch-1"}) || !b.Holders[0].Starting {
		t.Fatalf("blocking = %+v err=%v, want batch-1 starting", b, err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "batch-1", State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	if b, err := res.Blocking(dep.ID); err != nil || len(b.Holders) != 0 {
		t.Fatalf("blocking once the holder stopped = %+v err=%v", b, err)
	}
}

func TestResidencyAdapterBlockingAndHeldSleeps(t *testing.T) {
	res, auto, st, dep, other := residencyFixture(t)
	// An occupant that cannot sleep blocks the wake.
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", other.ID).Update("sleepable", false).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := auto.Wake(dep.ID, false); err != nil {
		t.Fatal(err)
	}
	if b, err := res.Blocking(dep.ID); err != nil || !b.Blocked {
		t.Fatalf("blocking with an occupant that cannot sleep = %+v err=%v", b, err)
	}
	// An operator's sleep is not the arbiter's to end.
	if err := st.OrderDeploymentSleep(dep.ID, 2, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Wakeable(dep.ID); ok {
		t.Fatal("a held sleep reads wakeable")
	}
	if waiters, err := res.Waiters(); err != nil || len(waiters) != 0 {
		t.Fatalf("waiters with nothing held = %+v err=%v", waiters, err)
	}
	if act := res.Activity(dep.ID); act.InFlight != 0 || act.Held != 0 || !act.LastRequest.IsZero() {
		t.Fatalf("activity with no traffic = %+v", act)
	}
	res.Drain(other.ID, []string{"batch-1"}, true)
	if n := res.InstancesInFlight([]string{"batch-1"}); n != 0 {
		t.Fatalf("in flight on an idle replica = %d", n)
	}
	res.Drain(other.ID, []string{"batch-1"}, false)
}

// A host that cannot hold the weights gets an L2 yield instead of L1.
func TestResidencyAdapterYieldsAtL2WhenTheHostCannotHoldTheWeights(t *testing.T) {
	res, auto, st, dep, other := residencyFixture(t)
	res.host = func(string) (int64, int64, bool) { return 1 << 30, 64 << 30, true }
	if _, err := auto.Wake(dep.ID, false); err != nil {
		t.Fatal(err)
	}
	level, err := res.Yield(other.ID, []string{"batch-1"}, false)
	if err != nil || level != proto.SleepLevelL2 || intentOf(t, st, "batch-1") != proto.SleepLevelL2 {
		t.Fatalf("yield = %d err=%v intent=%d, want L2", level, err, intentOf(t, st, "batch-1"))
	}
}

// The holder's sleep level preference decides the level of its yield: l2
// whatever the host has, l1 strictly (refused where the host cannot hold the
// weights), auto L1 with the L2 fallback.
func TestResidencyAdapterYieldFollowsTheHoldersSleepLevelPreference(t *testing.T) {
	roomy := func(string) (int64, int64, bool) { return 64 << 30, 64 << 30, true }
	full := func(string) (int64, int64, bool) { return 1 << 30, 64 << 30, true }
	for _, c := range []struct {
		name, pref string
		host       func(string) (int64, int64, bool)
		level      int
		refused    bool
	}{
		{"auto with room", yieldpolicy.LevelAuto, roomy, proto.SleepLevelL1, false},
		{"auto without room", yieldpolicy.LevelAuto, full, proto.SleepLevelL2, false},
		{"l2 with room", yieldpolicy.LevelL2, roomy, proto.SleepLevelL2, false},
		{"l2 without room", yieldpolicy.LevelL2, full, proto.SleepLevelL2, false},
		{"l1 with room", yieldpolicy.LevelL1, roomy, proto.SleepLevelL1, false},
		{"l1 without room is refused", yieldpolicy.LevelL1, full, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, auto, st, dep, other := residencyFixture(t)
			res.host = c.host
			if err := st.SetDeploymentYieldPolicy(other.ID, yieldpolicy.Policy{Yield: yieldpolicy.Anytime, SleepLevel: c.pref, Unfinished: yieldpolicy.UnfinishedAbort, HoldMaxSec: 1800}); err != nil {
				t.Fatal(err)
			}
			if _, err := auto.Wake(dep.ID, false); err != nil {
				t.Fatal(err)
			}
			level, err := res.Yield(other.ID, []string{"batch-1"}, false)
			if c.refused {
				if err == nil || intentOf(t, st, "batch-1") != 0 {
					t.Fatalf("yield = %d err=%v intent=%d, want a refusal that changes nothing", level, err, intentOf(t, st, "batch-1"))
				}
				return
			}
			if err != nil || level != c.level || intentOf(t, st, "batch-1") != c.level {
				t.Fatalf("yield = %d err=%v intent=%d, want L%d", level, err, intentOf(t, st, "batch-1"), c.level)
			}
		})
	}
}
