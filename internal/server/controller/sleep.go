package controller

import (
	"maps"
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// sleepOrder is the part of an instance's desired state a sleep push carries.
type sleepOrder struct {
	level int
	mode  string
}

// sleepPushes is the sleep intent last pushed to each instance, by
// deployment. Its zero value is ready to use.
type sleepPushes struct {
	mu    sync.Mutex
	byDep map[uint]map[string]sleepOrder
}

// last is a copy of what was last pushed to dep's instances, by instance.
func (p *sleepPushes) last(dep uint) map[string]sleepOrder {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.byDep[dep])
}

// set replaces what was pushed to dep's instances.
func (p *sleepPushes) set(dep uint, pushed map[string]sleepOrder) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byDep == nil {
		p.byDep = map[uint]map[string]sleepOrder{}
	}
	p.byDep[dep] = pushed
}

// record notes the order pushed to one of dep's instances.
func (p *sleepPushes) record(dep uint, instance string, order sleepOrder) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byDep == nil {
		p.byDep = map[uint]map[string]sleepOrder{}
	}
	if p.byDep[dep] == nil {
		p.byDep[dep] = map[string]sleepOrder{}
	}
	p.byDep[dep][instance] = order
}

// forget drops what was pushed to dep's instances.
func (p *sleepPushes) forget(dep uint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byDep, dep)
}

// syncSleep pushes each instance's sleep intent to its node when the agent
// has not been handed it yet. The intent lives in the instance's desired state
// (the store writes it for deployment-wide and per-instance orders alike); the
// controller remembers what it last pushed per instance. An instance it has
// not pushed to in this process is pushed only when its reported level differs
// from the intent: a server restart reconnects every agent, and the reconnect
// hands each one its full desired state.
func (c *Controller) syncSleep(dep *store.Deployment, insts []store.Instance) {
	pushed := c.sleepPushes.last(dep.ID)
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
	c.sleepPushes.set(dep.ID, next)
}

// recordSleepPushed notes the sleep intent a launch push carried, so a sleep
// order written between the instance's creation and its push is pushed again.
func (c *Controller) recordSleepPushed(depID uint, spec proto.InstanceDesiredState) {
	c.sleepPushes.record(depID, spec.UUID, sleepOrder{level: spec.DesiredSleep, mode: spec.SleepMode})
}
