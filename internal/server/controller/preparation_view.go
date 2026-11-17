package controller

import (
	"slices"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// preparationPlacements are the nodes a deployment's pass actually chose
// before any of their instances exist: where its weights are being prepared.
// They are an observation, never a placement instruction. The deployment's
// UpdatedAt versions an entry, so any edit hides it until the pass that the
// edit's own event queues; forgetDeployment drops it.
//
// generation counts the changes of an entry. A pass sets its choice again
// on every reconcile; only a different node set (or version) counts, so a
// reader comparing generations sees each real change and nothing else. No
// bus event is published for it: a deployment event would queue the very
// deployment whose pass set it.
type preparationPlacements struct {
	mu         sync.Mutex
	nodes      map[uint]preparationPlacement
	generation uint64
}

type preparationPlacement struct {
	version time.Time
	nodes   []uint
}

func (p *preparationPlacements) set(dep *store.Deployment, nodes []uint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	was, had := p.nodes[dep.ID]
	if len(nodes) == 0 {
		if had {
			delete(p.nodes, dep.ID)
			p.generation++
		}
		return
	}
	if had && was.version.Equal(dep.UpdatedAt) && slices.Equal(was.nodes, nodes) {
		return
	}
	if p.nodes == nil {
		p.nodes = map[uint]preparationPlacement{}
	}
	p.nodes[dep.ID] = preparationPlacement{dep.UpdatedAt, append([]uint(nil), nodes...)}
	p.generation++
}

func (p *preparationPlacements) get(dep *store.Deployment) []uint {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.nodes[dep.ID]
	if !ok || !entry.version.Equal(dep.UpdatedAt) {
		return nil
	}
	return append([]uint(nil), entry.nodes...)
}

func (p *preparationPlacements) forget(id uint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, had := p.nodes[id]; had {
		delete(p.nodes, id)
		p.generation++
	}
}

func (p *preparationPlacements) changes() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.generation
}

// Placements reads the nodes the controller's passes chose for instances to
// come.
type Placements struct{ p *preparationPlacements }

// Placements is the read side of the controller's preparation placements.
func (c *Controller) Placements() Placements { return Placements{&c.placements} }

// Nodes are the nodes the deployment's last pass chose and is preparing
// weights on; nil before a pass chose any.
func (v Placements) Nodes(dep *store.Deployment) []uint { return v.p.get(dep) }

// Generation counts the changes of what Nodes answers; it moves only when a
// deployment's chosen nodes actually change.
func (v Placements) Generation() uint64 { return v.p.changes() }

// CachePhase is the eviction phase the controller is in for a cache entry it
// is making room for, or "" (prepobs.PhaseEvicting / PhaseEvictionFailed).
func (c *Controller) CachePhase(ref store.FileRef) string {
	return c.evictions.Get(ref, c.now())
}
