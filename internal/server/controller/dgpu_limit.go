package controller

import (
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// dgpuLimitPushes holds the deployments whose dGPU memory limit was changed
// in place and whose rewritten instances are not yet handed to their nodes.
// Its zero value is ready to use.
type dgpuLimitPushes struct {
	mu      sync.Mutex
	pending map[uint]bool
}

// mark notes that dep's limit changed in place.
func (p *dgpuLimitPushes) mark(dep uint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = map[uint]bool{}
	}
	p.pending[dep] = true
}

// take reports whether dep has a push pending and clears it.
func (p *dgpuLimitPushes) take(dep uint) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	pending := p.pending[dep]
	delete(p.pending, dep)
	return pending
}

// forget drops dep's pending push: once it has stopped there are no running
// containers to hold to the new limit, and its instances carry the limit
// when they start again.
func (p *dgpuLimitPushes) forget(dep uint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, dep)
}

// pushDGPULimits hands the nodes of a deployment whose memory limit changed
// in place the desired state of each live slice, so the agent holds the
// running container to the limit it now carries (engine
// reconcilePartitionLimit). Only the change itself is pushed, once: an agent
// that is offline, or a server that restarts before the push, is covered by
// the full desired state every agent is handed when it connects. A failed
// push is retried.
func (c *Controller) pushDGPULimits(dep *store.Deployment, insts []store.Instance) {
	if !c.dgpuPushes.take(dep.ID) {
		return
	}
	byNode := map[uint][]proto.InstanceDesiredState{}
	for i := range insts {
		switch insts[i].State {
		case proto.InstStopped, proto.InstFailed, proto.InstPendingRemoval, proto.InstRemoving:
			continue
		}
		spec, err := insts[i].DesiredState()
		if err != nil || spec.VRAMLimitBytes <= 0 {
			continue
		}
		byNode[insts[i].NodeID] = append(byNode[insts[i].NodeID], spec)
	}
	for nodeID, specs := range byNode {
		node, err := c.store.NodeByID(nodeID)
		if err != nil {
			continue
		}
		if !c.hub.Online(node.UUID) {
			continue // its next connection hands it the full desired state
		}
		if err := c.hub.PushToNode(node.UUID, proto.StateUpdate{Instances: specs}); err != nil {
			c.log.Warn("dGPU limit push failed; will retry", "dep", dep.ID, "node", node.UUID, "err", err)
			c.dgpuPushes.mark(dep.ID)
			c.q.addAfter(depKey(dep.ID), c.retryBackoff)
			continue
		}
		c.log.Info("pushed dGPU memory limit", "dep", dep.ID, "node", node.UUID, "instances", len(specs), "vram_limit_bytes", dep.VRAMLimitBytes)
	}
}
