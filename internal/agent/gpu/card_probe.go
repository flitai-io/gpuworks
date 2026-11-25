package gpu

import (
	"context"
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
)

// probeCards is the part of Probe every vendor whose cards are read from a
// tool has in common. A host without the tool is not a failed probe: it has
// no driver for these cards, and the cards plugged in are still reported
// (Roster). list returns the cards the tool lists, each complete (identity,
// memory, health finding); an error from it means the tool could not be
// asked, so nothing is known about any card and whatever it returned beside
// the error is dropped.
//
// Only an answer replaces baseline: a tool that stops answering says nothing
// about the cards having changed. baseline is nil for a vendor that compares
// nothing later.
func probeCards(
	ctx context.Context, vendor, hostRoot string, installed bool,
	list func(context.Context) ([]proto.GPUInfo, error), baseline *cardBaseline,
) ([]proto.GPUInfo, error) {
	if !installed {
		return Roster(vendor, hostRoot, nil, false), nil
	}
	gpus, err := list(ctx)
	if err != nil {
		return Roster(vendor, hostRoot, nil, false), err
	}
	baseline.publish(gpus)
	return Roster(vendor, hostRoot, gpus, true), nil
}

// cardBaseline is what each card was when the tool last listed it, by the
// tool's own index. Injection and metrics run later and ask the tool again;
// an index that now names another card (one was passed through to a virtual
// machine and the rest were renumbered) is caught by comparing with this.
//
// The zero value is ready to use. A published map is never written again, so
// a reader may keep what all returns after the lock is released.
type cardBaseline struct {
	mu    sync.RWMutex
	cards map[int]proto.GPUInfo
}

// publish replaces the baseline with the cards of one listing. A card the
// tool gave no index (-1) has no place in it. A nil baseline keeps nothing.
func (b *cardBaseline) publish(gpus []proto.GPUInfo) {
	if b == nil {
		return
	}
	cards := make(map[int]proto.GPUInfo, len(gpus))
	for _, g := range gpus {
		if g.Index >= 0 {
			cards[g.Index] = g
		}
	}
	b.mu.Lock()
	b.cards = cards
	b.mu.Unlock()
}

// at returns the card the last listing had at index.
func (b *cardBaseline) at(index int) (proto.GPUInfo, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	g, ok := b.cards[index]
	return g, ok
}

// all returns the last listing's cards by index; the map must not be
// written.
func (b *cardBaseline) all() map[int]proto.GPUInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cards
}
