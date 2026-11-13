package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// occupiedProber reports the compute processes it is given.
type occupiedProber struct {
	gpu.FixedProber
	procs []gpu.ComputeProcess
	err   error
}

func (p *occupiedProber) ComputeProcesses(context.Context) ([]gpu.ComputeProcess, error) {
	return p.procs, p.err
}

// occupancyNode is an agent that has been told what it should be running
// — nothing, unless a case says otherwise. A node that has not been told
// yet is its own case (TestAnAgentThatDoesNotYetKnowItsOwnInstances...).
func occupancyNode(t *testing.T, cards []proto.GPUInfo, procs []gpu.ComputeProcess, err error) *NodeRuntime {
	t.Helper()
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: cards}, procs: procs, err: err}, nil)
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
	return n
}

func twoOwnedCards() []proto.GPUInfo {
	return []proto.GPUInfo{
		{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true},
		{Index: 1, UUID: "GPU-40", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", Healthy: true},
	}
}

// Somebody logged into the node and started their own workload. Its memory
// is spoken for by something the platform cannot plan around, so the card
// is reported as taken, with who has it.
func TestACardUsedOutsideThePlatformIsReportedWithWhoHasIt(t *testing.T) {
	n := occupancyNode(t, twoOwnedCards(), []gpu.ComputeProcess{
		// The vendor tool prints eight domain digits; the card's own
		// address has four.
		{PID: 4242, Name: "python3", PCIeBDF: "00000000:40:00.0", MemoryBytes: 20 << 30},
	}, nil)
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)

	if len(cards[0].ForeignProcesses) != 0 {
		t.Fatalf("the idle card = %+v, want nothing on it", cards[0].ForeignProcesses)
	}
	taken := cards[1].ForeignProcesses
	if len(taken) != 1 || taken[0].PID != 4242 || taken[0].Name != "python3" || taken[0].MemoryBytes != 20<<30 {
		t.Fatalf("the taken card = %+v", taken)
	}
}

// Not knowing is not the same as knowing a card is free: a vendor tool that
// cannot be asked leaves every card as the last report left it.
func TestAnUnavailableProcessListLeavesCardsAlone(t *testing.T) {
	n := occupancyNode(t, twoOwnedCards(), nil, errors.New("nvidia-smi: no answer within 10s"))
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	for _, card := range cards {
		if len(card.ForeignProcesses) != 0 {
			t.Fatalf("card = %+v, want nothing claimed when the tool could not be asked", card)
		}
	}
}

// An agent that has just restarted has its instances still running on the
// cards while the control plane has not yet told it what they are. Reading
// "I am told of no instances" as "none of these cards are mine" calls every
// card the platform is using somebody else's, and stops scheduling on
// exactly the busiest node — seen on gpu3 after an agent upgrade.
func TestAnAgentThatDoesNotYetKnowItsOwnInstancesClaimsNothing(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
			procs: []gpu.ComputeProcess{{PID: 7001, Name: "vllm", PCIeBDF: "0000:3f:00.0"}}}, nil)
	if ours, known := n.cardsInUse(); known || len(ours) != 0 {
		t.Fatalf("cardsInUse = %v known=%v, want it to admit it does not know", ours, known)
	}
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	for _, card := range cards {
		if len(card.ForeignProcesses) != 0 {
			t.Fatalf("card %s = %+v, want nothing claimed before this agent knows its own instances",
				card.PCIeBDF, card.ForeignProcesses)
		}
	}
}

// A card this node does not own is not examined: it is somebody else's
// hardware and its processes are not ours to report.
func TestCardsOwnedElsewhereAreNotExamined(t *testing.T) {
	cards := []proto.GPUInfo{
		{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
	}
	n := occupancyNode(t, cards, []gpu.ComputeProcess{{PID: 1, Name: "qemu", PCIeBDF: "0000:1b:00.0"}}, nil)
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[0].ForeignProcesses) != 0 {
		t.Fatalf("a passed-through card = %+v, want its VM's processes not reported as foreign", cards[0].ForeignProcesses)
	}
}

// A prober whose tool does not list processes at all reports nothing, and
// nothing breaks.
func TestAProberWithoutProcessListingReportsNothing(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "ascend", GPUs: twoOwnedCards()}, nil)
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	for _, card := range cards {
		if len(card.ForeignProcesses) != 0 {
			t.Fatalf("card = %+v", card)
		}
	}
}

// A card already running one of this platform's instances is not examined:
// its processes are ours, and the card is accounted for and not offered
// again anyway.
func TestACardRunningOurOwnInstanceHasNoForeignProcesses(t *testing.T) {
	n := occupancyNode(t, twoOwnedCards(), []gpu.ComputeProcess{
		{PID: 7001, Name: "vllm", PCIeBDF: "0000:3f:00.0", MemoryBytes: 20 << 30},
		{PID: 4242, Name: "python3", PCIeBDF: "0000:40:00.0", MemoryBytes: 8 << 30},
	}, nil)
	// The card at 3f runs one of ours, named by its identity rather than
	// by a device index that drifts.
	n.heldCards = func() engine.HeldCards {
		return engine.HeldCards{Idents: []string{accelerator.CardIdent("", "0000:3f:00.0")}}
	}

	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[0].ForeignProcesses) != 0 {
		t.Fatalf("our own instance was reported as foreign: %+v", cards[0].ForeignProcesses)
	}
	if len(cards[1].ForeignProcesses) != 1 {
		t.Fatalf("the card nothing of ours runs on = %+v, want the foreign process named", cards[1].ForeignProcesses)
	}
}

// The server writes each report's occupancy over what it had, so a round
// that could not find out must say what the last one found. Saying nothing
// read as "free", and the card somebody else was using went to a
// deployment one report after the vendor tool first timed out.
func TestARoundThatCannotFindOutRestatesTheLastAnswer(t *testing.T) {
	prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
		procs: []gpu.ComputeProcess{{PID: 4242, Name: "python3", PCIeBDF: "0000:40:00.0"}}}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none", prober, nil)
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[1].ForeignProcesses) != 1 {
		t.Fatalf("first round = %+v", cards[1].ForeignProcesses)
	}

	for _, round := range []struct {
		name    string
		probeOK bool
		err     error
	}{
		{"the process list timed out", true, errors.New("nvidia-smi: no answer within 10s")},
		{"the card probe itself failed", false, nil},
	} {
		prober.err = round.err
		cards = twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, round.probeOK)
		if got := cards[1].ForeignProcesses; len(got) != 1 || got[0].PID != 4242 {
			t.Fatalf("%s: the taken card = %+v, want the last answer restated", round.name, got)
		}
		if len(cards[0].ForeignProcesses) != 0 {
			t.Fatalf("%s: the idle card = %+v", round.name, cards[0].ForeignProcesses)
		}
	}

	// A round that does find out replaces the answer, including with
	// "nobody": the process went away, and after occupancyReleaseRounds
	// free readings in a row the card is free again.
	prober.err, prober.procs = nil, nil
	for i := 1; i < occupancyReleaseRounds; i++ {
		cards = twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, true)
		if len(cards[1].ForeignProcesses) != 1 {
			t.Fatalf("free reading %d let the card go early: %+v", i, cards[1].ForeignProcesses)
		}
	}
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[1].ForeignProcesses) != 0 {
		t.Fatalf("after the process left = %+v, want the card free", cards[1].ForeignProcesses)
	}
	prober.err = errors.New("timeout")
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[1].ForeignProcesses) != 0 {
		t.Fatalf("a later failed round brought back %+v", cards[1].ForeignProcesses)
	}
}

// The shape seen on a live node: a vLLM instance launched after cards were
// bound by identity on device 0, and a callable engine (strata) launched
// before, whose desired state names only device 1. Both cards were in use
// by the platform, yet the second was reported as held outside it — with
// strata's own processes as the culprits — and stopped being scheduled.
func TestAnInstanceWithoutACardBindingStillOwnsItsCard(t *testing.T) {
	cards := func() []proto.GPUInfo {
		return []proto.GPUInfo{
			{Index: 0, UUID: "GPU-b1b4", Vendor: "nvidia", PCIeBDF: "00000000:32:00.0", Healthy: true},
			{Index: 1, UUID: "GPU-1863", Vendor: "nvidia", PCIeBDF: "00000000:4B:00.0", Healthy: true},
		}
	}
	n := occupancyNode(t, cards(), []gpu.ComputeProcess{
		{PID: 900, Name: "python3", PCIeBDF: "00000000:32:00.0", MemoryBytes: 8 << 30},
		{PID: 1744, Name: "/opt/strata/engine/strata-vision", PCIeBDF: "00000000:4B:00.0", MemoryBytes: 1 << 30},
		{PID: 1750, Name: "/opt/strata/engine/strata", PCIeBDF: "00000000:4B:00.0", MemoryBytes: 21 << 30},
	}, nil)
	n.heldCards = func() engine.HeldCards {
		return engine.HeldCards{Idents: []string{accelerator.CardIdent("GPU-b1b4", "00000000:32:00.0")}, Indexes: []int{1}}
	}
	got := cards()
	n.CardHealthFindings(context.Background(), got, true)
	for _, card := range got {
		if len(card.ForeignProcesses) != 0 {
			t.Fatalf("card %s = %+v, want the platform's own instances not called foreign", card.PCIeBDF, card.ForeignProcesses)
		}
	}
	// And the card it does not hold is still examined.
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{Indexes: []int{1}} }
	got = cards()
	n.CardHealthFindings(context.Background(), got, true)
	if p := got[0].ForeignProcesses; len(p) != 1 || p[0].PID != 900 {
		t.Fatalf("the card nothing of ours holds = %+v, want its process named", p)
	}
	if len(got[1].ForeignProcesses) != 0 {
		t.Fatalf("the unbound instance's card = %+v", got[1].ForeignProcesses)
	}
}

// Naming an unbound instance's card needs a probe that listed the cards.
// Before one has (or when its index is not among them) which card it holds
// is not known, and not knowing is not the same as "not ours": the last
// answer is restated rather than its processes called foreign.
func TestAnUnboundInstanceWhoseCardIsNotNamedYetClaimsNothing(t *testing.T) {
	n := occupancyNode(t, twoOwnedCards(), []gpu.ComputeProcess{
		{PID: 7001, Name: "vllm", PCIeBDF: "0000:3f:00.0"},
	}, nil)
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{Indexes: []int{0}} }
	if ours, known := n.cardsInUse(); known || len(ours) != 0 {
		t.Fatalf("before any probe cardsInUse = %v known=%v, want not known", ours, known)
	}
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if ours, known := n.cardsInUse(); !known || !ours[accelerator.CardIdent("GPU-3f", "0000:3f:00.0")] {
		t.Fatalf("after a probe cardsInUse = %v known=%v, want device 0's card", ours, known)
	}
	if len(cards[0].ForeignProcesses) != 0 {
		t.Fatalf("card 0 = %+v", cards[0].ForeignProcesses)
	}

	// An index the probe did not list: restated, not claimed.
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{Indexes: []int{5}} }
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	for _, card := range cards {
		if len(card.ForeignProcesses) != 0 {
			t.Fatalf("card %s = %+v, want nothing claimed while an instance's card cannot be named", card.PCIeBDF, card.ForeignProcesses)
		}
	}
	if _, known := n.cardsInUse(); known {
		t.Fatal("an index no probe named read as known")
	}
}

// End to end through the engine: the control plane replays a desired state
// without identities (an instance launched before cards were bound), the
// recording runtime "runs" it, and the occupancy report does not call its
// card foreign — nor the headless follower's of a two-node unit.
func TestAReplayedSpecWithoutIdentitiesKeepsItsCardThroughTheEngine(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec proto.InstanceDesiredState
	}{
		{"single node", proto.InstanceDesiredState{UUID: "strata", DesiredState: "running", Engine: "callable", Image: "strata:0.1", GPUIndexes: []int{1}, APIPort: 30001}},
		{"headless follower", proto.InstanceDesiredState{UUID: "follower", DesiredState: "running", Engine: "callable", Image: "glm:1", GPUIndexes: []int{1}, Headless: true, DistRole: "follower", NNodes: 2, NodeRank: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
				procs: []gpu.ComputeProcess{{PID: 1750, Name: "/opt/strata/engine/strata", PCIeBDF: "0000:40:00.0", MemoryBytes: 21 << 30}}}
			n := newNodeRuntime(slog.New(slog.DiscardHandler), recording.New(filepath.Join(t.TempDir(), "rec.jsonl")),
				filepath.Join(t.TempDir(), "models"), "", prober, nil, engine.WithNetworkInterfaceResolver(nil))
			n.engine.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{tc.spec})

			cards := twoOwnedCards()
			n.CardHealthFindings(context.Background(), cards, true)
			if len(cards[1].ForeignProcesses) != 0 {
				t.Fatalf("the instance's own card = %+v, want it not called foreign", cards[1].ForeignProcesses)
			}
			// Withdrawn by the control plane: once its teardown is over, a
			// process still on the card is somebody else's. (While the
			// teardown runs the card stays ours - engine.HeldCards.)
			n.engine.SetDesiredFull(context.Background(), nil)
			deadline := time.Now().Add(10 * time.Second)
			for held := n.engine.HeldCards(); len(held.Idents)+len(held.Indexes) > 0; held = n.engine.HeldCards() {
				if time.Now().After(deadline) {
					t.Fatalf("the withdrawn instance's teardown never finished: %+v", held)
				}
				time.Sleep(10 * time.Millisecond)
			}
			cards = twoOwnedCards()
			n.CardHealthFindings(context.Background(), cards, true)
			if p := cards[1].ForeignProcesses; len(p) != 1 || p[0].PID != 1750 {
				t.Fatalf("after the instance was withdrawn and torn down = %+v, want the process named", p)
			}
		})
	}
}

// A prober that cannot list processes still names the cards: the runtime
// check asks which cards are ours as well, and an unbound instance's card
// that never got a name would hold it back as "not known yet" forever.
func TestCardsAreNamedEvenWhenTheProberCannotListProcesses(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()}, nil)
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{Indexes: []int{1}} }
	n.CardHealthFindings(context.Background(), twoOwnedCards(), true)
	if ours, known := n.cardsInUse(); !known || !ours[accelerator.CardIdent("GPU-40", "0000:40:00.0")] {
		t.Fatalf("cardsInUse = %v known=%v, want device 1's card", ours, known)
	}
}

// A card whose only instances of ours all sleep is examined after all: the
// sleeping engine's own process is known, and anything else there is
// somebody else's — a wake onto the card would run out of memory. Without a
// known process for the sleeper the card is left alone, as before.
func TestASleepersCardReportsAProcessThatIsNotItsEngine(t *testing.T) {
	ident := accelerator.CardIdent("", "0000:3f:00.0")
	n := occupancyNode(t, twoOwnedCards(), []gpu.ComputeProcess{
		{PID: 7001, Name: "vllm", PCIeBDF: "0000:3f:00.0", MemoryBytes: 600 << 20},
		{PID: 4242, Name: "python3", PCIeBDF: "00000000:3f:00.0", MemoryBytes: 4 << 30},
	}, nil)
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{Idents: []string{ident}} }
	n.sleepingCards = func() map[string]map[int]bool { return map[string]map[int]bool{ident: {7001: true}} }

	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if got := cards[0].ForeignProcesses; len(got) != 1 || got[0].PID != 4242 || got[0].MemoryBytes != 4<<30 {
		t.Fatalf("the sleeper's card = %+v, want only the process that is not its engine", got)
	}

	// The sleeper's process unknown (its residue was not measured): the card
	// is not examined.
	n.sleepingCards = func() map[string]map[int]bool { return nil }
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[0].ForeignProcesses) != 0 {
		t.Fatalf("a card of ours with unknown processes = %+v, want it left alone", cards[0].ForeignProcesses)
	}
}

// The runtime check's own process on a card nobody uses is not somebody
// outside the platform: while it runs, and for a moment after (the vendor
// listing can be a few seconds old), the card keeps its last answer.
func TestTheRuntimeChecksOwnProcessIsNotForeign(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	n := occupancyNode(t, twoOwnedCards(), []gpu.ComputeProcess{
		{PID: 9001, Name: "python3", PCIeBDF: "0000:3f:00.0", MemoryBytes: 300 << 20},
		{PID: 4242, Name: "python3", PCIeBDF: "0000:40:00.0", MemoryBytes: 8 << 30},
	}, nil)
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{Now: func() time.Time { return now }})
	checked := accelerator.CardIdent("GPU-3f", "0000:3f:00.0")

	end := n.cardSmoke.Begin(checked)
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[0].ForeignProcesses) != 0 {
		t.Fatalf("the card being checked = %+v, want the check's process left out", cards[0].ForeignProcesses)
	}
	if len(cards[1].ForeignProcesses) != 1 {
		t.Fatalf("the other card = %+v, want its foreign process still named", cards[1].ForeignProcesses)
	}

	end()
	now = now.Add(10 * time.Second)
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[0].ForeignProcesses) != 0 {
		t.Fatal("a listing taken just after the check still blamed its process")
	}

	// Past the grace period a process there is somebody's.
	now = now.Add(time.Minute)
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[0].ForeignProcesses) != 1 {
		t.Fatalf("card = %+v, want the process reported once the check is long over", cards[0].ForeignProcesses)
	}
}

// holderRuntime is a runtime that knows of allocations no process list
// shows (the kube runtime: the kubelet gave a card to another pod).
type holderRuntime struct {
	runtime.Driver
	holders map[string][]proto.CardProcess
	err     error
}

func (h *holderRuntime) ExternalCardHolders(context.Context) (map[string][]proto.CardProcess, error) {
	return h.holders, h.err
}

// A card the kubelet gave another pod is the platform's no more than one a
// stray process holds, even before that pod starts using it; a round in
// which the kubelet cannot be asked keeps the last answer.
func TestACardAKubernetesPodHoldsIsReportedAndRestated(t *testing.T) {
	rt := &holderRuntime{Driver: recording.New(filepath.Join(t.TempDir(), "rec.jsonl")),
		holders: map[string][]proto.CardProcess{"GPU-40": {{Name: "Pod ml/train-0"}}}}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), rt, t.TempDir(), "",
		&occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()}}, nil)
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[0].ForeignProcesses) != 0 || len(cards[1].ForeignProcesses) != 1 || cards[1].ForeignProcesses[0].Name != "Pod ml/train-0" {
		t.Fatalf("cards = %+v", cards)
	}
	rt.err, rt.holders = errors.New("kubelet unreachable"), nil
	cards = twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if len(cards[1].ForeignProcesses) != 1 {
		t.Fatalf("a round without the kubelet's answer freed the card: %+v", cards[1])
	}
}
