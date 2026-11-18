package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/pcilink"
	"gpuworks.ai/gpuworks/internal/proto"
)

// pcieHost is one card straight under a root port, whose link and error
// counters a test changes between reports. The root port's config space is
// not readable, so only the card's own link is judged.
type pcieHost struct {
	t    *testing.T
	root string
	card string
}

func newPCIeHost(t *testing.T) *pcieHost {
	t.Helper()
	root := t.TempDir()
	card := filepath.Join(root, "sys/devices/pci0000:00/0000:00:01.0/0000:3f:00.0")
	if err := os.MkdirAll(card, 0o755); err != nil {
		t.Fatal(err)
	}
	bus := filepath.Join(root, "sys/bus/pci/devices")
	if err := os.MkdirAll(bus, 0o755); err != nil {
		t.Fatal(err)
	}
	for bdf, dir := range map[string]string{"0000:00:01.0": filepath.Dir(card), "0000:3f:00.0": card} {
		if err := os.Symlink(dir, filepath.Join(bus, bdf)); err != nil {
			t.Fatal(err)
		}
	}
	h := &pcieHost{t: t, root: root, card: card}
	h.write(filepath.Dir(card), "max_link_speed", "16.0 GT/s PCIe")
	h.write(filepath.Dir(card), "max_link_width", "16")
	h.write(filepath.Dir(card), "current_link_speed", "16.0 GT/s PCIe")
	h.write(filepath.Dir(card), "current_link_width", "16")
	h.write(card, "max_link_speed", "16.0 GT/s PCIe")
	h.write(card, "max_link_width", "16")
	h.link("16.0", "16")
	h.aer(0, 0)
	return h
}

func (h *pcieHost) write(dir, name, value string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *pcieHost) link(speed, width string) {
	h.write(h.card, "current_link_speed", speed+" GT/s PCIe")
	h.write(h.card, "current_link_width", width)
}

func (h *pcieHost) aer(nonFatal, fatal int) {
	h.write(h.card, "aer_dev_correctable", "TOTAL_ERR_COR 0")
	h.write(h.card, "aer_dev_nonfatal", fmt.Sprint("TOTAL_ERR_NONFATAL ", nonFatal))
	h.write(h.card, "aer_dev_fatal", fmt.Sprint("TOTAL_ERR_FATAL ", fatal))
}

func pcieCard() []proto.GPUInfo {
	return []proto.GPUInfo{{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true, HealthStatus: accelerator.HealthOK}}
}

func pcieNode(h *pcieHost, now *time.Time) *NodeRuntime {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, h.t.TempDir(), "none", nil, nil)
	n.pcie.reader = pcilink.Reader{Root: h.root}
	n.pcie.now = func() time.Time { return *now }
	return n
}

var cardIdent = accelerator.CardIdent("GPU-3f", "0000:3f:00.0")

func report(n *NodeRuntime, busy bool, passed func(string, time.Time) bool) proto.GPUInfo {
	cards := pcieCard()
	n.applyPCIeFindings(cards, map[string]bool{cardIdent: busy}, passed)
	return cards[0]
}

// A narrow link is raised after three reports in a row and cleared after
// three good ones; one odd sample changes nothing.
func TestANarrowLinkIsRaisedAndClearedAfterThreeReports(t *testing.T) {
	h := newPCIeHost(t)
	now := time.Now()
	n := pcieNode(h, &now)
	h.link("16.0", "8")
	for i := 1; i <= 2; i++ {
		if c := report(n, false, nil); c.HealthCode != "" {
			t.Fatalf("report %d raised %q too early", i, c.HealthCode)
		}
	}
	c := report(n, false, nil)
	if c.HealthCode != accelerator.HealthPCIeLinkDegraded || c.HealthStatus != accelerator.HealthWarning || !c.Healthy {
		t.Fatalf("third report = %+v, want a schedulable warning", c)
	}
	if want := "PCIe Link 0000:3f:00.0=x8 of x16, 16.0 of 16.0 GT/s"; c.HealthMessage != want {
		t.Fatalf("evidence = %q, want %q", c.HealthMessage, want)
	}
	h.link("16.0", "16")
	for i := 1; i <= 2; i++ {
		if c := report(n, false, nil); c.HealthCode != accelerator.HealthPCIeLinkDegraded {
			t.Fatalf("good report %d cleared it too early", i)
		}
	}
	if c := report(n, false, nil); c.HealthCode != "" {
		t.Fatalf("still %q after three good reports", c.HealthCode)
	}
}

// A unified-memory part's width is its platform's wiring, not a degradation:
// the same narrow fabric link that warns on a discrete card is a fact on a
// Thor (measured on raytone-thor: LnkCap x16, LnkSta x1, constant, the
// parent port reporting no width of its own).
func TestAnOnSoCCardsWidthIsFactNotFinding(t *testing.T) {
	h := newPCIeHost(t)
	now := time.Now()
	n := pcieNode(h, &now)
	h.link("2.5", "1")
	cards := pcieCard()
	cards[0].Name = "NVIDIA Thor"
	for i := 0; i <= pcieRounds; i++ {
		n.applyPCIeFindings(cards, nil, nil)
	}
	if cards[0].HealthCode != "" || !cards[0].Healthy {
		t.Fatalf("an on-SoC card's width judged as degradation: %+v", cards[0])
	}

	h2 := newPCIeHost(t)
	n2 := pcieNode(h2, &now)
	h2.link("2.5", "1")
	discrete := pcieCard()
	discrete[0].Name = "NVIDIA GeForce RTX 5090"
	for i := 0; i < pcieRounds; i++ {
		n2.applyPCIeFindings(discrete, nil, nil)
	}
	if discrete[0].HealthCode != accelerator.HealthPCIeLinkDegraded {
		t.Fatalf("a discrete card's narrow width no longer judged: %+v", discrete[0])
	}
}

// Speed is judged only on a busy card: an idle one is allowed to slow down.
func TestASlowLinkCountsOnlyWhileTheCardIsBusy(t *testing.T) {
	h := newPCIeHost(t)
	now := time.Now()
	n := pcieNode(h, &now)
	h.link("2.5", "16")
	for i := 0; i < 5; i++ {
		if c := report(n, false, nil); c.HealthCode != "" {
			t.Fatalf("an idle card's slow link raised %q", c.HealthCode)
		}
	}
	for i := 0; i < 3; i++ {
		report(n, true, nil)
	}
	if c := report(n, true, nil); c.HealthCode != accelerator.HealthPCIeLinkDegraded {
		t.Fatalf("a busy card's slow link = %q", c.HealthCode)
	}
}

// Error counters count since boot: the first reading is the baseline, new
// non-fatal errors warn for a while, new fatal ones hold until a runtime
// check has passed on the card since.
func TestAERIncreasesRaiseFindings(t *testing.T) {
	h := newPCIeHost(t)
	now := time.Now()
	n := pcieNode(h, &now)
	h.aer(7, 2) // errors from before the agent started
	if c := report(n, false, nil); c.HealthCode != "" {
		t.Fatalf("the baseline raised %q", c.HealthCode)
	}
	h.aer(8, 2)
	if c := report(n, false, nil); c.HealthCode != accelerator.HealthPCIeErrors || c.HealthStatus != accelerator.HealthWarning {
		t.Fatalf("a new non-fatal error = %+v", c)
	}
	now = now.Add(pcieErrorsWarnFor + time.Minute)
	if c := report(n, false, nil); c.HealthCode != "" {
		t.Fatalf("still %q after a quiet period", c.HealthCode)
	}

	h.aer(8, 3)
	fatalAt := now
	var passedAfter time.Time
	passed := func(ident string, t time.Time) bool { return ident == cardIdent && passedAfter.After(t) }
	if c := report(n, false, passed); c.HealthCode != accelerator.HealthPCIeFatal || c.Healthy {
		t.Fatalf("a new fatal error = %+v, want the card stopped", c)
	}
	now = now.Add(time.Hour)
	if c := report(n, false, passed); c.HealthCode != accelerator.HealthPCIeFatal {
		t.Fatal("a fatal error cleared by time alone")
	}
	passedAfter = fatalAt.Add(time.Second)
	if c := report(n, false, passed); c.HealthCode != "" {
		t.Fatalf("still %q after a runtime check passed", c.HealthCode)
	}
}

// A PCIe finding never hides a more severe one the vendor tool reported.
func TestPCIeFindingsYieldToMoreSevereOnes(t *testing.T) {
	h := newPCIeHost(t)
	now := time.Now()
	n := pcieNode(h, &now)
	h.link("16.0", "8")
	for i := 0; i < 3; i++ {
		report(n, false, nil)
	}
	cards := pcieCard()
	cards[0].HealthCode, cards[0].HealthStatus = accelerator.HealthResetRequired, accelerator.HealthError
	n.applyPCIeFindings(cards, nil, nil)
	if cards[0].HealthCode != accelerator.HealthResetRequired {
		t.Fatalf("code = %q", cards[0].HealthCode)
	}
}

// Doctor sees what the reports hold, and asking it moves no count: a
// diagnosis in the middle of three good readings does not clear the finding
// sooner.
func TestDoctorSeesHeldHardwareFindingsWithoutMovingThem(t *testing.T) {
	h := newPCIeHost(t)
	now := time.Now()
	n := pcieNode(h, &now)
	h.link("16.0", "8")
	for i := 0; i < 3; i++ {
		report(n, false, nil)
	}
	h.link("16.0", "16")
	report(n, false, nil)
	for i := 0; i < 5; i++ {
		cards := pcieCard()
		n.applyHeldHardwareFindings(cards)
		if cards[0].HealthCode != accelerator.HealthPCIeLinkDegraded {
			t.Fatalf("held = %q", cards[0].HealthCode)
		}
	}
	if c := report(n, false, nil); c.HealthCode != accelerator.HealthPCIeLinkDegraded {
		t.Fatal("Doctor's reads counted as good reports")
	}

	ctx := withDoctorHeldFindings(context.Background(), n.applyHeldHardwareFindings)
	var r proto.DoctorReport
	doctorAccelerators(ctx, &r, &gpu.FixedProber{VendorName: "nvidia", GPUs: pcieCard()})
	if len(r.Accelerators.GPUs) != 1 || r.Accelerators.GPUs[0].HealthCode != accelerator.HealthPCIeLinkDegraded ||
		len(r.Findings) == 0 || r.Findings[0].Code != accelerator.HealthPCIeLinkDegraded {
		t.Fatalf("doctor = %+v findings=%+v", r.Accelerators, r.Findings)
	}
}
