package planning

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// assertFleetMatchesPerNode compares each card of FleetCards with the figures
// the per-node ledger gives for it.
func assertFleetMatchesPerNode(t *testing.T, st *store.Store, nodes ...*store.Node) {
	t.Helper()
	list := make([]store.Node, 0, len(nodes))
	for _, node := range nodes {
		list = append(list, *node)
	}
	fleet, err := FleetCards(st, list)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		allocations := GPUAllocations(st, node)
		occupants := GPUOccupants(st, node)
		cards := fleet.Cards[node.ID]
		if len(cards) != len(node.OwnedGPUs()) {
			t.Fatalf("node %s: %d cards, want %d", node.Name, len(cards), len(node.OwnedGPUs()))
		}
		for _, card := range cards {
			alloc := allocations[card.Index]
			if card.AwakeBytes != alloc.AllocatedBytes || card.SleepingBytes != alloc.SleepingBytes {
				t.Errorf("node %s card %d: fleet awake/sleeping = %d/%d, per-node ledger = %d/%d",
					node.Name, card.Index, card.AwakeBytes, card.SleepingBytes, alloc.AllocatedBytes, alloc.SleepingBytes)
			}
			var want []FleetOccupant
			for _, o := range occupants[card.Index] {
				want = append(want, FleetOccupant{
					DeploymentID: o.Instance.DeploymentID, Asleep: o.Asleep, Instance: o.Instance.UUID,
					SleepLevel: o.Instance.SleepLevel, ClaimBytes: o.ClaimBytes, ResidueBytes: o.ResidueBytes,
				})
			}
			if !reflect.DeepEqual(card.Occupants, want) {
				t.Errorf("node %s card %d: occupants %+v, want %+v", node.Name, card.Index, card.Occupants, want)
			}
		}
		// One node read on its own gives the cards the fleet pass gives it.
		alone, err := NodeCards(st, node)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(alone, cards) {
			t.Errorf("node %s: NodeCards %+v, FleetCards %+v", node.Name, alone, cards)
		}
	}
}

// A card's state is the first that holds: offline, unhealthy, held outside
// with nothing of ours serving on it, awake, asleep, free. A card that
// serves partitions beside processes outside the platform (ADR 0068) is
// serving: reading it as held would hide that the platform works on it.
func TestFleetCardStatePrecedence(t *testing.T) {
	for _, tc := range []struct {
		card   FleetCard
		online bool
		want   CardState
	}{
		{FleetCard{Unhealthy: true, AwakeBytes: 1}, false, CardOffline},
		{FleetCard{Unhealthy: true, HeldOutside: true, AwakeBytes: 1}, true, CardUnhealthy},
		{FleetCard{HeldOutside: true, SleepingBytes: 1}, true, CardHeldOutside},
		{FleetCard{HeldOutside: true, AwakeBytes: 1, SleepingBytes: 1}, true, CardAwake},
		{FleetCard{AwakeBytes: 1, SleepingBytes: 1}, true, CardAwake},
		{FleetCard{SleepingBytes: 1}, true, CardAsleep},
		{FleetCard{}, true, CardFree},
	} {
		if got := tc.card.State(tc.online); got != tc.want {
			t.Errorf("%+v online=%v = %s, want %s", tc.card, tc.online, got, tc.want)
		}
	}
	if len(CardStates) != 6 || CardStates[0] != CardOffline || CardStates[5] != CardFree {
		t.Fatalf("CardStates = %v", CardStates)
	}
}

// The fleet pass charges the cards exactly as the per-node ledger does,
// including a follower that sleeps with its leader.
func TestFleetCardsMatchThePerNodeLedger(t *testing.T) {
	for _, level := range []int{0, 1} {
		t.Run(fmt.Sprintf("leader level %d", level), func(t *testing.T) {
			st, a, b, _ := multiNodeSleepFixture(t, level)
			assertFleetMatchesPerNode(t, st, a, b)
			fleet, err := FleetCards(st, []store.Node{*a, *b})
			if err != nil {
				t.Fatal(err)
			}
			follower := fleet.Cards[b.ID][0]
			if asleep := level == 1; len(follower.Occupants) != 1 || follower.Occupants[0].Asleep != asleep {
				t.Fatalf("follower card occupants = %+v, want asleep=%v", follower.Occupants, asleep)
			}
		})
	}
}

// A sleeper is charged the measured residue when there is one, else the
// estimate of its model, from rows read for the fleet and not one by one.
func TestFleetCardsChargeASleepersResidue(t *testing.T) {
	st, node, dep := residueFixture(t, 16<<30, "bfloat16")
	sleepReported(t, st, dep, 0)
	assertFleetMatchesPerNode(t, st, node)
	fleet, err := FleetCards(st, []store.Node{*node})
	if err != nil {
		t.Fatal(err)
	}
	if got := fleet.Cards[node.ID][0]; got.SleepingBytes != 3<<29 || got.AwakeBytes != 0 {
		t.Fatalf("estimated residue card = %+v, want 1.5 GiB sleeping", got)
	}
	sleepReported(t, st, dep, 700<<20)
	if fleet, err = FleetCards(st, []store.Node{*node}); err != nil {
		t.Fatal(err)
	}
	if got := fleet.Cards[node.ID][0].SleepingBytes; got != 700<<20 {
		t.Fatalf("measured residue = %d, want 700 MiB", got)
	}
}

// What the card shows besides the ledger: a process outside the platform, a
// faulty card, an instance on its way up or to sleep, a card the vendor tool
// does not list, and one the node does not own.
func TestFleetCardsFlagWhatTheLedgerDoesNotSay(t *testing.T) {
	st, node, dep := residueFixture(t, 16<<30, "bfloat16")
	db := st.DB()
	for _, gpu := range []store.GPU{
		{NodeID: node.ID, Index: 1, UUID: "g1", MemoryTotal: 48 << 30, ForeignProcesses: `[{"pid":7}]`},
		{NodeID: node.ID, Index: 2, UUID: "g2", MemoryTotal: 48 << 30, HealthStatus: "Error"},
		{NodeID: node.ID, Index: -1, UUID: "g3", MemoryTotal: 48 << 30},
		{NodeID: node.ID, Index: 4, UUID: "g4", MemoryTotal: 48 << 30, Ownership: "vfio"},
	} {
		if err := db.Create(&gpu).Error; err != nil {
			t.Fatal(err)
		}
	}
	// The instance is ordered to sleep but its engine has not slept yet.
	if err := st.SetDeploymentSleep(dep.ID, 2); err != nil {
		t.Fatal(err)
	}
	reloaded, err := st.NodeByID(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := FleetCards(st, []store.Node{*reloaded})
	if err != nil {
		t.Fatal(err)
	}
	cards := fleet.Cards[node.ID]
	if len(cards) != 3 {
		t.Fatalf("cards = %d, want the 3 listed and owned: %+v", len(cards), cards)
	}
	if !cards[0].Switching || cards[0].AwakeBytes != 48<<30 {
		t.Errorf("card 0 = %+v, want switching and still fully claimed", cards[0])
	}
	if !cards[1].HeldOutside || cards[1].Unhealthy {
		t.Errorf("card 1 = %+v, want held outside only", cards[1])
	}
	if !cards[2].Unhealthy || cards[2].HeldOutside {
		t.Errorf("card 2 = %+v, want unhealthy only", cards[2])
	}
}

// A fleet is read in a number of statements that does not grow with its
// nodes: the live instances, the deployments and the sleepers' models.
func TestFleetCardsReadsDoNotGrowWithTheNodes(t *testing.T) {
	reads := func(t *testing.T, nodes int) int64 {
		t.Helper()
		st, base, dep := residueFixture(t, 16<<30, "bfloat16")
		_ = base
		sleepReported(t, st, dep, 0)
		var list []store.Node
		for i := 0; i < nodes; i++ {
			node, err := st.RegisterNode(fmt.Sprintf("n%d", i), store.HashToken(fmt.Sprintf("t%d", i)), fmt.Sprintf("n%d", i))
			if err != nil {
				t.Fatal(err)
			}
			for g := 0; g < 2; g++ {
				if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: g, UUID: fmt.Sprintf("n%d-%d", i, g), MemoryTotal: 48 << 30}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if i%2 == 0 {
				in := store.Instance{UUID: fmt.Sprintf("sleeper-%d", i), DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, SleepLevel: 2, DesiredStateJSON: sleepSpec("x", 2)}
				if err := st.DB().Create(&in).Error; err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := st.NodeByID(node.ID)
			if err != nil {
				t.Fatal(err)
			}
			list = append(list, *loaded)
		}
		var count atomic.Int64
		db := st.DB()
		name := "count-" + t.Name() + fmt.Sprint(nodes)
		if err := db.Callback().Query().Before("gorm:query").Register(name, func(*gorm.DB) { count.Add(1) }); err != nil {
			t.Fatal(err)
		}
		if _, err := FleetCards(st, list); err != nil {
			t.Fatal(err)
		}
		return count.Load()
	}
	var few, many int64
	t.Run("3 nodes", func(t *testing.T) { few = reads(t, 3) })
	t.Run("40 nodes", func(t *testing.T) { many = reads(t, 40) })
	if few != many {
		t.Fatalf("FleetCards ran %d statements for 3 nodes and %d for 40; it must not grow with the nodes", few, many)
	}
	if many > 6 {
		t.Fatalf("FleetCards ran %d statements, want a handful", many)
	}
}

// A replica awake on two cards (TP=2) beside another deployment asleep on
// one of them: both cards are awake, the replica is on each with its claim,
// the sleeper only on its own card with its residue, as the per-node ledger
// charges them.
func TestFleetCardsOfAReplicaSpanningCardsBesideASleeper(t *testing.T) {
	c := newYieldCards(t, 2)
	a := c.deployment(t, "a", "anytime")
	b := c.deployment(t, "b", "timeslice")
	c.instance(t, a, "a-1", "[0,1]", 0)
	c.instance(t, b, "b-1", "[1]", 1)
	// The replica was admitted with 10 GiB of each card, which leaves room
	// for the sleeper's residue beside it.
	claim, _ := json.Marshal(proto.InstanceDesiredState{UUID: "a-1", DesiredState: "running", Sleepable: true, MemoryClaimBytes: 10 << 30})
	if err := c.st.DB().Model(&store.Instance{}).Where("uuid = ?", "a-1").Select("DesiredStateJSON").Updates(&store.Instance{DesiredStateJSON: string(claim)}).Error; err != nil {
		t.Fatal(err)
	}
	assertFleetMatchesPerNode(t, c.st, c.node)
	fleet, err := FleetCards(c.st, []store.Node{*c.node})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, card := range fleet.Cards[c.node.ID] {
		line := fmt.Sprintf("%d %s sleeping=%v", card.Index, card.State(true), card.SleepingBytes > 0)
		for _, o := range card.Occupants {
			line += fmt.Sprintf(" %s/asleep=%v/residue=%v", o.Instance, o.Asleep, o.ResidueBytes > 0)
		}
		got = append(got, line)
	}
	want := []string{
		"0 awake sleeping=false a-1/asleep=false/residue=false",
		"1 awake sleeping=true a-1/asleep=false/residue=false b-1/asleep=true/residue=true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cards =\n%v\nwant\n%v", got, want)
	}
	if cards := fleet.Cards[c.node.ID]; cards[0].AwakeBytes <= 0 || cards[0].AwakeBytes != cards[1].AwakeBytes {
		t.Fatalf("the replica's claim is not the same on both its cards: %+v", cards)
	}
}
