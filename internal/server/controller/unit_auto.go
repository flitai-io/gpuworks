package controller

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Automatic placement of a multi-node or PD deployment's units
// (store.Deployment.UnitsAuto). The platform picks a unit's nodes whenever
// the unit starts, preferring the nodes it names - the ones it ran on last.
// A unit whose nodes are all online and can take it again keeps them. One
// whose node stopped taking replicas, no longer satisfies the deployment's
// node requirements, or would be refused at launch (its cards taken by
// others) has those ranks moved to other nodes, with the same candidates,
// trial plans and write as node replacement; its other ranks stay. A node
// that is offline or gone is node replacement's, with its wait.

// EventUnitPlaced records an automatic re-pick of a unit's nodes.
const EventUnitPlaced = "UNIT_PLACED"

// autoRepickRetry is how long a deployment whose re-pick found no nodes
// waits before reading every node again for another.
const autoRepickRetry = time.Minute

// autoPlaceUnit re-picks the nodes of a unit about to start, for a deployment
// the platform places, when the nodes it names cannot take it. It reports
// whether it rewrote the deployment's units; the pass then ends and the next
// one launches on the new nodes.
func (c *Controller) autoPlaceUnit(ctx context.Context, deployment *unitDeployment, model *store.Model, slot planning.UnitSlot) (bool, error) {
	dep := deployment.dep
	if !dep.UnitsAuto || slot.Unit.Pool() {
		return false, nil
	}
	role, index := slot.Unit.Role, slot.Unit.Index
	current := c.currentUnits(deployment)
	if index >= len(current[role]) {
		// A drain's replacement, not yet written: drainNamedUnits owns it.
		return false, nil
	}
	unit := current[role][index]
	nodes := map[string]*store.Node{}
	for _, rank := range unit.Ranks() {
		node := deployment.nodes[rank.NodeUUID]
		if node == nil || !c.hub.Online(node.UUID) {
			return false, nil
		}
		if leavingOn(deployment.insts, node.ID) {
			// The deployment's own instances are still leaving this node
			// (a restart, a retired attempt): their cards come back.
			return false, nil
		}
		nodes[node.UUID] = node
	}
	unfit := map[string]bool{}
	var unfitRanks []int
	var reason *scheduler.Reason
	for i, rank := range unit.Ranks() {
		node := nodes[rank.NodeUUID]
		switch {
		case !node.Scheduling.TakesReplicas():
			reason = &scheduler.Reason{Code: scheduler.ReasonNodeCordoned, Params: map[string]any{"node": node.DisplayName()}}
		case c.matchDeploymentRequirements(deployment, node) != nil:
			reason = &scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"node": node.DisplayName()}}
		default:
			continue
		}
		unfit[node.UUID] = true
		unfitRanks = append(unfitRanks, i)
	}
	if len(unfitRanks) == 0 {
		refused, why, err := c.unitTrial(deployment, model).Unit(ctx, role, planning.PDUnit{}, unit, nodes)
		if err != nil || refused == "" {
			return false, err
		}
		reason = why
		unfit[refused] = true
		for i, rank := range unit.Ranks() {
			if rank.NodeUUID == refused {
				unfitRanks = append(unfitRanks, i)
			}
		}
	}
	if !c.autoRepickDue(dep.ID) {
		return false, nil
	}
	moved, ok, err := c.repickUnit(ctx, deployment, model, role, index, unit, unfitRanks, unfit)
	if err != nil {
		return false, err
	}
	if !ok {
		c.autoRepickFailed(dep.ID)
		return false, nil
	}
	current[role][index] = moved
	if err := c.writeUnits(dep, deployment, current); err != nil {
		c.log.Warn("automatic placement could not be written", "dep", dep.ID, "err", err)
		c.autoRepickFailed(dep.ID)
		return false, nil
	}
	params := map[string]any{"unit": index + 1, "from": c.rankNames(deployment, unit), "to": c.rankNames(deployment, moved),
		store.EventParamNodeUUIDs: nodeUUIDList(append(rankUUIDs(unit), rankUUIDs(moved)...)...)}
	if role != "" {
		params["role"] = role
	}
	if reason != nil {
		params["reason"] = reason.Code
	}
	c.store.AddDeploymentEventCode(dep.ID, nodeReplaceEventKind, EventUnitPlaced, params)
	c.log.Info("unit placed on other nodes", "dep", dep.ID, "role", role, "unit", index, "from", unit.Ranks(), "to", moved.Ranks())
	c.q.add(depKey(dep.ID))
	return true, nil
}

// repickUnit moves the ranks of unit at unfitRanks to other nodes - the
// candidates node replacement uses, in the deployment's strategy's order -
// and, for an ordinary multi-node replica whose ranks cannot be moved one by
// one, chooses the whole replica again.
func (c *Controller) repickUnit(ctx context.Context, deployment *unitDeployment, model *store.Model, role string, index int, unit planning.PDUnit, unfitRanks []int, unfit map[string]bool) (planning.PDUnit, bool, error) {
	dep := deployment.dep
	var listed []planning.ListedRank
	for _, r := range c.roleOrder(deployment) {
		for _, u := range c.currentUnits(deployment)[r] {
			gpus := c.rankGPUs(deployment, r, u)
			for i, rank := range u.Ranks() {
				listed = append(listed, planning.ListedRank{NodeUUID: rank.NodeUUID, GPUs: gpus, Leader: i == 0 && deployment.pd == nil})
			}
		}
	}
	facts, err := c.unitFacts(deployment, listed, c.rankGPUs(deployment, role, unit))
	if err != nil {
		return unit, false, err
	}
	facts.Strategy = dep.PlacementStrategy
	byUUID := map[string]*store.Node{}
	for i := range facts.Nodes {
		byUUID[facts.Nodes[i].UUID] = &facts.Nodes[i]
	}
	moved, _, ok := c.replaceRanks(ctx, deployment, model, facts, byUUID, role, index, unit, unfitRanks, unfit)
	if !ok && deployment.pd == nil && len(unit.Ranks()) > 1 {
		moved, _, ok = c.replaceUnitWhole(ctx, deployment, model, facts, role, index, unit, unfit, false)
	}
	return moved, ok, nil
}

// leavingOn reports whether one of the deployment's instances is still
// leaving the node: draining or being removed.
func leavingOn(insts []store.Instance, nodeID uint) bool {
	for _, inst := range insts {
		if inst.NodeID != nodeID {
			continue
		}
		switch inst.State {
		case proto.InstDraining, proto.InstRemoving, proto.InstPendingRemoval:
			return true
		}
	}
	return false
}

// rankNames are the display names of a unit's nodes, by rank.
func (c *Controller) rankNames(deployment *unitDeployment, unit planning.PDUnit) []string {
	var out []string
	for _, rank := range unit.Ranks() {
		out = append(out, c.nodeName(deployment, rank.NodeUUID))
	}
	return out
}

// autoRepickDue reports whether a deployment may look for other nodes now:
// one whose last look found none waits autoRepickRetry, so a unit that cannot
// start does not read every node on every pass.
func (c *Controller) autoRepickDue(dep uint) bool {
	c.failover.mu.Lock()
	defer c.failover.mu.Unlock()
	last, ok := c.failover.repicked[dep]
	if ok && c.now().Sub(last) < autoRepickRetry {
		c.q.addAfter(depKey(dep), autoRepickRetry-c.now().Sub(last))
		return false
	}
	return true
}

func (c *Controller) autoRepickFailed(dep uint) {
	c.failover.mu.Lock()
	defer c.failover.mu.Unlock()
	if c.failover.repicked == nil {
		c.failover.repicked = map[uint]time.Time{}
	}
	c.failover.repicked[dep] = c.now()
	c.q.addAfter(depKey(dep), autoRepickRetry)
}
