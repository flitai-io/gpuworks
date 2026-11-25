package gpu

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A host without the vendor's tool is not a failed probe, and its tool is
// never run.
func TestProbeCardsWithoutTheToolAsksNothing(t *testing.T) {
	baseline := &cardBaseline{cards: map[int]proto.GPUInfo{0: {UUID: "kept"}}}
	asked := false
	gpus, err := probeCards(t.Context(), accelerator.VendorPPU, t.TempDir(), false, func(context.Context) ([]proto.GPUInfo, error) {
		asked = true
		return nil, nil
	}, baseline)
	if err != nil || len(gpus) != 0 {
		t.Fatalf("probe = %v, %v; want no cards and no error", gpus, err)
	}
	if asked {
		t.Fatal("the tool was asked although it is not installed")
	}
	if g, ok := baseline.at(0); !ok || g.UUID != "kept" {
		t.Fatalf("baseline = %+v, %v; want the earlier answer kept", g, ok)
	}
}

// A tool that cannot be asked says nothing about the cards having changed:
// the error is returned and the identities of the last answer stay.
func TestProbeCardsKeepsTheBaselineWhenTheToolFails(t *testing.T) {
	baseline := &cardBaseline{cards: map[int]proto.GPUInfo{0: {UUID: "kept"}}}
	failure := errors.New("driver not loaded")
	gpus, err := probeCards(t.Context(), accelerator.VendorPPU, t.TempDir(), true, func(context.Context) ([]proto.GPUInfo, error) {
		// What a vendor returns beside an error is not trusted.
		return []proto.GPUInfo{{Index: 0, UUID: "partial"}}, failure
	}, baseline)
	if !errors.Is(err, failure) {
		t.Fatalf("err = %v; want the tool's own error", err)
	}
	if len(gpus) != 0 {
		t.Fatalf("cards = %+v; want none from a listing that failed", gpus)
	}
	if g, _ := baseline.at(0); g.UUID != "kept" {
		t.Fatalf("baseline card 0 = %q; want the earlier answer kept", g.UUID)
	}
}

// An answer replaces the baseline whole: a card that is gone must not be
// what a later index is compared against, and a card without an index has
// no place in it.
func TestProbeCardsReplacesTheBaselineWithEachAnswer(t *testing.T) {
	baseline := &cardBaseline{cards: map[int]proto.GPUInfo{0: {UUID: "gone"}, 7: {UUID: "gone-too"}}}
	listed := []proto.GPUInfo{
		{Index: 0, UUID: "GPU-a", PCIeBDF: "0000:19:00.0"},
		{Index: 1, UUID: "GPU-b", PCIeBDF: "0000:67:00.0"},
		{Index: -1, UUID: "unindexed"},
	}
	gpus, err := probeCards(t.Context(), accelerator.VendorPPU, t.TempDir(), true, func(context.Context) ([]proto.GPUInfo, error) {
		return listed, nil
	}, baseline)
	if err != nil || len(gpus) != len(listed) {
		t.Fatalf("probe = %+v, %v; want the three listed cards", gpus, err)
	}
	all := baseline.all()
	if len(all) != 2 || all[0].UUID != "GPU-a" || all[1].PCIeBDF != "0000:67:00.0" {
		t.Fatalf("baseline = %+v; want exactly the two indexed cards of this answer", all)
	}
	if _, ok := baseline.at(7); ok {
		t.Fatal("card 7 of an earlier answer is still in the baseline")
	}
}
