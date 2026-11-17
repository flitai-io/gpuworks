package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// resolveBoundCards rewrites the spec's device indexes to the ones the cards
// it is bound to carry right now. The server binds an instance to hardware
// identities (accelerator.CardIdent) because a device index numbers only the
// cards the driver could initialize: when one fails to initialize, is passed
// through to a VM, or is added, every later card shifts down or up and the
// index the instance was placed with would name a different card — one that
// may already be serving another instance.
//
// Everything downstream (device injection, dGPU partitioning, udocker's
// device nodes) keeps working in indexes, which is what the vendor runtimes
// actually take; this is the one place identity is turned back into them.
//
// A spec without identities comes from a server that predates them, or from
// a prober that reports neither a PCI address nor a UUID; it is used as it
// is. A card that cannot be found fails the launch: starting on whichever
// card now holds that index is the failure this exists to prevent.
func (e *Engine) resolveBoundCards(ctx context.Context, spec *proto.InstanceDesiredState) error {
	if len(spec.GPUIdents) == 0 || e.prober == nil {
		return nil
	}
	if len(spec.GPUIdents) != len(spec.GPUIndexes) {
		// Not something a current server sends; the indexes remain the only
		// complete statement of the selection.
		slog.Warn("instance carries a partial card binding; using its device indexes",
			"instance", spec.UUID, "idents", len(spec.GPUIdents), "indexes", len(spec.GPUIndexes))
		return nil
	}
	gpus, err := e.prober.Probe(ctx)
	if err != nil {
		// The cards cannot be enumerated at all. The indexes are as good as
		// anything else here, and the launch fails later with the runtime's
		// own error if the card really is gone.
		slog.Warn("card binding not checked: the accelerator probe failed",
			"instance", spec.UUID, "err", err)
		return nil
	}
	indexOf := make(map[string]int, len(gpus))
	for _, g := range gpus {
		if g.Index < 0 {
			// A card the vendor CLI did not list has no device to be
			// launched against: the probe reports it so an operator can
			// see the hardware, not so a container can be pointed at it.
			// Treating it as resolvable started instances on device -1.
			continue
		}
		if ident := accelerator.CardIdent(g.UUID, g.PCIeBDF); ident != "" {
			indexOf[ident] = g.Index
		}
	}
	resolved := make([]int, 0, len(spec.GPUIdents))
	for i, ident := range spec.GPUIdents {
		index, ok := indexOf[ident]
		if !ok {
			return withFailure(proto.RuntimeFailure{Code: proto.ErrorAcceleratorCardMissing, Resource: ident},
				fmt.Errorf("the accelerator this instance is placed on is not present: %s (it was device %d); the node reports %s",
					ident, spec.GPUIndexes[i], describeCards(gpus)))
		}
		resolved = append(resolved, index)
	}
	if !sameInts(resolved, spec.GPUIndexes) {
		slog.Info("instance follows its cards to their current device indexes",
			"instance", spec.UUID, "from", spec.GPUIndexes, "to", resolved)
		spec.GPUIndexes = resolved
	}
	return nil
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// describeCards names what the node does have, so the failure says which
// card went missing and what replaced it instead of only an identity.
func describeCards(gpus []proto.GPUInfo) string {
	if len(gpus) == 0 {
		return "no accelerators"
	}
	parts := make([]string, 0, len(gpus))
	for _, g := range gpus {
		ident := accelerator.CardIdent(g.UUID, g.PCIeBDF)
		if ident == "" {
			ident = "unidentified"
		}
		parts = append(parts, fmt.Sprintf("device %d = %s", g.Index, ident))
	}
	return strings.Join(parts, ", ")
}

// HeldCards are the cards this agent's instances occupy, including the ones
// no metrics are scraped from.
//
// It is deliberately not MetricSpecs: that drops headless workers and
// instances with no API port, and RunningSpecs drops sleepers. A sleeping
// instance still holds its CUDA context and a headless worker is still
// computing, so leaving either out told the agent those cards were idle —
// their own processes then read as somebody else's, and the runtime check
// would create a context on a card that is serving.
type HeldCards struct {
	// Idents are hardware identities: e.desired carries the indexes
	// placement chose, which drift, and the identity is what survives that.
	Idents []string
	// Indexes are the device indexes of instances whose spec binds no
	// identity (a server that predates the binding, or a partial one; see
	// resolveBoundCards). They are still this agent's cards: the caller
	// names them from the cards it probed, rather than reading "no identity"
	// as "no card".
	Indexes []int
	// Partitions and PartitionIndexes repeat, for each memory-limited
	// partition among them, its cards (by identity, or by index as above).
	// A card listed as often here as above holds only partitions, whose
	// engines keep one process each on it.
	Partitions       []string
	PartitionIndexes []int
}

// HeldCards is what the running instances occupy, and what the instances
// being torn down still do: an instance ordered away keeps its engine on the
// card until its container is gone (cleanupRequired), and reading that
// engine as somebody else's process marked our own card as held outside the
// platform - the replacement then waited on it as on a stranger's.
func (e *Engine) HeldCards() HeldCards {
	e.mu.Lock()
	defer e.mu.Unlock()
	var held HeldCards
	add := func(s proto.InstanceDesiredState) {
		partition := s.VRAMLimitBytes > 0
		if len(s.GPUIdents) > 0 && len(s.GPUIdents) == len(s.GPUIndexes) {
			held.Idents = append(held.Idents, s.GPUIdents...)
			if partition {
				held.Partitions = append(held.Partitions, s.GPUIdents...)
			}
			return
		}
		held.Indexes = append(held.Indexes, s.GPUIndexes...)
		if partition {
			held.PartitionIndexes = append(held.PartitionIndexes, s.GPUIndexes...)
		}
	}
	for _, s := range e.desired {
		if s.DesiredState == "running" {
			add(s)
		}
	}
	// A same-UUID replacement waiting on its old generation holds both: the
	// cards the old one has not let go of and the ones the new one wants.
	for _, s := range e.cleanupRequired {
		add(s)
	}
	return held
}

// CardIdentsOf is the hardware identity of the cards one instance was given,
// whatever state it reached. A caller asking after a start failure needs the
// cards the attempt used, which HeldCards deliberately leaves out: the
// instance is no longer running.
func (e *Engine) CardIdentsOf(uuid string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	spec, ok := e.desired[uuid]
	if !ok {
		return nil
	}
	return append([]string(nil), spec.GPUIdents...)
}
