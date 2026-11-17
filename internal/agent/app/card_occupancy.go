package app

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
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
// parsing. Its cost is that a card running only our instances is not
// examined for a second, foreign workload. A card our instance shares with
// processes found there before it joined them is read differently: the
// server may place a memory-limited partition beside them and counts what
// they hold (ADR 0068), so every process there beyond the one each of our
// engines keeps on the card is somebody else's (readSharedCard).

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
	// own is, on a card our instance shares with foreign processes, the
	// processes taken for our engines', by PID with their names
	// (readSharedCard). Kept in memory only: a restarted agent takes them
	// again from what the card lists.
	own map[string]map[int]string
	// shared is the cards readSharedCard has read since our instances last
	// joined them (readsShared).
	shared map[string]bool
	// guessed is, on a card readSharedCard last read with more unknown
	// processes than our engines had places for, those processes by PID with
	// their names: which of them are ours was a guess, so the next reading
	// takes none of them as known either way (readSharedCard), and the state
	// file keeps them without their PIDs (saveLocked).
	guessed map[string]map[int]string
	// path is where last is kept across restarts (cardOccupancyFile); empty
	// keeps it in memory only.
	path string
	// bootTime reads when the running boot started (hostBootTime when nil).
	bootTime func() time.Time
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
// it free, whether or not one of this node's own instances is on it now: a
// partition placed beside those processes was admitted against what they
// held, and the card is not to read as free the moment they stop showing.
func (o *cardOccupancy) remember(gpus []proto.GPUInfo) (changed []proto.GPUInfo) {
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
			if len(held) == 0 || !accelerator.CardUsable(card.Ownership) {
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
	previous := o.last
	o.last = next
	o.saveLocked(previous)
	return changed
}

// knownForeign is the processes the last answer named on a card as
// somebody else's: on a card our instance has joined since, they are the
// ones still known not to be ours.
func (o *cardOccupancy) knownForeign(ident string) []proto.CardProcess {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]proto.CardProcess(nil), o.last[ident]...)
}

// readSharedCard reads a card our instances share with processes known to
// be foreign there (known), listing procs, and returns the foreign ones. Our
// engines keep one process each on a card - the rule the sleep residue's
// reading relies on too - so slots of the listed processes are ours:
//
//   - a known foreign process still listed under its PID and name (the name
//     guards against the PID being reused by a process of ours) is foreign,
//     at what it holds now, or at its last figure when the tool prints none;
//   - a process already taken for one of our engines' stays ours;
//   - of the rest, as many as our engines have no process for yet are taken
//     for theirs, and any beyond them are foreign - a second job somebody
//     starts beside ours, or the foreign workload restarted under a new PID.
//     When more appear than that at once, the ones holding the most are
//     counted foreign: overstating what the outside holds refuses a
//     partition, understating it runs one out of memory.
//
// When which of them are ours is a guess - more processes than our engines
// have places for, none known - the guess is recorded (guessed) and not taken
// as knowledge: the next reading weighs those processes afresh, so once the
// outside job leaves, an engine of ours that was counted foreign in its
// place is ours again rather than foreign for as long as it runs.
//
// A known process no longer listed - one that exited, restarted under a new
// PID, or was carried over a reboot without its PID (see load) - is not
// reported: the card has been read. Its memory is not offered at once,
// though: when nothing foreign is left, remember keeps the last answer for
// the release rounds. The card is read so for as long as our instances stay
// (readsShared).
func (o *cardOccupancy) readSharedCard(ident string, known []proto.CardProcess, slots int, procs []gpu.ComputeProcess, bdf string) []proto.CardProcess {
	o.mu.Lock()
	defer o.mu.Unlock()
	previous, guessed := o.own[ident], o.guessed[ident]
	own := map[int]string{}
	var foreign, fresh []proto.CardProcess
	for _, proc := range procs {
		if !gpu.SameBDF(proc.PCIeBDF, bdf) {
			continue
		}
		entry := proto.CardProcess{PID: proc.PID, Name: proc.Name, MemoryBytes: proc.MemoryBytes}
		if name, ok := guessed[proc.PID]; ok && name == proc.Name {
			fresh = append(fresh, entry)
			continue
		}
		// Ours before known foreign: a process a guess counted foreign and
		// a later reading found to be ours stays named in the last answer
		// for the release rounds.
		if name, ok := previous[proc.PID]; ok && name == proc.Name {
			own[proc.PID] = proc.Name
			continue
		}
		if i := slices.IndexFunc(known, func(k proto.CardProcess) bool { return k.PID > 0 && k.PID == proc.PID && k.Name == proc.Name }); i >= 0 {
			if entry.MemoryBytes <= 0 {
				entry.MemoryBytes = known[i].MemoryBytes
			}
			foreign = append(foreign, entry)
			continue
		}
		fresh = append(fresh, entry)
	}
	free := max(0, slots-len(own))
	var guess map[int]string
	if len(fresh) > free {
		if free > 0 {
			guess = make(map[int]string, len(fresh))
			for _, entry := range fresh {
				guess[entry.PID] = entry.Name
			}
		}
		slices.SortStableFunc(fresh, func(a, b proto.CardProcess) int { return cmp.Compare(a.MemoryBytes, b.MemoryBytes) })
		foreign = append(foreign, fresh[free:]...)
		fresh = fresh[:free]
	}
	for _, entry := range fresh {
		own[entry.PID] = entry.Name
	}
	if o.own == nil {
		o.own, o.shared = map[string]map[int]string{}, map[string]bool{}
	}
	if o.guessed == nil {
		o.guessed = map[string]map[int]string{}
	}
	o.own[ident], o.shared[ident] = own, true
	if guess != nil {
		o.guessed[ident] = guess
	} else {
		delete(o.guessed, ident)
	}
	return foreign
}

// readsShared is whether readSharedCard has read the card since our
// instances last joined it: a card once shared with foreign processes is
// read so for as long as they stay, and a foreign workload that comes back
// after its processes were let go is found again.
func (o *cardOccupancy) readsShared(ident string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.shared[ident]
}

// setOwn records, on a card examined this round, the processes known to be
// our engines' there: those of instances that all sleep (pids), or none when
// nothing of ours runs there. When such an engine wakes, readSharedCard
// starts from them rather than from whatever the card lists first.
func (o *cardOccupancy) setOwn(ident string, pids map[int]bool, procs []gpu.ComputeProcess, bdf string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.shared, ident)
	delete(o.guessed, ident)
	if len(pids) == 0 {
		delete(o.own, ident)
		return
	}
	own := map[int]string{}
	for _, proc := range procs {
		if pids[proc.PID] && gpu.SameBDF(proc.PCIeBDF, bdf) {
			own[proc.PID] = proc.Name
		}
	}
	if o.own == nil {
		o.own, o.shared = map[string]map[int]string{}, map[string]bool{}
	}
	o.own[ident] = own
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
	ours, partitionsOnly, known := n.cardsInUse()
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
		// Otherwise a card our instances share with processes known to be
		// foreign is read by how many processes our engines keep there
		// (readSharedCard), and any other card of ours is not examined: a
		// new process there may be our engine's.
		//
		// A card that holds only partitions of ours is read so too: each
		// partition's engine keeps one process there, so a job somebody
		// starts beside them later is told from ours and reported
		// (proto.PartitionSafetyForeignTrackedPartitioned). A card with a
		// whole-card instance of ours is not: its engine keeps no fixed
		// number of processes.
		own, examine := sleeping[ident]
		if slots := ours[ident]; slots > 0 && !examine {
			if known := n.occupancy.knownForeign(ident); len(known) > 0 || n.occupancy.readsShared(ident) || partitionsOnly[ident] {
				card.ForeignProcesses = n.occupancy.readSharedCard(ident, known, slots, procs, card.PCIeBDF)
			}
			continue
		}
		n.occupancy.setOwn(ident, own, procs, card.PCIeBDF)
		for _, proc := range procs {
			if !gpu.SameBDF(proc.PCIeBDF, card.PCIeBDF) || own[proc.PID] {
				continue
			}
			card.ForeignProcesses = append(card.ForeignProcesses, proto.CardProcess{
				PID: proc.PID, Name: proc.Name, MemoryBytes: proc.MemoryBytes,
			})
		}
	}
	for _, card := range n.occupancy.remember(gpus) {
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
//
// Each card counts how many of our instances hold it: an instance waiting on
// its old generation counts for both.
//
// partitionsOnly are the cards of ours that hold only memory-limited
// partitions (engine.HeldCards.Partitions), whose engines keep one process
// each there.
func (n *NodeRuntime) cardsInUse() (cards map[string]int, partitionsOnly map[string]bool, known bool) {
	var held engine.HeldCards
	switch {
	case n.heldCards != nil:
		held, known = n.heldCards(), true
	case n.engine == nil:
		return map[string]int{}, map[string]bool{}, false
	default:
		held, known = n.engine.HeldCards(), n.engine.DesiredKnown()
	}
	count := func(idents []string, indexes []int) map[string]int {
		out := map[string]int{}
		for _, ident := range idents {
			if ident != "" {
				out[ident]++
			}
		}
		for _, index := range indexes {
			ident, ok := n.occupancy.identOf(index)
			if !ok {
				known = false
				continue
			}
			out[ident]++
		}
		return out
	}
	ours := count(held.Idents, held.Indexes)
	partitions := count(held.Partitions, held.PartitionIndexes)
	partitionsOnly = map[string]bool{}
	for ident, slots := range ours {
		if partitions[ident] == slots {
			partitionsOnly[ident] = true
		}
	}
	return ours, partitionsOnly, known
}

// cardSet is cardsInUse's answer without the counts and kinds.
func cardSet(counts map[string]int, _ map[string]bool, known bool) (map[string]bool, bool) {
	cards := make(map[string]bool, len(counts))
	for ident := range counts {
		cards[ident] = true
	}
	return cards, known
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
