package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/proto"
)

func hostCheck(t *testing.T, vendor string) gpu.HostRuntimeCheck {
	t.Helper()
	check, ok := gpu.HostRuntimeCheckFor(vendor)
	if !ok {
		t.Fatalf("vendor %q has no host runtime check", vendor)
	}
	return check
}

// smokeNode is a node runtime whose card check answers as told.
func smokeNode(t *testing.T, cards []proto.GPUInfo, fail map[string]error) *NodeRuntime {
	t.Helper()
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cards}, nil)
	check := hostCheck(t, "nvidia")
	answering := check
	answering.Try = func(_ context.Context, visible string) error { return fail[visible] }
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: func(ctx context.Context) []cardsmoke.Card { return n.smokeCards(ctx, check) },
		Try:   cardSmokeAttempt(answering),
	})
	n.cardSmoke.RunOnce(context.Background())
	return n
}

func cardsOfOneHealthyNode() []proto.GPUInfo {
	return []proto.GPUInfo{
		{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true},
		{Index: 1, UUID: "GPU-40", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", Healthy: true},
	}
}

// A card the vendor tool calls healthy and the runtime cannot use is not
// healthy, and the report says which card and why.
func TestARuntimeFailureMakesTheCardUnusable(t *testing.T) {
	n := smokeNode(t, cardsOfOneHealthyNode(), map[string]error{"GPU-40": errors.New("cuInit=101")})
	cards := cardsOfOneHealthyNode()
	n.CardHealthFindings(context.Background(), cards, true)

	if cards[0].HealthCode != "" || !cards[0].Healthy {
		t.Fatalf("the card that answered = %+v, want it untouched", cards[0])
	}
	bad := cards[1]
	if bad.HealthCode != accelerator.HealthRuntimeUnusable || bad.Healthy || bad.HealthStatus != accelerator.HealthError {
		t.Fatalf("the card the runtime cannot use = %+v", bad)
	}
	if !strings.Contains(bad.HealthMessage, "cuInit=101") {
		t.Fatalf("the finding does not carry what the runtime said: %q", bad.HealthMessage)
	}
}

// A check that never ran is not a card that failed.
func TestACardNeverCheckedKeepsItsVerdict(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cardsOfOneHealthyNode()}, nil)
	cards := cardsOfOneHealthyNode()
	n.CardHealthFindings(context.Background(), cards, true)
	for _, card := range cards {
		if card.HealthCode != "" || !card.Healthy {
			t.Fatalf("card = %+v, want it untouched when nothing was checked", card)
		}
	}

	n2 := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cardsOfOneHealthyNode()}, nil)
	check := hostCheck(t, "nvidia")
	n2.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards:       func(ctx context.Context) []cardsmoke.Card { return n2.smokeCards(ctx, check) },
		Try:         func(context.Context, cardsmoke.Card) error { return nil },
		Unavailable: func() string { return "python3 is not installed" },
	})
	n2.cardSmoke.RunOnce(context.Background())
	cards = cardsOfOneHealthyNode()
	n2.CardHealthFindings(context.Background(), cards, true)
	for _, card := range cards {
		if card.HealthCode != "" || !card.Healthy {
			t.Fatalf("card = %+v, want it untouched when the check could not run", card)
		}
	}
}

// The vendor tool's own finding names the cause more precisely, so it is
// not replaced by the runtime's symptom.
func TestAVendorFindingOutranksTheRuntimeSymptom(t *testing.T) {
	cards := []proto.GPUInfo{{
		Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0",
		Healthy: false, HealthStatus: accelerator.HealthError, HealthCode: accelerator.HealthMemoryRemapFailed,
		HealthMessage: "remapped_rows.failure=1",
	}}
	n := smokeNode(t, cards, map[string]error{"GPU-3f": errors.New("cuInit=101")})
	n.CardHealthFindings(context.Background(), cards, true)
	if cards[0].HealthCode != accelerator.HealthMemoryRemapFailed {
		t.Fatalf("health code = %q, want the vendor tool's more specific finding", cards[0].HealthCode)
	}
	if !strings.Contains(cards[0].HealthMessage, "remapped_rows.failure=1") {
		t.Fatalf("the vendor evidence was replaced: %q", cards[0].HealthMessage)
	}
}

func TestOnlyTheNodesOwnNamedCardsAreAsked(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: []proto.GPUInfo{
			{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true},
			{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
			{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1a:00.0", Ownership: accelerator.CardWithoutDriver},
			{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1c:00.0", HealthCode: accelerator.HealthDriverInitFailed},
		}}, nil)
	asked := n.smokeCards(context.Background(), hostCheck(t, "nvidia"))
	if len(asked) != 1 || asked[0].Visible != "GPU-3f" || asked[0].Ident != "pci:0000:3f:00.0" {
		t.Fatalf("cards to check = %+v, want only the node's own, named by UUID", asked)
	}

	ascend := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "ascend", GPUs: []proto.GPUInfo{
			{Index: 14, UUID: "ascend-14", Vendor: "ascend", PCIeBDF: "0000:95:00.0", Healthy: true},
		}}, nil)
	asked = ascend.smokeCards(context.Background(), hostCheck(t, "ascend"))
	if len(asked) != 1 || asked[0].Visible != "14" || asked[0].Ident != "pci:0000:95:00.0" {
		t.Fatalf("Ascend cards to check = %+v, want the physical die identifier", asked)
	}
}

// A hermetic prober describes cards that do not exist; asking the host's
// own runtime about them would test the host instead of the fixture.
func TestASyntheticProberIsNeverChecked(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cardsOfOneHealthyNode()}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.startCardSmoke(ctx)
	if n.cardSmoke != nil {
		t.Fatal("a synthetic prober started a real runtime check")
	}
}

// A card running one of this platform's instances has answered the
// question by serving, so the runtime check leaves it alone.
func TestACardServingAnInstanceIsNotChecked(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cardsOfOneHealthyNode()}, nil)
	n.heldCards = func() engine.HeldCards {
		return engine.HeldCards{Idents: []string{accelerator.CardIdent("GPU-40", "0000:40:00.0")}}
	}

	var asked []string
	check := hostCheck(t, "nvidia")
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: func(ctx context.Context) []cardsmoke.Card { return n.smokeCards(ctx, check) },
		Try: func(_ context.Context, card cardsmoke.Card) error {
			asked = append(asked, card.Visible)
			return nil
		},
		Busy: func(card cardsmoke.Card) bool { ours, _, _ := n.cardsInUse(); return ours[card.Ident] > 0 },
	})
	n.cardSmoke.RunOnce(context.Background())
	if len(asked) != 1 || asked[0] != "GPU-3f" {
		t.Fatalf("checked %v, want only the idle card", asked)
	}

	n.heldCards = func() engine.HeldCards { return engine.HeldCards{} }
	asked = nil
	n.cardSmoke.RunOnce(context.Background())
	if len(asked) != 2 {
		t.Fatalf("checked %v, want both cards once nothing of ours runs", asked)
	}
}

func TestCUDASmokePrerequisiteNamesWhatIsMissing(t *testing.T) {
	reason := gpu.CUDASmokePrerequisite()
	if reason != "" && !strings.Contains(reason, "python3") && !strings.Contains(reason, "libcuda") {
		t.Fatalf("reason = %q, want it to name what is missing", reason)
	}
	_ = time.Now
}

func TestSmokeUnavailableNamesWhyNothingCanBeChecked(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cardsOfOneHealthyNode()}, nil)
	reason := smokeUnavailable(hostCheck(t, n.prober.Vendor()))
	if reason != "" && !strings.Contains(reason, "python3") && !strings.Contains(reason, "libcuda") && !strings.Contains(reason, "driver") {
		t.Fatalf("reason = %q, want it to name what is missing", reason)
	}
}

func TestCardSmokeNeverStartsForSyntheticHardware(t *testing.T) {
	for _, tc := range []struct {
		vendor string
		want   bool
		why    string
	}{
		{"nvidia", false, "a synthetic prober describes cards that do not exist"},
		{"ascend", false, "a synthetic prober describes cards that do not exist"},
		{"hygon", false, "a synthetic prober describes cards that do not exist"},
		{"", false, "a node with no accelerator"},
	} {
		n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
			&gpu.FixedProber{VendorName: tc.vendor, GPUs: cardsOfOneHealthyNode()}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		n.startCardSmoke(ctx)
		started := n.cardSmoke != nil
		cancel()
		if started != tc.want {
			t.Errorf("%s: started = %v, want %v (%s)", tc.vendor, started, tc.want, tc.why)
		}
	}
}

func TestAscendHostRuntimeFailureDoesNotBlameTheFirstDie(t *testing.T) {
	cards := []proto.GPUInfo{
		{Index: 14, UUID: "ascend-14", Vendor: "ascend", PCIeBDF: "0000:95:00.0", Healthy: true},
		{Index: 15, UUID: "ascend-15", Vendor: "ascend", PCIeBDF: "0000:96:00.0", Healthy: true},
	}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "ascend", GPUs: cards}, nil)
	check := hostCheck(t, "ascend")
	var attempted []string
	check.Try = func(_ context.Context, visible string) error {
		attempted = append(attempted, visible)
		return &gpu.HostRuntimeUnavailableError{Reason: "the Ascend ACL runtime could not be used on this host: aclInit=100039"}
	}
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: func(ctx context.Context) []cardsmoke.Card { return n.smokeCards(ctx, check) },
		Try:   cardSmokeAttempt(check),
	})
	n.cardSmoke.RunOnce(context.Background())
	reported := append([]proto.GPUInfo(nil), cards...)
	n.CardHealthFindings(context.Background(), reported, true)

	if len(attempted) != 1 || attempted[0] != "14" {
		t.Fatalf("attempts = %v, want only the first die before the host-level stop", attempted)
	}
	for _, card := range reported {
		if card.HealthCode != "" || !card.Healthy {
			t.Fatalf("card = %+v, want every die left to the vendor tool's verdict", card)
		}
	}
	if got := n.cardSmoke.Skipped(); !strings.Contains(got, "aclInit=100039") {
		t.Fatalf("skipped = %q, want the reason no die could be checked", got)
	}
	if outcomes := n.cardSmoke.Outcomes(); len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want nothing recorded against a die", outcomes)
	}
}

// An instance that could not start makes its cards answer, even though the
// agent still holds them for it: a failed start is not serving work, and
// without this the card it failed on stays schedulable and takes the next
// attempt. 192.0.2.129 looped this way on its one unusable die.
func TestAFailedInstanceMakesItsCardsAnswer(t *testing.T) {
	cards := cardsOfOneHealthyNode()
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: cards}, nil)
	bad := accelerator.CardIdent("GPU-40", "0000:40:00.0")
	// Held for the instance, as the agent holds a card it placed work on.
	n.heldCards = func() engine.HeldCards { return engine.HeldCards{Idents: []string{bad}} }
	n.instanceCardIdents = func(uuid string) []string {
		if uuid == "inst-1" {
			return []string{bad}
		}
		return nil
	}
	check := hostCheck(t, "nvidia")
	answering := check
	answering.Try = func(_ context.Context, visible string) error {
		if visible == "GPU-40" {
			return errors.New("cuInit=101")
		}
		return nil
	}
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards:    func(ctx context.Context) []cardsmoke.Card { return n.smokeCards(ctx, check) },
		Try:      cardSmokeAttempt(answering),
		Busy:     func(card cardsmoke.Card) bool { ours, _, _ := n.cardsInUse(); return ours[card.Ident] > 0 },
		Interval: time.Hour,
	})
	n.cardSmokeCtx = context.Background()

	// The schedule leaves the held card alone, so nothing is known about it.
	n.cardSmoke.RunOnce(context.Background())
	if _, asked := n.cardSmoke.Outcomes()[bad]; asked {
		t.Fatal("a round asked a card an instance holds")
	}

	n.reportInstance(proto.InstanceStatus{UUID: "inst-1", State: proto.InstFailed, Message: "container exited (code 1)"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		outcome, ok := n.cardSmoke.Outcomes()[bad]
		if ok && !outcome.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the card the instance failed on was never asked: %+v", n.cardSmoke.Outcomes())
		}
		time.Sleep(5 * time.Millisecond)
	}
	report := cardsOfOneHealthyNode()
	n.CardHealthFindings(context.Background(), report, true)
	if report[1].HealthCode != accelerator.HealthRuntimeUnusable || report[1].Healthy {
		t.Fatalf("the failed card = %+v, want it reported unusable and unschedulable", report[1])
	}
	if report[0].HealthCode != "" {
		t.Fatalf("the other card = %+v, want it untouched", report[0])
	}
}
