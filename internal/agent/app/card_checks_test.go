package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/agent/gpu/pcilink"
	"gpuworks.ai/gpuworks/internal/proto"
)

// realNVIDIAProber is a fixture the hardware watches are allowed to look at:
// unlike FixedProber it does not call itself synthetic.
type realNVIDIAProber struct{ *gpu.FixedProber }

func (realNVIDIAProber) Synthetic() bool { return false }

// checksNode is one NVIDIA card on a host whose PCIe path the test controls.
func checksNode(t *testing.T) (*NodeRuntime, *pcieHost, *time.Time) {
	t.Helper()
	h := newPCIeHost(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		realNVIDIAProber{&gpu.FixedProber{VendorName: "nvidia", GPUs: pcieCard()}}, nil)
	n.pcie.reader = pcilink.Reader{Root: h.root}
	n.pcie.now = func() time.Time { return now }
	n.checks.now = func() time.Time { return now }
	return n, h, &now
}

func checksOf(card proto.GPUInfo) map[string]proto.CardCheck {
	out := map[string]proto.CardCheck{}
	for _, c := range card.Checks {
		out[c.ID] = c
	}
	return out
}

func reportChecks(n *NodeRuntime, mutate func(*proto.GPUInfo), cliAnswered bool) proto.GPUInfo {
	cards := pcieCard()
	if mutate != nil {
		mutate(&cards[0])
	}
	n.CardHealthFindings(context.Background(), cards, cliAnswered)
	return cards[0]
}

// Every check of the vendor is reported, in the catalog's order, and each says
// what it is: a check with nothing to read is not applicable, the runtime
// check without a runner says the host cannot be asked, and nothing that did
// not run reads as a pass.
func TestEveryCheckIsReportedInOrderAndNothingUnrunPasses(t *testing.T) {
	n, _, _ := checksNode(t)
	card := reportChecks(n, nil, true)

	var ids []string
	for _, c := range card.Checks {
		ids = append(ids, c.ID)
	}
	want := []string{}
	for _, spec := range accelerator.CardChecks("nvidia") {
		want = append(want, spec.ID)
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("checks = %v, want %v", ids, want)
	}
	got := checksOf(card)
	for id, state := range map[string]string{
		accelerator.CheckVendorHealth: proto.CheckPass,
		accelerator.CheckVendorCLI:    proto.CheckPass,
		accelerator.CheckOccupancy:    proto.CheckNA, // FixedProber lists no processes
		accelerator.CheckPCIeLink:     proto.CheckPass,
		accelerator.CheckPCIeErrors:   proto.CheckPass,
		accelerator.CheckXid:          proto.CheckNA, // no kernel log here
		accelerator.CheckNVLink:       proto.CheckNA,
		accelerator.CheckRuntime:      proto.CheckSkip,
	} {
		if got[id].Status != state {
			t.Errorf("%s = %+v, want %s", id, got[id], state)
		}
	}
	if got[accelerator.CheckRuntime].Code != proto.CardCheckSkipUnavailable {
		t.Errorf("runtime = %+v, want skipped as unavailable", got[accelerator.CheckRuntime])
	}
}

// A card that exists only in a fixture is never asked by the hardware watches
// or the runtime check; those are not applicable rather than pending forever.
func TestASyntheticCardHasNoHardwareOrRuntimeChecks(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia", GPUs: pcieCard()}, nil)
	cards := pcieCard()
	n.CardHealthFindings(context.Background(), cards, true)
	got := checksOf(cards[0])
	for _, id := range []string{accelerator.CheckPCIeLink, accelerator.CheckXid, accelerator.CheckRuntime} {
		if got[id].Status != proto.CheckNA {
			t.Errorf("%s = %+v, want not applicable", id, got[id])
		}
	}
	if got[accelerator.CheckVendorHealth].Status != proto.CheckPass {
		t.Errorf("vendor health = %+v", got[accelerator.CheckVendorHealth])
	}
}

// A problem the vendor tool reported holds until three clean readings in a
// row: the card stays unschedulable meanwhile and says how far it has got.
func TestAVendorProblemHoldsUntilThreeCleanReadings(t *testing.T) {
	n, _, _ := checksNode(t)
	bad := func(c *proto.GPUInfo) {
		gpu.SetCardFinding(c, accelerator.HealthMemoryUncorrectable, []accelerator.HealthField{{Name: "ecc.errors.uncorrected.volatile.total", Value: "3"}})
	}
	card := reportChecks(n, bad, true)
	if c := checksOf(card)[accelerator.CheckVendorHealth]; c.Status != proto.CheckFail || c.Code != accelerator.HealthMemoryUncorrectable ||
		len(c.Evidence) != 1 || c.Evidence[0].Value != "3" {
		t.Fatalf("bad reading = %+v", c)
	}
	if c := checksOf(card)[accelerator.CheckVendorCLI]; len(c.Evidence) != 0 {
		t.Fatalf("a passing check carries evidence: %+v", c)
	}
	for i := 1; i < vendorReleaseRounds; i++ {
		card = reportChecks(n, nil, true)
		if card.HealthCode != accelerator.HealthMemoryUncorrectable || card.Healthy {
			t.Fatalf("clean reading %d released the card: %+v", i, card)
		}
		if c := checksOf(card)[accelerator.CheckVendorHealth]; c.Status != proto.CheckFail {
			t.Fatalf("clean reading %d: check = %+v", i, c)
		}
		if !strings.Contains(card.HealthMessage, "Recovery") {
			t.Fatalf("clean reading %d: evidence %q does not say it is recovering", i, card.HealthMessage)
		}
	}
	card = reportChecks(n, nil, true)
	if card.HealthCode != "" || checksOf(card)[accelerator.CheckVendorHealth].Status != proto.CheckPass {
		t.Fatalf("after %d clean readings = %+v", vendorReleaseRounds, card)
	}

	// The problem is kept for whoever asks, after the card recovered.
	detail := n.checkDetail(cardIdent, accelerator.CheckVendorHealth)
	if detail.Last == nil || detail.Last.Status != proto.CheckPass {
		t.Fatalf("last = %+v", detail.Last)
	}
	if detail.LastProblem == nil || detail.LastProblem.Code != accelerator.HealthMemoryUncorrectable ||
		len(detail.LastProblem.Evidence) == 0 || detail.LastProblem.Probe == "" {
		t.Fatalf("last problem = %+v", detail.LastProblem)
	}
}

// A vendor tool that did not answer fails its own check and leaves the vendor
// verdict unknown, not passed.
func TestAnUnansweredVendorToolIsItsOwnFailure(t *testing.T) {
	n, _, _ := checksNode(t)
	card := reportChecks(n, func(c *proto.GPUInfo) { gpu.SetCardFinding(c, accelerator.HealthCLIUnanswered, nil) }, false)
	got := checksOf(card)
	if got[accelerator.CheckVendorCLI].Status != proto.CheckFail || got[accelerator.CheckVendorCLI].Code != accelerator.HealthCLIUnanswered {
		t.Fatalf("vendor cli = %+v", got[accelerator.CheckVendorCLI])
	}
	if got[accelerator.CheckVendorHealth].Status != proto.CheckSkip || got[accelerator.CheckVendorHealth].Code != proto.CardCheckSkipProbeFailed {
		t.Fatalf("vendor health = %+v", got[accelerator.CheckVendorHealth])
	}
}

// The link and the error counters are two checks: a narrow link warns on its
// own line while the counters pass, though the card carries one code.
func TestPCIeLinkAndErrorsAreReportedApart(t *testing.T) {
	n, h, _ := checksNode(t)
	h.link("16.0", "8")
	var card proto.GPUInfo
	for i := 0; i < pcieRounds; i++ {
		card = reportChecks(n, nil, true)
	}
	got := checksOf(card)
	if got[accelerator.CheckPCIeLink].Status != proto.CheckWarn || got[accelerator.CheckPCIeLink].Code != accelerator.HealthPCIeLinkDegraded {
		t.Fatalf("link = %+v", got[accelerator.CheckPCIeLink])
	}
	if got[accelerator.CheckPCIeErrors].Status != proto.CheckPass {
		t.Fatalf("errors = %+v", got[accelerator.CheckPCIeErrors])
	}
	detail := n.checkDetail(cardIdent, accelerator.CheckPCIeLink)
	if detail.Last == nil || len(detail.Last.Evidence) == 0 || !strings.Contains(detail.Last.Probe, "lspci") {
		t.Fatalf("link detail = %+v", detail.Last)
	}
}

// Somebody else's process warns the occupancy check with who it is.
func TestOccupancyWarnsWithTheProcesses(t *testing.T) {
	n := occupancyNode(t, twoOwnedCards(), []gpu.ComputeProcess{{PID: 4242, Name: "python3", PCIeBDF: "0000:40:00.0", MemoryBytes: 2 << 30}}, nil)
	cards := twoOwnedCards()
	n.CardHealthFindings(context.Background(), cards, true)
	if c := checksOf(cards[0])[accelerator.CheckOccupancy]; c.Status != proto.CheckPass {
		t.Fatalf("free card = %+v", c)
	}
	c := checksOf(cards[1])[accelerator.CheckOccupancy]
	if c.Status != proto.CheckWarn || c.Code != proto.CardCheckForeignProcesses {
		t.Fatalf("taken card = %+v", c)
	}
	detail := n.checkDetail(accelerator.CardIdent("GPU-40", "0000:40:00.0"), accelerator.CheckOccupancy)
	if detail.Last == nil || len(detail.Last.Evidence) != 1 || detail.Last.Evidence[0].Name != "PID 4242" ||
		detail.Last.Evidence[0].Value != "python3, 2048 MiB" {
		t.Fatalf("detail = %+v", detail.Last)
	}
}

// The runtime check's state follows the runner: pending until it answers, the
// answer with its age and duration, a skip when a later round left the card
// alone, and a failure that stands through its backoff.
func TestTheRuntimeCheckFollowsTheRunner(t *testing.T) {
	n, _, now := checksNode(t)
	var busy bool
	fail := error(nil)
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: func(context.Context) []cardsmoke.Card { return []cardsmoke.Card{{Ident: cardIdent, Visible: "GPU-3f"}} },
		Try:   func(context.Context, cardsmoke.Card) error { return fail },
		Busy:  func(cardsmoke.Card) bool { return busy },
		Now:   func() time.Time { return *now },
	})
	if c := checksOf(reportChecks(n, nil, true))[accelerator.CheckRuntime]; c.Status != proto.CheckSkip || c.Code != proto.CardCheckNotRunYet {
		t.Fatalf("before any round = %+v", c)
	}

	n.cardSmoke.RunOnce(context.Background())
	*now = now.Add(2 * time.Minute)
	c := checksOf(reportChecks(n, nil, true))[accelerator.CheckRuntime]
	if c.Status != proto.CheckPass || c.AgeS != 120 {
		t.Fatalf("after a pass = %+v, want a pass two minutes old", c)
	}

	busy = true
	*now = now.Add(15 * time.Minute)
	n.cardSmoke.RunOnce(context.Background())
	if c := checksOf(reportChecks(n, nil, true))[accelerator.CheckRuntime]; c.Status != proto.CheckSkip || c.Code != proto.CardCheckSkipBusy {
		t.Fatalf("serving card = %+v", c)
	}
	detail := n.checkDetail(cardIdent, accelerator.CheckRuntime)
	if detail.Last == nil || detail.Last.Status != proto.CheckSkip || detail.LastRan == nil || detail.LastRan.Status != proto.CheckPass {
		t.Fatalf("detail = %+v, want the skip and the pass before it", detail)
	}
	if !strings.Contains(detail.LastRan.Probe, "CUDA_VISIBLE_DEVICES='GPU-3f'") {
		t.Errorf("probe = %q", detail.LastRan.Probe)
	}

	busy, fail = false, errors.New("cuda runtime smoke failed: cuCtxCreate=999")
	*now = now.Add(15 * time.Minute)
	n.cardSmoke.RunOnce(context.Background())
	*now = now.Add(15 * time.Minute)
	n.cardSmoke.RunOnce(context.Background()) // inside the backoff: not asked
	card := reportChecks(n, nil, true)
	if c := checksOf(card)[accelerator.CheckRuntime]; c.Status != proto.CheckFail || c.Code != accelerator.HealthRuntimeUnusable {
		t.Fatalf("failing card = %+v", c)
	}
	if card.HealthCode != accelerator.HealthRuntimeUnusable {
		t.Fatalf("card = %+v", card)
	}
}

// Rechecking a card runs the check now, keeps its answer for whoever asks and
// records it with the runner, so the card's state follows at the next report.
func TestRecheckingACardRecordsItsAnswer(t *testing.T) {
	n, _, now := checksNode(t)
	fail := errors.New("cuda runtime smoke failed: cuCtxCreate=999")
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: func(context.Context) []cardsmoke.Card { return []cardsmoke.Card{{Ident: cardIdent, Visible: "GPU-3f"}} },
		Try:   func(context.Context, cardsmoke.Card) error { return fail },
		Now:   func() time.Time { return *now },
	})
	n.cardSmoke.RunOnce(context.Background())
	if c := checksOf(reportChecks(n, nil, true))[accelerator.CheckRuntime]; c.Status != proto.CheckFail {
		t.Fatalf("before the recheck = %+v", c)
	}

	fail = nil
	*now = now.Add(time.Minute)
	detail, err := n.recheckCard(context.Background(), cardIdent)
	if err != nil || detail.Last == nil || detail.Last.Status != proto.CheckPass || detail.LastProblem == nil {
		t.Fatalf("recheck = %+v, %v", detail, err)
	}
	card := reportChecks(n, nil, true)
	if c := checksOf(card)[accelerator.CheckRuntime]; c.Status != proto.CheckPass || card.HealthCode != "" {
		t.Fatalf("after the recheck = %+v, card %+v", c, card)
	}

	if _, err := n.recheckCard(context.Background(), "GPU-unknown"); err == nil {
		t.Fatal("a card this node never listed was rechecked")
	}
}

// Asking for a check's detail runs nothing and answers for an unknown card
// with no runs rather than an error.
func TestADetailOfACardNeverSeenHasNoRuns(t *testing.T) {
	n, _, _ := checksNode(t)
	detail := n.checkDetail("GPU-none", accelerator.CheckRuntime)
	if detail.Last != nil || detail.AgentStartedAt == "" {
		t.Fatalf("detail = %+v", detail)
	}
}

// Evidence is cut to the report's bound at a field's edge.
func TestEvidenceIsBounded(t *testing.T) {
	var fields []accelerator.HealthField
	for i := 0; i < 100; i++ {
		fields = append(fields, accelerator.HealthField{Name: "field", Value: strings.Repeat("x", 40)})
	}
	got := boundedEvidence(fields)
	size := 0
	for _, f := range got {
		size += len(f.Name) + len(f.Value) + 2
	}
	if len(got) == 0 || len(got) == len(fields) || size > proto.CardCheckEvidenceBytes {
		t.Fatalf("kept %d fields, %d bytes", len(got), size)
	}
}
