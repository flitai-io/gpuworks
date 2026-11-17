package app

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A card that holds only partitions of ours is read by how many processes
// their engines keep there (readSharedCard) even when nothing foreign was
// known on it: a job somebody starts on it afterwards is reported, so the
// server can note it on the instance and count its memory
// (proto.PartitionSafetyForeignTrackedPartitioned).
func TestAProcessStartedBesideOurPartitionLaterIsReported(t *testing.T) {
	const bdf = "0000:3f:00.0"
	ours := accelerator.CardIdent("GPU-3f", bdf)
	prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()}}
	n := occupancyNode(t, twoOwnedCards(), nil, nil)
	n.prober = prober
	n.heldCards = func() engine.HeldCards {
		return engine.HeldCards{Idents: []string{ours}, Partitions: []string{ours}}
	}
	read := func() []proto.CardProcess {
		t.Helper()
		cards := twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, true)
		return cards[0].ForeignProcesses
	}
	engineProc := gpu.ComputeProcess{PID: 7001, Name: "python3", PCIeBDF: bdf, MemoryBytes: 17 << 30}
	prober.procs = []gpu.ComputeProcess{engineProc}
	if got := read(); len(got) != 0 {
		t.Fatalf("our partition's own engine = %+v, want nothing foreign", got)
	}
	// Somebody starts a job on the card.
	job := gpu.ComputeProcess{PID: 9100, Name: "train.py", PCIeBDF: bdf, MemoryBytes: 30 << 30}
	prober.procs = []gpu.ComputeProcess{engineProc, job}
	if got := read(); len(got) != 1 || got[0].PID != 9100 || got[0].MemoryBytes != 30<<30 {
		t.Fatalf("after a job started beside our partition = %+v, want it reported", got)
	}
	// Not knowing is not knowing it left.
	prober.err = context.DeadlineExceeded
	if got := read(); len(got) != 1 || got[0].PID != 9100 {
		t.Fatalf("a round that could not list = %+v, want the job restated", got)
	}
	prober.err = nil
	// The job ends: released after the release rounds, not at once.
	prober.procs = []gpu.ComputeProcess{engineProc}
	for i := 1; i < occupancyReleaseRounds; i++ {
		if got := read(); len(got) != 1 || got[0].PID != 9100 {
			t.Fatalf("reading %d after the job ended let the card go early: %+v", i, got)
		}
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("after the release rounds = %+v, want the card clear", got)
	}
	// Our engine restarts under a new PID: still ours, not a stranger.
	prober.procs = []gpu.ComputeProcess{{PID: 7055, Name: "python3", PCIeBDF: bdf, MemoryBytes: 17 << 30}}
	if got := read(); len(got) != 0 {
		t.Fatalf("our engine restarted = %+v, want nothing foreign", got)
	}
}

// A card that also holds a whole-card instance of ours is not read so: a
// whole-card engine keeps no fixed number of processes, and a count would
// call its own workers foreign. It is examined as before - not at all.
func TestACardWithAWholeCardInstanceIsNotReadByCount(t *testing.T) {
	const bdf = "0000:3f:00.0"
	ours := accelerator.CardIdent("GPU-3f", bdf)
	prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()},
		procs: []gpu.ComputeProcess{{PID: 7001, Name: "python3", PCIeBDF: bdf}, {PID: 7002, Name: "python3", PCIeBDF: bdf}}}
	n := occupancyNode(t, twoOwnedCards(), nil, nil)
	n.prober = prober
	for name, held := range map[string]engine.HeldCards{
		"whole-card only":         {Idents: []string{ours}},
		"a partition and a whole": {Idents: []string{ours, ours}, Partitions: []string{ours}},
	} {
		n.heldCards = func() engine.HeldCards { return held }
		cards := twoOwnedCards()
		n.CardHealthFindings(context.Background(), cards, true)
		if got := cards[0].ForeignProcesses; len(got) != 0 {
			t.Fatalf("%s: %+v, want the card not read by count", name, got)
		}
	}
}
