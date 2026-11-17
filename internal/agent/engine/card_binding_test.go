package engine

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

func nvidiaCard(index int, bdf string) proto.GPUInfo {
	return proto.GPUInfo{Index: index, UUID: "GPU-" + bdf, Vendor: "nvidia", PCIeBDF: bdf, Healthy: true}
}

func engineWithCards(gpus ...proto.GPUInfo) *Engine {
	return &Engine{prober: &gpu.FixedProber{VendorName: "nvidia", GPUs: gpus}}
}

// The cards an instance was placed on now carry different device indexes
// (one card failed to initialize and dropped out of the vendor CLI). The
// launch must follow the identity, not the index.
func TestResolveBoundCardsFollowsTheCardsToTheirCurrentIndexes(t *testing.T) {
	e := engineWithCards(nvidiaCard(0, "0000:1c:00.0"), nvidiaCard(1, "0000:3f:00.0"))
	spec := proto.InstanceDesiredState{
		UUID:       "inst-1",
		GPUIndexes: []int{1, 2},
		GPUIdents:  []string{"pci:0000:1c:00.0", "pci:0000:3f:00.0"},
	}
	if err := e.resolveBoundCards(context.Background(), &spec); err != nil {
		t.Fatalf("resolve = %v", err)
	}
	if len(spec.GPUIndexes) != 2 || spec.GPUIndexes[0] != 0 || spec.GPUIndexes[1] != 1 {
		t.Fatalf("indexes = %v, want [0 1]", spec.GPUIndexes)
	}
}

// Starting on whichever card now holds that index is exactly the failure
// this exists to prevent, so a missing card fails the launch and the message
// names what the node does have.
func TestResolveBoundCardsRefusesToStartWhenTheCardIsGone(t *testing.T) {
	e := engineWithCards(nvidiaCard(0, "0000:3f:00.0"))
	spec := proto.InstanceDesiredState{
		UUID:       "inst-2",
		GPUIndexes: []int{0},
		GPUIdents:  []string{"pci:0000:1c:00.0"},
	}
	err := e.resolveBoundCards(context.Background(), &spec)
	if err == nil {
		t.Fatal("a launch on an absent card was allowed")
	}
	for _, want := range []string{"pci:0000:1c:00.0", "device 0 = pci:0000:3f:00.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if len(spec.GPUIndexes) != 1 || spec.GPUIndexes[0] != 0 {
		t.Fatalf("a refused resolve rewrote the spec: %v", spec.GPUIndexes)
	}
}

// An old server sends no identities, and a prober may report cards with
// neither a PCI address nor a UUID. Both keep the indexes they were given.
func TestResolveBoundCardsLeavesUnboundSpecsAlone(t *testing.T) {
	e := engineWithCards(nvidiaCard(0, "0000:1c:00.0"))
	spec := proto.InstanceDesiredState{UUID: "inst-3", GPUIndexes: []int{7}}
	if err := e.resolveBoundCards(context.Background(), &spec); err != nil || len(spec.GPUIndexes) != 1 || spec.GPUIndexes[0] != 7 {
		t.Fatalf("spec without identities: err=%v indexes=%v", err, spec.GPUIndexes)
	}

	// A count mismatch is not something a current server sends; the indexes
	// remain the only complete statement of the selection.
	mismatch := proto.InstanceDesiredState{UUID: "inst-4", GPUIndexes: []int{3, 4}, GPUIdents: []string{"pci:0000:1c:00.0"}}
	if err := e.resolveBoundCards(context.Background(), &mismatch); err != nil {
		t.Fatalf("mismatched lengths = %v", err)
	}
	if len(mismatch.GPUIndexes) != 2 || mismatch.GPUIndexes[0] != 3 {
		t.Fatalf("mismatched lengths rewrote the spec: %v", mismatch.GPUIndexes)
	}

	// Without a prober there is nothing to resolve against.
	bare := &Engine{}
	noProber := proto.InstanceDesiredState{UUID: "inst-5", GPUIndexes: []int{2}, GPUIdents: []string{"pci:0000:1c:00.0"}}
	if err := bare.resolveBoundCards(context.Background(), &noProber); err != nil || noProber.GPUIndexes[0] != 2 {
		t.Fatalf("no prober: err=%v indexes=%v", err, noProber.GPUIndexes)
	}
}

// A probe that cannot enumerate the cards says nothing about where they are;
// the launch proceeds and fails later with the runtime's own error if the
// card really is gone, rather than being blocked by an unreadable CLI.
func TestResolveBoundCardsProceedsWhenTheProbeFails(t *testing.T) {
	e := &Engine{prober: &failingProber{}}
	spec := proto.InstanceDesiredState{UUID: "inst-6", GPUIndexes: []int{1}, GPUIdents: []string{"pci:0000:1c:00.0"}}
	if err := e.resolveBoundCards(context.Background(), &spec); err != nil {
		t.Fatalf("resolve = %v", err)
	}
	if len(spec.GPUIndexes) != 1 || spec.GPUIndexes[0] != 1 {
		t.Fatalf("indexes = %v, want the spec's own", spec.GPUIndexes)
	}
}

type failingProber struct{ gpu.FixedProber }

func (*failingProber) Probe(context.Context) ([]proto.GPUInfo, error) {
	return nil, context.DeadlineExceeded
}

// A card the vendor CLI did not list has no device to launch against.
// Resolving a binding to it started the container on device -1, which is
// the case this function's own doc says must fail the launch instead.
func TestABindingToAnUnlistedCardFailsRatherThanResolvingToMinusOne(t *testing.T) {
	e := &Engine{prober: &gpu.FixedProber{VendorName: "nvidia", GPUs: []proto.GPUInfo{
		{Index: -1, Name: "GA102", PCIeBDF: "0000:1c:00.0"}, // driver could not bring it up
		{Index: 0, UUID: "GPU-3f", Name: "A100", PCIeBDF: "0000:3f:00.0"},
	}}}
	spec := proto.InstanceDesiredState{
		UUID: "i-1", GPUIdents: []string{"pci:0000:1c:00.0"}, GPUIndexes: []int{1},
	}
	err := e.resolveBoundCards(context.Background(), &spec)
	if err == nil {
		t.Fatalf("launch allowed on an unlisted card: indexes = %v", spec.GPUIndexes)
	}
	for _, i := range spec.GPUIndexes {
		if i < 0 {
			t.Fatalf("indexes = %v, want no device -1", spec.GPUIndexes)
		}
	}
}

// What the running instances hold: bound ones by identity, ones whose spec
// binds no identity (a server that predates the binding) by their device
// indexes so the caller can still name them — never dropped, since a card
// left out reads as somebody else's.
func TestHeldCardsKeepsInstancesWithoutABinding(t *testing.T) {
	e := &Engine{desired: map[string]proto.InstanceDesiredState{
		"bound":    {UUID: "bound", DesiredState: "running", GPUIndexes: []int{0}, GPUIdents: []string{"pci:0000:32:00.0"}},
		"legacy":   {UUID: "legacy", DesiredState: "running", GPUIndexes: []int{1}},
		"headless": {UUID: "headless", DesiredState: "running", GPUIndexes: []int{2}, Headless: true},
		"partial":  {UUID: "partial", DesiredState: "running", GPUIndexes: []int{3, 4}, GPUIdents: []string{"pci:0000:9a:00.0"}},
		"stopped":  {UUID: "stopped", DesiredState: "stopped", GPUIndexes: []int{5}, GPUIdents: []string{"pci:0000:b1:00.0"}},
		"stopping": {UUID: "stopping", DesiredState: "stopped", GPUIndexes: []int{6}},
	}}
	held := e.HeldCards()
	if len(held.Idents) != 1 || held.Idents[0] != "pci:0000:32:00.0" {
		t.Fatalf("idents = %v", held.Idents)
	}
	got := map[int]bool{}
	for _, index := range held.Indexes {
		got[index] = true
	}
	// A partial binding is not trusted, the same as at launch: its indexes
	// are the complete statement of the selection.
	if len(got) != 4 || !got[1] || !got[2] || !got[3] || !got[4] {
		t.Fatalf("indexes = %v, want 1 2 3 4", held.Indexes)
	}
}

// gatedStopDriver holds every stop until release is closed, the way a busy
// container runtime keeps an engine on its card while it shuts down.
type gatedStopDriver struct {
	*recording.Driver
	stopping chan struct{}
	release  chan struct{}
}

func (d *gatedStopDriver) Status(context.Context, string) (runtime.Status, error) {
	return runtime.Status{Exists: true, Running: true}, nil
}

func (d *gatedStopDriver) Stop(ctx context.Context, id string) error {
	close(d.stopping)
	<-d.release
	return d.Driver.Stop(ctx, id)
}

// An instance ordered away still has its engine on the card until its
// container is gone. Until then the card is this agent's: reading that
// engine as a stranger's process marked our own card as held outside the
// platform, and the replacement waited on it. Once the teardown is done the
// card is let go.
func TestHeldCardsKeepsAnInstanceUntilItsTeardownEnds(t *testing.T) {
	driver := &gatedStopDriver{Driver: recording.New(t.TempDir() + "/calls.json"), stopping: make(chan struct{}), release: make(chan struct{})}
	e := newHermeticEngine(driver, func(proto.InstanceStatus) {}, slog.New(slog.DiscardHandler), t.TempDir())
	e.desired["leaving"] = proto.InstanceDesiredState{UUID: "leaving", DesiredState: "running", GPUIndexes: []int{0}, GPUIdents: []string{"pci:0000:17:00.0"}}
	e.desiredKnown = true

	done := make(chan struct{})
	go func() {
		e.ApplyUpdate(context.Background(), nil, []string{"leaving"})
		close(done)
	}()
	<-driver.stopping
	if held := e.HeldCards(); len(held.Idents) != 1 || held.Idents[0] != "pci:0000:17:00.0" {
		t.Fatalf("while its container stops, the card is not ours: %+v", held)
	}
	close(driver.release)
	<-done
	if held := e.HeldCards(); len(held.Idents) != 0 || len(held.Indexes) != 0 {
		t.Fatalf("after the teardown the card is still held: %+v", held)
	}
}

// Memory-limited partitions repeat their cards in Partitions (or
// PartitionIndexes), so the occupancy reading can tell a card holding only
// partitions from one with a whole-card engine.
func TestHeldCardsNamesThePartitionsCards(t *testing.T) {
	e := &Engine{desired: map[string]proto.InstanceDesiredState{
		"partition": {UUID: "partition", DesiredState: "running", GPUIndexes: []int{0}, GPUIdents: []string{"pci:0000:32:00.0"}, VRAMLimitBytes: 16 << 30},
		"whole":     {UUID: "whole", DesiredState: "running", GPUIndexes: []int{1}, GPUIdents: []string{"pci:0000:33:00.0"}},
		"legacy":    {UUID: "legacy", DesiredState: "running", GPUIndexes: []int{2}, VRAMLimitBytes: 8 << 30},
	}}
	held := e.HeldCards()
	if len(held.Partitions) != 1 || held.Partitions[0] != "pci:0000:32:00.0" {
		t.Fatalf("partitions = %v", held.Partitions)
	}
	if len(held.PartitionIndexes) != 1 || held.PartitionIndexes[0] != 2 {
		t.Fatalf("partition indexes = %v", held.PartitionIndexes)
	}
}
