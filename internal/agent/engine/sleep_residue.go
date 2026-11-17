package engine

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// defaultResidueSettle is how long the agent waits after a sleep order returns
// before it reads the card's memory. The engine releases its weights and KV
// cache asynchronously from the HTTP answer, and the driver's counter lags the
// release, so an immediate read still includes memory that is about to be freed.
const defaultResidueSettle = time.Second

const memoryUsedMetric = "gpuworks_gpu_memory_used_bytes"

// measureSleepResidue returns the GPU memory a sleeping instance still holds
// per card (CUDA context, graph pools, allocator slack), which the server needs
// to place other work on the same card. It returns the largest used-bytes value
// across the instance's cards, or 0 when the figure is not trustworthy. 0 means
// "unmeasured", never "nothing held": the card's used memory belongs to this
// instance only when nothing else can be using the card, so every doubt reports
// 0 and the server falls back to its estimate.
//
// The reading is trusted only when
//   - no other instance this agent desires is assigned to any of these cards
//     (compared by hardware identity where both carry one, and by device index
//     otherwise, which can only over-report sharing), and
//   - the vendor tool lists at most one compute process on each card. The
//     sleeping engine keeps exactly one worker process per card, so a second
//     one is somebody else's. The listing does not say whose a process is,
//     which is why this is a count and not a PID match. A prober without a
//     process listing (the ComputeProcessLister extension) cannot tell, and
//     its reading is accepted on the first condition alone.
func (e *Engine) measureSleepResidue(ctx context.Context, spec proto.InstanceDesiredState) int64 {
	if e.prober == nil || len(spec.GPUIndexes) == 0 {
		return 0
	}
	if e.cardsSharedWithOthers(spec) {
		return 0
	}
	resolved := spec
	if err := e.resolveBoundCards(ctx, &resolved); err != nil {
		e.log.Debug("sleep residue not measured: card binding unresolved", "uuid", spec.UUID, "err", err)
		return 0
	}
	own, single := e.cardsHaveSingleProcess(ctx, resolved)
	if !single {
		return 0
	}
	if e.residueSettle > 0 {
		timer := time.NewTimer(e.residueSettle)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0
		case <-timer.C:
		}
	}
	metrics, err := e.prober.Metrics(ctx)
	if err != nil {
		e.log.Debug("sleep residue not measured: metrics unavailable", "uuid", spec.UUID, "err", err)
		return 0
	}
	used := map[int]float64{}
	for _, m := range metrics {
		if v, ok := m.Values[memoryUsedMetric]; ok {
			used[m.Index] = v
		}
	}
	var residue int64
	for _, index := range resolved.GPUIndexes {
		v, ok := used[index]
		if !ok || v < 0 {
			e.log.Debug("sleep residue not measured: no memory reading for card", "uuid", spec.UUID, "gpu", index)
			return 0
		}
		if b := int64(v); b > residue {
			residue = b
		}
	}
	if len(own) > 0 {
		e.mu.Lock()
		e.processLocked(spec.UUID).sleepPIDs = own
		e.mu.Unlock()
	}
	return residue
}

// SleepingCardPIDs are the cards on which every instance this agent desires
// running is asleep and the process each such engine keeps there is known
// (learned when its residue was measured), with those processes, by card
// identity. Any other process on such a card is not the platform's: the
// node reports it, so a wake onto the card is refused rather than run out
// of memory. A card one of whose instances is awake, or whose sleeper's
// process is not known, is absent.
func (e *Engine) SleepingCardPIDs() map[string]map[int]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]map[int]bool{}
	blocked := map[string]bool{}
	for uuid, spec := range e.desired {
		if spec.DesiredState != "running" {
			continue
		}
		process := e.processOfLocked(uuid)
		pids := process.sleepPIDs
		asleep := process.sleepLevel > 0
		for ident := range pids {
			if !asleep {
				blocked[ident] = true
			}
		}
		for _, ident := range spec.GPUIdents {
			pid, ok := pids[ident]
			if !asleep || !ok {
				blocked[ident] = true
				continue
			}
			if out[ident] == nil {
				out[ident] = map[int]bool{}
			}
			out[ident][pid] = true
		}
	}
	for ident := range blocked {
		delete(out, ident)
	}
	return out
}

// cardsSharedWithOthers reports whether any other instance this agent holds a
// desired state for is assigned to one of spec's cards. Instances awaiting
// cleanup count too: their old generation may still hold the card.
func (e *Engine) cardsSharedWithOthers(spec proto.InstanceDesiredState) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for uuid, other := range e.desired {
		if uuid != spec.UUID && other.UUID != spec.UUID && sharesCard(spec, other) {
			return true
		}
	}
	for uuid, other := range e.cleanupRequired {
		if uuid != spec.UUID && other.UUID != spec.UUID && sharesCard(spec, other) {
			return true
		}
	}
	return false
}

func sharesCard(a, b proto.InstanceDesiredState) bool {
	for _, ia := range a.GPUIdents {
		for _, ib := range b.GPUIdents {
			if ia != "" && ia == ib {
				return true
			}
		}
	}
	for _, ia := range a.GPUIndexes {
		for _, ib := range b.GPUIndexes {
			if ia == ib {
				return true
			}
		}
	}
	return false
}

// cardsHaveSingleProcess reports whether no compute process other than the
// instance's own engine can be on its cards, and that process on each card
// (by card identity) when the vendor tool lists processes. A prober that
// cannot list processes gives no evidence either way and is accepted.
func (e *Engine) cardsHaveSingleProcess(ctx context.Context, spec proto.InstanceDesiredState) (map[string]int, bool) {
	lister, ok := e.prober.(gpu.ComputeProcessLister)
	if !ok {
		return nil, true
	}
	gpus, err := e.prober.Probe(ctx)
	if err != nil {
		return nil, false
	}
	procs, err := lister.ComputeProcesses(ctx)
	if err != nil {
		e.log.Debug("sleep residue not measured: compute processes unavailable", "uuid", spec.UUID, "err", err)
		return nil, false
	}
	own := map[string]int{}
	for _, index := range spec.GPUIndexes {
		bdf := ""
		for _, g := range gpus {
			if g.Index == index {
				bdf = g.PCIeBDF
				break
			}
		}
		if bdf == "" {
			// Without the card's address its processes cannot be told apart.
			return nil, false
		}
		var on []gpu.ComputeProcess
		for _, p := range procs {
			if gpu.SameBDF(p.PCIeBDF, bdf) {
				on = append(on, p)
			}
		}
		if len(on) > 1 {
			e.log.Debug("sleep residue not measured: other processes on the card", "uuid", spec.UUID, "gpu", index, "processes", len(on))
			return nil, false
		}
		if len(on) == 1 {
			own[accelerator.CardIdent("", bdf)] = on[0].PID
		}
	}
	return own, true
}
