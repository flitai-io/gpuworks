package controller

import (
	"maps"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// sleepOrder is the part of an instance's desired state a sleep push carries.
type sleepOrder struct {
	level int
	mode  string
}

// syncSleep pushes each instance's sleep intent to its node when the agent
// has not been handed it yet. The intent lives in the instance's desired state
// (the store writes it for deployment-wide and per-instance orders alike); the
// controller remembers what it last pushed per instance. An instance it has
// not pushed to in this process is pushed only when its reported level differs
// from the intent: a server restart reconnects every agent, and the reconnect
// hands each one its full desired state.
func (c *Controller) syncSleep(dep *store.Deployment, insts []store.Instance) {
	c.sleepMu.Lock()
	pushed := maps.Clone(c.sleepPushed[dep.ID])
	c.sleepMu.Unlock()
	next := map[string]sleepOrder{}
	updatesByNode := map[string][]proto.InstanceDesiredState{}
	for i := range insts {
		in := &insts[i]
		if !store.HoldsSleepIntent(in) {
			continue
		}
		spec, err := in.DesiredState()
		if err != nil {
			c.log.Warn("sleep intent not pushed: desired state unreadable", "instance", in.UUID, "err", err)
			continue
		}
		order := sleepOrder{level: spec.DesiredSleep, mode: spec.SleepMode}
		last, seen := pushed[in.UUID]
		if (seen && last == order) || (!seen && order.level == in.SleepLevel) {
			next[in.UUID] = order
			continue
		}
		if seen {
			next[in.UUID] = last
		}
		node, err := c.store.NodeByID(in.NodeID)
		if err != nil {
			continue
		}
		updatesByNode[node.UUID] = append(updatesByNode[node.UUID], spec)
	}
	for nodeUUID, updates := range updatesByNode {
		if err := c.hub.PushToNode(nodeUUID, proto.StateUpdate{Instances: updates}); err != nil {
			c.log.Warn("sleep push failed; will retry", "dep", dep.ID, "node", nodeUUID, "err", err)
			c.q.addAfter(depKey(dep.ID), c.retryBackoff)
			continue
		}
		for _, spec := range updates {
			next[spec.UUID] = sleepOrder{level: spec.DesiredSleep, mode: spec.SleepMode}
			c.log.Info("pushed sleep level", "dep", dep.ID, "instance", spec.UUID, "level", spec.DesiredSleep, "node", nodeUUID)
		}
	}
	c.sleepMu.Lock()
	c.sleepPushed[dep.ID] = next
	c.sleepMu.Unlock()
}

// recordSleepPushed notes the sleep intent a launch push carried, so a sleep
// order written between the instance's creation and its push is pushed again.
func (c *Controller) recordSleepPushed(depID uint, spec proto.InstanceDesiredState) {
	c.sleepMu.Lock()
	defer c.sleepMu.Unlock()
	if c.sleepPushed[depID] == nil {
		c.sleepPushed[depID] = map[string]sleepOrder{}
	}
	c.sleepPushed[depID][spec.UUID] = sleepOrder{level: spec.DesiredSleep, mode: spec.SleepMode}
}

// forgetSleepPushes drops the sleep intents pushed for a deployment's
// instances.
func (c *Controller) forgetSleepPushes(depID uint) {
	c.sleepMu.Lock()
	defer c.sleepMu.Unlock()
	delete(c.sleepPushed, depID)
}
