package controller

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// EventNodeDrainMoved is on a multi-node or PD deployment whose unit moved
// off a draining node.
const EventNodeDrainMoved = "NODE_DRAIN_MOVED"

// drainSurge is a multi-node or PD unit being moved off a draining node:
// the replacement starts next to the unit (surge), and the unit is rewritten
// onto it once it runs, so service never drops. It lives in memory; after a
// server restart the replacement no unit claims is drained and the move is
// chosen again.
type drainSurge struct {
	Role  string
	Index int
	From  planning.PDUnit
	To    planning.PDUnit
}

func (f *nodeFailover) surges(dep uint) []drainSurge {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]drainSurge(nil), f.drains[dep]...)
}

func (f *nodeFailover) setSurges(dep uint, surges []drainSurge) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.drains == nil {
		f.drains = map[uint][]drainSurge{}
	}
	if len(surges) == 0 {
		delete(f.drains, dep)
		return
	}
	f.drains[dep] = surges
}

// drainNamedUnits starts, for a multi-node or PD unit with a rank on a
// draining node, a replacement on other nodes next to it - one unit at a
// time per deployment - and returns the units to reconcile with the
// replacement added. With no node to move to, the unit stays and waits.
func (c *Controller) drainNamedUnits(ctx context.Context, deployment *unitDeployment, units []planning.Unit) ([]planning.Unit, error) {
	dep := deployment.dep
	if len(units) == 0 || units[0].Pool() {
		return units, nil
	}
	surges := c.failover.surges(dep.ID)
	current := c.currentUnits(deployment)
	// A recorded move whose unit was edited since is history.
	var live []drainSurge
	for _, surge := range surges {
		if list := current[surge.Role]; surge.Index < len(list) && sameUnit(list[surge.Index], surge.From) {
			live = append(live, surge)
		}
	}
	if len(live) == 0 {
		draining, err := c.store.DrainingNodeIDs()
		if err != nil || len(draining) == 0 {
			c.failover.setSurges(dep.ID, nil)
			return units, err
		}
		if surge, ok := c.chooseDrainSurge(ctx, deployment, current, draining); ok {
			live = append(live, surge)
		}
	}
	c.failover.setSurges(dep.ID, live)
	out := append([]planning.Unit(nil), units...)
	for _, surge := range live {
		// The replacement's nodes are not the deployment's yet: the pass
		// launches and matches on them all the same.
		for _, rank := range surge.To.Ranks() {
			if deployment.nodes[rank.NodeUUID] != nil {
				continue
			}
			node, err := c.store.NodeWithGPUs(rank.NodeUUID)
			if err != nil {
				return units, err
			}
			deployment.nodes[node.UUID] = node
			deployment.nodeUUIDs[node.ID] = node.UUID
		}
		unit := planning.Unit{Role: surge.Role, Index: len(current[surge.Role]) + len(deployment.surgeUnits[surge.Role])}
		for _, rank := range surge.To.Ranks() {
			unit.Ranks = append(unit.Ranks, planning.UnitRank{Nodes: []string{rank.NodeUUID}, GPUs: rank.GPUIndexes})
		}
		out = append(out, unit)
		if deployment.pd != nil {
			// A PD launch reads the unit it starts from the placement; the
			// replacement is not in it until the move is written.
			if deployment.surgeUnits == nil {
				deployment.surgeUnits = map[string][]planning.PDUnit{}
			}
			deployment.surgeUnits[surge.Role] = append(deployment.surgeUnits[surge.Role], surge.To)
		}
	}
	return out, nil
}

// chooseDrainSurge picks the first unit with a rank on a draining node that
// can move whole or rank by rank, and where its ranks go.
func (c *Controller) chooseDrainSurge(ctx context.Context, deployment *unitDeployment, current map[string][]planning.PDUnit, draining map[uint]bool) (drainSurge, bool) {
	leaving := map[string]bool{}
	for uuid, node := range deployment.nodes {
		if draining[node.ID] {
			leaving[uuid] = true
		}
	}
	if len(leaving) == 0 {
		return drainSurge{}, false
	}
	model, err := c.store.GetModel(deployment.dep.ModelID)
	if err != nil {
		return drainSurge{}, false
	}
	var listed []planning.ListedRank
	for role, list := range current {
		for _, unit := range list {
			gpus := c.rankGPUs(deployment, role, unit)
			for i, rank := range unit.Ranks() {
				listed = append(listed, planning.ListedRank{NodeUUID: rank.NodeUUID, GPUs: gpus, Leader: i == 0 && deployment.pd == nil})
			}
		}
	}
	facts, err := c.unitFacts(deployment, listed, 1)
	if err != nil {
		return drainSurge{}, false
	}
	nodes := map[string]*store.Node{}
	for i := range facts.Nodes {
		nodes[facts.Nodes[i].UUID] = &facts.Nodes[i]
	}
	for _, role := range c.roleOrder(deployment) {
		for index, unit := range current[role] {
			var moving []int
			for i, rank := range unit.Ranks() {
				if leaving[rank.NodeUUID] {
					moving = append(moving, i)
				}
			}
			if len(moving) == 0 {
				continue
			}
			facts.PerRank = c.rankGPUs(deployment, role, unit)
			// The replacement runs next to the unit until it takes over. A
			// PD unit's generations each have ports of their own, so only
			// its leaving ranks move; an ordinary multi-node replica's
			// leader holds the deployment's master port, so its replacement
			// is chosen whole and led from another node.
			var to planning.PDUnit
			ok := false
			if deployment.pd != nil {
				to, _, ok = c.replaceRanks(ctx, deployment, model, facts, nodes, role, index, unit, moving, leaving)
			} else {
				to, _, ok = c.replaceUnitWhole(ctx, deployment, model, facts, role, index, unit, leaving, true)
			}
			if !ok || sameUnit(to, unit) {
				continue
			}
			c.log.Info("unit moving off a draining node", "dep", deployment.dep.ID, "role", role, "unit", index)
			return drainSurge{Role: role, Index: index, From: unit, To: to}, true
		}
	}
	c.q.addAfter(depKey(deployment.dep.ID), time.Minute)
	return drainSurge{}, false
}

// completeDrainSurge rewrites a unit onto its running replacement: the old
// generation no longer fills any unit and drains, the replacement already
// runs the rewritten one. It reports whether it rewrote the placement.
func (c *Controller) completeDrainSurge(deployment *unitDeployment, match planning.UnitMatch) (bool, error) {
	dep := deployment.dep
	surges := c.failover.surges(dep.ID)
	if len(surges) == 0 {
		return false, nil
	}
	current := c.currentUnits(deployment)
	var keep []drainSurge
	changed := false
	for _, surge := range surges {
		if !surgeRunning(match, surge) {
			keep = append(keep, surge)
			continue
		}
		current[surge.Role][surge.Index] = surge.To
		changed = true
		params := map[string]any{"unit": surge.Index + 1, store.EventParamNodeUUIDs: nodeUUIDList(append(rankUUIDs(surge.From), rankUUIDs(surge.To)...)...)}
		if surge.Role != "" {
			params["role"] = surge.Role
		}
		c.store.AddDeploymentEventCode(dep.ID, nodeReplaceEventKind, EventNodeDrainMoved, params)
	}
	if !changed {
		return false, nil
	}
	if err := c.writeUnits(dep, deployment, current); err != nil {
		c.log.Warn("drain move could not be written", "dep", dep.ID, "err", err)
		return false, nil
	}
	c.failover.setSurges(dep.ID, keep)
	c.q.add(depKey(dep.ID))
	return true, nil
}

// surgeRunning reports whether the replacement unit's leader runs.
func surgeRunning(match planning.UnitMatch, surge drainSurge) bool {
	for _, slot := range match.Slots {
		if slot.Unit.Role != surge.Role || len(slot.Unit.Ranks) != len(surge.To.Ranks()) {
			continue
		}
		same := true
		for i, rank := range surge.To.Ranks() {
			if len(slot.Unit.Ranks[i].Nodes) != 1 || slot.Unit.Ranks[i].Nodes[0] != rank.NodeUUID {
				same = false
				break
			}
		}
		if same && slot.Inst != nil && slot.Inst.State == proto.InstRunning {
			return true
		}
	}
	return false
}

// currentUnits is the deployment's units as its placement lists them, by role.
func (c *Controller) currentUnits(deployment *unitDeployment) map[string][]planning.PDUnit {
	units := map[string][]planning.PDUnit{}
	if deployment.pd != nil {
		for _, role := range deployment.pd.mode.Roles {
			units[role] = append([]planning.PDUnit(nil), deployment.pd.placement.Units(role)...)
		}
		return units
	}
	units[""] = planning.ReplicaUnitsOf(deployment.dep, deploymentNodeOrder(deployment))
	return units
}

func (c *Controller) roleOrder(deployment *unitDeployment) []string {
	if deployment.pd != nil {
		return deployment.pd.mode.Roles
	}
	return []string{""}
}
