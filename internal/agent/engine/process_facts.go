package engine

// processFacts is what the agent learned about one instance's engine process,
// true only while that process lives. A container started later under the
// same UUID is a new process and knows none of it. The record is bounded by
// its process: it starts empty where a container is created (startOrAdopt,
// which also covers a dead container replaced without passing through
// reclaimInstance) and goes whole where a container is confirmed gone
// (reclaimInstance) and on a runtime switch; no exit path removes a part of
// it (ADR 0066).
type processFacts struct {
	// sleepLevel is the level the engine sleeps at; sleepKnown is false until
	// this agent has learned it (adoptSleepLevel asks an engine it does not
	// remember).
	sleepLevel int
	sleepKnown bool
	// prewarmed: the cold sleep/wake prewarm is done, or not wanted (an engine
	// adopted after an agent restart has already served).
	prewarmed bool
	// l1HostHeld: the engine completed an L1 sleep since its container
	// started, so it keeps that pinned host memory (holdsL1HostBuffer).
	l1HostHeld bool
	// sleepPIDs is, by card identity, the process the sleeping engine keeps
	// on each of its cards, learned when its residue was measured.
	sleepPIDs map[string]int
}

// processLocked is uuid's process record, made on first write. e.mu is held.
func (e *Engine) processLocked(uuid string) *processFacts {
	p := e.processes[uuid]
	if p == nil {
		p = &processFacts{}
		e.processes[uuid] = p
	}
	return p
}

// processOfLocked is a copy of uuid's process record, the zero record when
// the agent knows nothing of the process. e.mu is held.
func (e *Engine) processOfLocked(uuid string) processFacts {
	if p := e.processes[uuid]; p != nil {
		return *p
	}
	return processFacts{}
}

// setSleepLevelLocked records the level uuid's engine sleeps at. e.mu is held.
func (e *Engine) setSleepLevelLocked(uuid string, level int) {
	p := e.processLocked(uuid)
	p.sleepLevel, p.sleepKnown = level, true
}

// forgetProcess drops everything the agent knew about uuid's engine process,
// once its container is confirmed gone.
func (e *Engine) forgetProcess(uuid string) {
	e.mu.Lock()
	delete(e.processes, uuid)
	e.mu.Unlock()
}
