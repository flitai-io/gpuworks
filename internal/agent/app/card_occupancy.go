package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Who else is using this node's cards.
//
// A node is not always the platform's alone: somebody logs in and starts a
// training script or a vLLM of their own. That card's memory is then spoken
// for by something the platform cannot see, let alone plan around, so
// placing a replica on it ends in an out-of-memory failure at startup — the
// platform's own accounting said the card was free.
//
// A process is "foreign" when the card it runs on holds none of this
// agent's instances. That is deliberately coarser than matching process
// IDs to containers: it never mistakes an idle card for a busy one, which
// is the error that would cost a deployment, and it needs no cgroup
// parsing. Its cost is that a card already running one of our instances is
// not examined for a second, foreign workload — and such a card is already
// accounted for and not offered again.

// cardOccupancy is the last occupancy this agent established, by card
// identity. The server writes a report's occupancy over what it had, so a
// round that could not find out — the vendor tool failed or timed out, or
// the agent has just started and does not know its own instances yet —
// must restate the last answer rather than send none: none reads as "free",
// and a card somebody else is using would be offered for a deployment.
type cardOccupancy struct {
	mu   sync.Mutex
	last map[string][]proto.CardProcess
	// freeRounds counts the readings in a row that found a card in last
	// free again. A card is let go only after occupancyReleaseRounds of
	// them: somebody's job that starts and stops every minute would
	// otherwise flip the card in and out of scheduling with every report.
	freeRounds map[string]int
	// identOfIndex is each card's identity by the device index it carried
	// in the last probe that listed the cards. It names the cards of an
	// instance whose desired state binds none (see engine.HeldCards).
	identOfIndex map[int]string
	// external is the last answer of the runtime's external card holders.
	external map[string][]proto.CardProcess
}

// learnCards records which card each device index names in a probe that
// listed the cards.
func (o *cardOccupancy) learnCards(gpus []proto.GPUInfo) {
	byIndex := make(map[int]string, len(gpus))
	for _, card := range gpus {
		if card.Index < 0 {
			continue
		}
		if ident := accelerator.CardIdent(card.UUID, card.PCIeBDF); ident != "" {
			byIndex[card.Index] = ident
		}
	}
	o.mu.Lock()
	o.identOfIndex = byIndex
	o.mu.Unlock()
}

// identOf is the card a device index named in the last probe, if any.
func (o *cardOccupancy) identOf(index int) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ident, ok := o.identOfIndex[index]
	return ident, ok
}

// occupancyReleaseRounds is how many readings in a row must find a card free
// before it is let go: about three minutes.
const occupancyReleaseRounds = 3

// remember records a round that did find out, and reports which cards'
// foreign use changed since the last one, so it is logged once rather than
// every minute. A card found free after being used outside the platform keeps
// its last processes until occupancyReleaseRounds readings in a row have found
// it free — unless it now holds one of this node's own instances (ours),
// whose processes are not foreign.
func (o *cardOccupancy) remember(gpus []proto.GPUInfo, ours map[string]bool) (changed []proto.GPUInfo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.freeRounds == nil {
		o.freeRounds = map[string]int{}
	}
	next := map[string][]proto.CardProcess{}
	for i := range gpus {
		card := &gpus[i]
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if ident == "" {
			continue
		}
		if len(card.ForeignProcesses) == 0 {
			held := o.last[ident]
			if len(held) == 0 || ours[ident] || !accelerator.CardUsable(card.Ownership) {
				delete(o.freeRounds, ident)
				continue
			}
			if o.freeRounds[ident]++; o.freeRounds[ident] >= occupancyReleaseRounds {
				delete(o.freeRounds, ident)
				slog.Info("card no longer in use outside the platform", "card", card.PCIeBDF)
				continue
			}
			card.ForeignProcesses = append([]proto.CardProcess(nil), held...)
			next[ident] = held
			continue
		}
		delete(o.freeRounds, ident)
		next[ident] = card.ForeignProcesses
		if !sameProcesses(o.last[ident], card.ForeignProcesses) {
			changed = append(changed, *card)
		}
	}
	o.last = next
	return changed
}

// foreign reports whether the last answer found somebody outside the
// platform using the card.
func (o *cardOccupancy) foreign(ident string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.last[ident]) > 0
}

// smokeOccupancyGrace is how long after a runtime check a listing may still
// show its process: npu-smi's table is reused for five seconds, and the
// listing is read a moment after the probe that fetched it.
const smokeOccupancyGrace = 15 * time.Second

// restateCard lays the last answer over one card this round did not examine.
func (o *cardOccupancy) restateCard(card *proto.GPUInfo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	card.ForeignProcesses = append([]proto.CardProcess(nil), o.last[accelerator.CardIdent(card.UUID, card.PCIeBDF)]...)
}

// restate lays the last answer over cards this round could not examine.
func (o *cardOccupancy) restate(gpus []proto.GPUInfo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range gpus {
		card := &gpus[i]
		if procs, ok := o.last[accelerator.CardIdent(card.UUID, card.PCIeBDF)]; ok && accelerator.CardUsable(card.Ownership) {
			card.ForeignProcesses = append([]proto.CardProcess(nil), procs...)
		}
	}
}

func sameProcesses(a, b []proto.CardProcess) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PID != b[i].PID || a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

// applyForeignProcesses marks the cards somebody outside the platform is
// using. It mutates the cards in place, like the other findings this agent
// contributes to its own report.
func (n *NodeRuntime) applyForeignProcesses(ctx context.Context, gpus []proto.GPUInfo, probeOK bool) {
	defer n.applyExternalHolders(ctx, gpus)
	lister, ok := n.prober.(gpu.ComputeProcessLister)
	if !ok || len(gpus) == 0 {
		return
	}
	if !probeOK {
		// The tool just failed to list the cards; asking it which
		// processes hold them spends the whole vendor timeout to be told
		// the same thing.
		slog.Debug("card occupancy restated: the accelerator probe failed this round")
		n.occupancy.restate(gpus)
		return
	}
	procs, err := lister.ComputeProcesses(ctx)
	if err != nil {
		// Not knowing is not the same as knowing a card is free.
		slog.Debug("compute processes unavailable; card occupancy restated", "err", err)
		n.occupancy.restate(gpus)
		return
	}
	ours, known := n.cardsInUse()
	if !known {
		// This agent has just started and has not been told what it should
		// be running. Every card serving one of our own instances would
		// read as somebody else's, and the cards the platform is using
		// most would be the ones it stopped scheduling.
		slog.Debug("card occupancy restated: this agent's own instances are not known yet")
		n.occupancy.restate(gpus)
		return
	}
	sleeping := n.sleepingCardPIDs()
	for i := range gpus {
		card := &gpus[i]
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if !accelerator.CardUsable(card.Ownership) {
			continue
		}
		if n.cardSmoke.Touched(ident, smokeOccupancyGrace) {
			// The runtime check's own process is on the card, or was when
			// the vendor tool listed it. The check only asks cards nobody
			// uses, so the last answer stands for this round.
			n.occupancy.restateCard(card)
			continue
		}
		// A card of ours is examined only when every instance on it sleeps
		// and its engines' own processes are known: any other process there
		// is somebody else's, and the server must refuse a wake onto it.
		own, examine := sleeping[ident]
		if ours[ident] && !examine {
			continue
		}
		for _, proc := range procs {
			if !gpu.SameBDF(proc.PCIeBDF, card.PCIeBDF) || own[proc.PID] {
				continue
			}
			card.ForeignProcesses = append(card.ForeignProcesses, proto.CardProcess{
				PID: proc.PID, Name: proc.Name, MemoryBytes: proc.MemoryBytes,
			})
		}
	}
	for _, card := range n.occupancy.remember(gpus, ours) {
		slog.Info("card is in use outside the platform",
			"card", card.PCIeBDF, "processes", len(card.ForeignProcesses))
	}
}

// sleepingCardPIDs are the cards of ours on which every instance sleeps and
// its engine's process is known (engine.SleepingCardPIDs), with those
// processes. sleepingCards is the seam tests use.
func (n *NodeRuntime) sleepingCardPIDs() map[string]map[int]bool {
	if n.sleepingCards != nil {
		return n.sleepingCards()
	}
	if n.engine == nil {
		return nil
	}
	return n.engine.SleepingCardPIDs()
}

// cardsInUse are the hardware identities this agent's own instances
// occupy, and whether that is known at all. It is the one answer to "is
// this card ours already", asked both by the runtime check (which leaves
// such a card alone) and by the occupancy report (which does not call its
// processes foreign).
//
// Identities, not device indexes: an index is placement-time and drifts,
// and comparing a drifted one called the wrong cards ours.
//
// known is false until the control plane has told this agent what to run.
// An agent that has just restarted still has its instances running on the
// cards, so an empty set read as "none of these are ours" would call every
// one of them foreign — measured on gpu3, where both cards were reported
// as used outside the platform for one report after an agent upgrade.
//
// An instance whose desired state binds no identity is named by its device
// indexes through the last probe. Leaving it out is the same mistake: a
// server that launched instances before cards were bound replayed them
// without identities, and every card serving one of them was reported as
// held outside the platform and stopped being scheduled. Until a probe has
// named those indexes, the answer is not known.
//
// heldCards is a seam: the engine starts containers, which a test of
// these rules has no business doing.
func (n *NodeRuntime) cardsInUse() (cards map[string]bool, known bool) {
	var held engine.HeldCards
	switch {
	case n.heldCards != nil:
		held, known = n.heldCards(), true
	case n.engine == nil:
		return map[string]bool{}, false
	default:
		held, known = n.engine.HeldCards(), n.engine.DesiredKnown()
	}
	ours := map[string]bool{}
	for _, ident := range held.Idents {
		if ident != "" {
			ours[ident] = true
		}
	}
	for _, index := range held.Indexes {
		ident, ok := n.occupancy.identOf(index)
		if !ok {
			known = false
			continue
		}
		ours[ident] = true
	}
	return ours, known
}

// externalCardHolders is a runtime that knows of allocations outside the
// platform no process list shows: the kube runtime, whose kubelet may have
// given a card to another pod that has not touched it yet.
type externalCardHolders interface {
	ExternalCardHolders(context.Context) (map[string][]proto.CardProcess, error)
}

// applyExternalHolders adds those holders to the cards' foreign use. A round
// that cannot ask restates the last answer, as for processes: not knowing is
// not the same as knowing a card is free.
func (n *NodeRuntime) applyExternalHolders(ctx context.Context, gpus []proto.GPUInfo) {
	runtime, ok := n.runtimeDriver().(externalCardHolders)
	if !ok {
		return
	}
	holders, err := runtime.ExternalCardHolders(ctx)
	n.occupancy.mu.Lock()
	if err != nil {
		slog.Debug("kubelet allocations unavailable; restating the last", "err", err)
		holders = n.occupancy.external
	} else {
		n.occupancy.external = holders
	}
	n.occupancy.mu.Unlock()
	for i := range gpus {
		card := &gpus[i]
		if accelerator.CardUsable(card.Ownership) {
			card.ForeignProcesses = append(card.ForeignProcesses, holders[card.UUID]...)
		}
	}
}
