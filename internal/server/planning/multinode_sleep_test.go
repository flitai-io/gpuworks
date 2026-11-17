package planning

import (
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// multiNodeSleepFixtures numbers the fixtures so each opens its own database.
var multiNodeSleepFixtures int

// multiNodeSleepFixture is a sleepable multi-node deployment of two
// replicas over two nodes: replica g1 led from GPU 0 of node a with its
// follower on GPU 0 of node b, replica g2 led from GPU 1 of node a with its
// follower on GPU 1 of node b. Every rank is ordered to sleep; leaderLevel is
// what the leaders report (followers report nothing of their own).
func multiNodeSleepFixture(t *testing.T, leaderLevel int) (*store.Store, *store.Node, *store.Node, map[string]store.Instance) {
	t.Helper()
	multiNodeSleepFixtures++
	st, err := store.Open(fmt.Sprintf("file:planning_mnsleep_%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), multiNodeSleepFixtures), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	nodes := make([]*store.Node, 0, 2)
	for _, name := range []string{"node-a", "node-b"} {
		node, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		for index := range 2 {
			if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: index, UUID: fmt.Sprintf("%s-gpu-%d", name, index), Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 16 << 30}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if node, err = st.NodeByID(node.ID); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "multi", ModelID: 1, NodeID: nodes[0].ID, ServedName: "multi", Engine: "vllm", Sleepable: true, State: "active", NNodes: 2, Replicas: 2, DesiredSleep: 1})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]store.Instance{}
	for _, row := range []store.Instance{
		{UUID: "g1-leader", NodeID: nodes[0].ID, GPUIndexes: "[0]", DistGroup: "g1", SleepLevel: leaderLevel},
		{UUID: "g1-follower", NodeID: nodes[1].ID, GPUIndexes: "[0]", DistGroup: "g1", Headless: true, NodeRank: 1},
		{UUID: "g2-leader", NodeID: nodes[0].ID, GPUIndexes: "[1]", DistGroup: "g2", SleepLevel: leaderLevel},
		{UUID: "g2-follower", NodeID: nodes[1].ID, GPUIndexes: "[1]", DistGroup: "g2", Headless: true, NodeRank: 1},
	} {
		row.DeploymentID, row.State, row.NNodes, row.DistAttempt = dep.ID, proto.InstRunning, 2, 1
		row.DesiredStateJSON = sleepSpec(row.UUID, 1)
		if _, err := st.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
		rows[row.UUID] = row
	}
	return st, nodes[0], nodes[1], rows
}

// A follower's engine sleeps with its leader's: once the leader reports
// asleep, the follower's card is charged only the residue. Before that, or
// without a running leader, the follower keeps its full claim.
func TestAFollowerGivesItsCardUpWithItsLeader(t *testing.T) {
	st, _, nodeB, rows := multiNodeSleepFixture(t, 1)
	if got := GPUAllocations(st, nodeB)[0]; got.AllocatedBytes != 0 || got.SleepingBytes != sleep.UnknownResidueBytes {
		t.Fatalf("follower of an asleep leader = %+v, want residue only", got)
	}
	follower := rows["g1-follower"]
	if !NeedsRoomToWake(st, &follower) {
		t.Fatal("waking an asleep follower needs no room")
	}

	st, _, nodeB, _ = multiNodeSleepFixture(t, 0)
	if got := GPUAllocations(st, nodeB)[0]; got.AllocatedBytes != 16<<30 || got.SleepingBytes != 0 {
		t.Fatalf("follower of a leader that has not slept yet = %+v, want the whole card", got)
	}

	st, _, nodeB, _ = multiNodeSleepFixture(t, 1)
	if err := st.SetInstanceFailed("g1-leader", proto.RuntimeFailure{Code: proto.ErrorContainerExited}, "gone"); err != nil {
		t.Fatal(err)
	}
	if got := GPUAllocations(st, nodeB)[0]; got.AllocatedBytes != 16<<30 {
		t.Fatalf("follower of a failed leader = %+v, want the whole card", got)
	}
}

// A partial wake brings a multi-node replica back whole or not at all: with
// an awake co-tenant on the card g2's follower slept on, g2 stays asleep
// even though its leader's card is free, and g1 wakes. All at once, the
// wake is refused.
func TestAPartialWakeAdmitsAMultiNodeReplicaWhole(t *testing.T) {
	st, _, nodeB, rows := multiNodeSleepFixture(t, 1)
	tenant, err := st.CreateDeployment(&store.Deployment{Name: "tenant", ModelID: 2, NodeID: nodeB.ID, ServedName: "tenant", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "tenant-0", DeploymentID: tenant.ID, NodeID: nodeB.ID, State: proto.InstRunning, GPUIndexes: "[1]"}); err != nil {
		t.Fatal(err)
	}
	groups := [][]store.Instance{{rows["g1-leader"], rows["g1-follower"]}, {rows["g2-leader"], rows["g2-follower"]}}

	admitted, refused, err := AdmitWake(st, groups, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := uuids(admitted); strings.Join(got, ",") != "g1-leader,g1-follower" {
		t.Fatalf("admitted = %v, want g1 whole", got)
	}
	if len(refused) != 1 || refused[0].Node.ID != nodeB.ID || strings.Join(uuids(refused[0].Instances), ",") != "g2-leader,g2-follower" {
		t.Fatalf("refused = %+v, want g2 whole, refused on node b", refused)
	}
	if occupants := refused[0].Verdict.Shortfalls[0].Occupants; len(occupants) != 1 || occupants[0] != "tenant" {
		t.Fatalf("occupants = %v", occupants)
	}

	admitted, refused, err = AdmitWake(st, groups, false)
	if err != nil || len(admitted) != 0 || len(refused) != 1 || refused[0].Node.ID != nodeB.ID {
		t.Fatalf("all at once = %v %+v %v, want refused on node b", uuids(admitted), refused, err)
	}
}

func uuids(insts []store.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, in := range insts {
		out = append(out, in.UUID)
	}
	return out
}

// A wake that fails on the leader rolls the whole replica back: the leader
// reports it is still asleep, so its followers' intent goes back to sleep
// with its own. The follower's card is then charged only the residue again,
// and the next wake is admitted - before, the follower kept its woken intent,
// was charged the whole card, and the retry was refused against the replica's
// own memory.
func TestAFailedWakeRollsTheWholeReplicaBack(t *testing.T) {
	st, _, nodeB, rows := multiNodeSleepFixture(t, 1)
	dep := rows["g1-leader"].DeploymentID
	if err := st.OrderInstancesSleep(dep, []string{"g1-leader", "g1-follower"}, 0, ""); err != nil {
		t.Fatal(err)
	}
	stillAsleep, target := 1, 0
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "g1-leader", State: proto.InstRunning, SleepLevel: stillAsleep, SleepTarget: &target, Phase: proto.PhaseSleep, Message: "wake_up failed"}); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"g1-leader", "g1-follower"} {
		inst, err := st.InstanceByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if got := InstanceSleepIntent(inst); got != 1 {
			t.Fatalf("%s intent after the failed wake = %d, want 1", uuid, got)
		}
	}
	if got := GPUAllocations(st, nodeB)[0]; got.AllocatedBytes != 0 || got.SleepingBytes != sleep.UnknownResidueBytes {
		t.Fatalf("follower card after the failed wake = %+v, want residue only", got)
	}
	leader, _ := st.InstanceByUUID("g1-leader")
	follower, _ := st.InstanceByUUID("g1-follower")
	admitted, refused, err := AdmitWake(st, [][]store.Instance{{*leader, *follower}}, false)
	if err != nil || len(refused) != 0 || len(admitted) != 2 {
		t.Fatalf("retried wake = admitted %v refused %+v err %v, want both admitted", uuids(admitted), refused, err)
	}
}

// A wake already in flight takes nothing more: the ledger charges the
// instance its whole claim the moment its intent is awake, so asking again
// must not count that claim a second time and refuse.
func TestAWakeInFlightNeedsNoRoom(t *testing.T) {
	st, _, _, rows := multiNodeSleepFixture(t, 1)
	dep := rows["g1-leader"].DeploymentID
	if err := st.OrderInstancesSleep(dep, []string{"g1-leader", "g1-follower"}, 0, ""); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"g1-leader", "g1-follower"} {
		inst, _ := st.InstanceByUUID(uuid)
		if NeedsRoomToWake(st, inst) {
			t.Fatalf("%s needs room while its wake is in flight", uuid)
		}
	}
}
