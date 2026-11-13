package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// holdingKubeDriver is the kube runtime as the card check sees it: it holds
// some cards, refuses the rest, and counts the holds that are open.
type holdingKubeDriver struct {
	kubeDriver
	mu   sync.Mutex
	held map[string]bool
	open int
}

func (d *holdingKubeDriver) HoldCardForCheck(_ context.Context, card string) (func(), string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.held[card] {
		return nil, "Kubernetes may give " + card + " to another pod"
	}
	d.open++
	return func() { d.mu.Lock(); d.open--; d.mu.Unlock() }, ""
}

func (d *holdingKubeDriver) holds() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.open
}

// On a Kubernetes node the per-card runtime check asks only the cards the
// platform holds (and keeps them held while asking), reports the others as
// skipped with a reason of their own, and never takes the container route,
// which would create a pod and borrow a card to ask it.
func TestKubeNodeAsksOnlyTheCardsThePlatformHolds(t *testing.T) {
	cards := []proto.GPUInfo{
		{Index: 0, UUID: "ascend-0", Vendor: "ascend", PCIeBDF: "0000:95:00.0", Healthy: true},
		{Index: 1, UUID: "ascend-1", Vendor: "ascend", PCIeBDF: "0000:96:00.0", Healthy: true},
		{Index: 2, UUID: "ascend-2", Vendor: "ascend", PCIeBDF: "0000:97:00.0", Healthy: true},
	}
	driver := &holdingKubeDriver{kubeDriver: kubeDriver{recording.New(filepath.Join(t.TempDir(), "rec.jsonl"))}, held: map[string]bool{"ascend-1": true}}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), driver, filepath.Join(t.TempDir(), "models"), "kube",
		&gpu.FixedProber{VendorName: "ascend", GPUs: cards}, nil, engine.WithNetworkInterfaceResolver(nil))

	var asked []string
	var heldWhileAsking []int
	old := doctorHostRuntimeCheck
	t.Cleanup(func() { doctorHostRuntimeCheck = old })
	doctorHostRuntimeCheck = func(vendor string) (gpu.HostRuntimeCheck, bool) {
		check, ok := old(vendor)
		check.Prerequisite = func() string { return "" }
		check.Try = func(_ context.Context, visible string) error {
			asked = append(asked, visible)
			heldWhileAsking = append(heldWhileAsking, driver.holds())
			return nil
		}
		return check, ok
	}

	check, ok := n.cardRuntimeCheck("ascend")
	if !ok {
		t.Fatal("a Kubernetes node has no runtime check")
	}
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: func(ctx context.Context) []cardsmoke.Card { return n.smokeCards(ctx, check) },
		Try:   cardSmokeAttempt(check),
	})
	n.cardSmoke.RunOnce(context.Background())

	if len(asked) != 1 || asked[0] != "1" || heldWhileAsking[0] != 1 || driver.holds() != 0 {
		t.Fatalf("cards asked = %v (holds open while asking %v, after %d); want only die 1, held while asked and released after", asked, heldWhileAsking, driver.holds())
	}
	skips, outcomes := n.cardSmoke.Skips(), n.cardSmoke.Outcomes()
	for _, ident := range []string{"pci:0000:95:00.0", "pci:0000:97:00.0"} {
		if skips[ident].Reason != cardsmoke.SkipNotHeld {
			t.Fatalf("skip of %s = %+v", ident, skips[ident])
		}
		if _, answered := outcomes[ident]; answered {
			t.Fatalf("%s has an outcome though it was never asked", ident)
		}
	}
	if !outcomes["pci:0000:96:00.0"].OK || len(skips) != 2 {
		t.Fatalf("outcomes %+v skips %+v", outcomes, skips)
	}
	if got := runtimeSkipReason(cardsmoke.SkipNotHeld); got != accelerator.CheckSkipNotHeld {
		t.Fatalf("the check catalog's word for it = %q", got)
	}

	// A recheck of a card Kubernetes owns says so too, and still asks nobody.
	if _, reason := n.cardSmoke.CheckCard(context.Background(), "pci:0000:97:00.0"); reason != cardsmoke.SkipNotHeld || len(asked) != 1 {
		t.Fatalf("recheck of a card that is not held: reason %q, asked %v", reason, asked)
	}
}

// A vendor whose only check is the container one (PPU) has none on a
// Kubernetes node: asking would create a pod and borrow a card.
func TestKubeNodeHasNoContainerCardCheck(t *testing.T) {
	driver := &holdingKubeDriver{kubeDriver: kubeDriver{recording.New(filepath.Join(t.TempDir(), "rec.jsonl"))}}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), driver, filepath.Join(t.TempDir(), "models"), "kube",
		&gpu.FixedProber{VendorName: "ppu"}, nil, engine.WithNetworkInterfaceResolver(nil))
	if _, framework := accelerator.ContainerSmokeFramework("ppu"); !framework {
		t.Skip("PPU has no container check to refuse")
	}
	old := doctorHostRuntimeCheck
	t.Cleanup(func() { doctorHostRuntimeCheck = old })
	doctorHostRuntimeCheck = func(string) (gpu.HostRuntimeCheck, bool) { return gpu.HostRuntimeCheck{}, false }
	if check, ok := n.cardRuntimeCheck("ppu"); ok {
		t.Fatalf("a Kubernetes node got a runtime check for PPU: %+v", check)
	}
}

// Doctor does not ask a card Kubernetes owns either: it lists it as not
// checked, with the reason, and goes on to the next card.
func TestDoctorLeavesACardThePlatformDoesNotHoldUnchecked(t *testing.T) {
	var asked []string
	run := doctorDieRun{unit: "die", check: gpu.HostRuntimeCheck{Timeout: 5e9, Try: func(_ context.Context, visible string) error {
		asked = append(asked, visible)
		if visible == "0" {
			return cardsmoke.NotHeld("Kubernetes may give card ascend-0 to another pod")
		}
		return nil
	}}}
	dies := []doctorDie{
		{gpu: proto.GPUInfo{Index: 0, UUID: "ascend-0", PCIeBDF: "0000:95:00.0"}, label: "0", visible: "0"},
		{gpu: proto.GPUInfo{Index: 1, UUID: "ascend-1", PCIeBDF: "0000:96:00.0"}, label: "1", visible: "1"},
	}
	run.ask(context.Background(), dies)
	if len(asked) != 2 || len(run.answered) != 1 || run.answered[0].label != "1" || len(run.failed) != 0 {
		t.Fatalf("asked %v answered %+v failed %+v", asked, run.answered, run.failed)
	}
	if len(run.notChecked) != 1 || run.notChecked[0].die.label != "0" || run.notChecked[0].reason != "Kubernetes may give card ascend-0 to another pod" {
		t.Fatalf("notChecked = %+v", run.notChecked)
	}
}
