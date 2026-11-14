package controller

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// markDGPULimitPush notes that a deployment's dGPU memory limit was changed
// in place: the instances it rewrote are to be handed to their nodes.
func (c *Controller) markDGPULimitPush(depID uint) {
	c.dgpuMu.Lock()
	defer c.dgpuMu.Unlock()
	if c.dgpuPending == nil {
		c.dgpuPending = map[uint]bool{}
	}
	c.dgpuPending[depID] = true
}

// forgetDGPULimitPush drops a deployment's pending limit push: once it has
// stopped there are no running containers to hold to the new limit, and its
// instances carry the limit when they start again.
func (c *Controller) forgetDGPULimitPush(depID uint) {
	c.dgpuMu.Lock()
	defer c.dgpuMu.Unlock()
	delete(c.dgpuPending, depID)
}

// pushDGPULimits hands the nodes of a deployment whose memory limit changed
// in place the desired state of each live slice, so the agent holds the
// running container to the limit it now carries (engine
// reconcilePartitionLimit). Only the change itself is pushed, once: an agent
// that is offline, or a server that restarts before the push, is covered by
// the full desired state every agent is handed when it connects. A failed
// push is retried.
func (c *Controller) pushDGPULimits(dep *store.Deployment, insts []store.Instance) {
	c.dgpuMu.Lock()
	pending := c.dgpuPending[dep.ID]
	delete(c.dgpuPending, dep.ID)
	c.dgpuMu.Unlock()
	if !pending {
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
			c.markDGPULimitPush(dep.ID)
			c.q.addAfter(depKey(dep.ID), c.retryBackoff)
			continue
		}
		c.log.Info("pushed dGPU memory limit", "dep", dep.ID, "node", node.UUID, "instances", len(specs), "vram_limit_bytes", dep.VRAMLimitBytes)
	}
}
