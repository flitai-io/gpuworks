package app

import (
	"context"
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// An agent that first reads a partitions-only card when more processes are
// there than its engines keep - just upgraded from a level that did not read
// such cards, or restarted while a job started beside our partition - cannot
// tell which are ours, and counts the ones holding the most as foreign. That
// answer is a guess and stays one: it is not taken as knowing which process
// is whose, so when the outside job leaves, the engine it was confused with
// is ours again and the card is let go after the release rounds - also
// across another restart, which finds the guess written to the state file.
func TestAGuessedOwnerIsNotKeptOnceTheCardSaysOtherwise(t *testing.T) {
	const bdf = "0000:3f:00.0"
	ours := accelerator.CardIdent("GPU-3f", bdf)
	engineProc := gpu.ComputeProcess{PID: 7001, Name: "python3", PCIeBDF: bdf, MemoryBytes: 20 << 30}
	job := gpu.ComputeProcess{PID: 9100, Name: "python3", PCIeBDF: bdf, MemoryBytes: 4 << 30}
	for _, restart := range []bool{false, true} {
		name := "same agent"
		if restart {
			name = "restarted while both run"
		}
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			prober := &occupiedProber{FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: twoOwnedCards()}}
			start := func() *NodeRuntime {
				n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, stateDir, "none", prober, nil)
				n.heldCards = func() engine.HeldCards {
					return engine.HeldCards{Idents: []string{ours}, Partitions: []string{ours}}
				}
				return n
			}
			n := start()
			read := func() []proto.CardProcess {
				t.Helper()
				cards := twoOwnedCards()
				n.CardHealthFindings(context.Background(), cards, true)
				return cards[0].ForeignProcesses
			}
			prober.procs = []gpu.ComputeProcess{job, engineProc}
			for range 2 {
				if got := read(); len(got) != 1 {
					t.Fatalf("both listed = %+v, want one counted foreign", got)
				}
			}
			if restart {
				n = start()
				if got := read(); len(got) != 1 {
					t.Fatalf("after the restart, both listed = %+v, want one counted foreign", got)
				}
			}
			// The outside job ends; only our engine is left.
			prober.procs = []gpu.ComputeProcess{engineProc}
			for range occupancyReleaseRounds {
				read()
			}
			for i := range 3 {
				if got := read(); len(got) != 0 {
					t.Fatalf("reading %d after the job left = %+v, want our engine not foreign", i, got)
				}
			}
		})
	}
}
