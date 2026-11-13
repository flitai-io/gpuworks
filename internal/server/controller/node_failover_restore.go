package controller

import (
	"encoding/json"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A server that has just started reads node replacement's moves back from
// the events it wrote when it made them, so a lost node that returns before
// its moved unit ran on the new node still takes the unit back, as it would
// have without the restart. Nothing is stored for it: NODE_REPLACED names the
// unit, the rank and both nodes, and NODE_REPLACE_REVERTED the undoing.
//
// A move is read back only while the facts still say it may be undone:
//
//   - the newest event for its unit and rank is the move, not a revert;
//   - the placement still has that rank on the node it moved to (an edit
//     since leaves the move as history, as revertFailovers does);
//   - no instance of the deployment has run on that node since the move.
//     The record in memory is dropped the first time the unit runs there, so
//     a unit that ran and stopped since must not be moved back. The check is
//     by node, not by unit: another unit of the deployment running on the
//     same node keeps the move from being read back, which leaves the
//     placement as it is - the behaviour before this read-back existed.

// failoverRestoreEvents bounds the events read per deployment: a deployment
// has a move or a revert for each rank a lost node made it move, and a few
// dozen are more than a fleet's node failures leave pending at once.
const failoverRestoreEvents = 100

// moveKey names a unit's rank.
func moveKey(role string, unit, rank int) string { return fmt.Sprintf("%s/%d/%d", role, unit, rank) }

// failoverEventParams is what restoreFailovers reads of a NODE_REPLACED or
// NODE_REPLACE_REVERTED event: the unit is 1-based, the node UUIDs are
// [from, to] of the move (of the revert, for a revert).
type failoverEventParams struct {
	Unit      int      `json:"unit"`
	Rank      int      `json:"rank"`
	Role      string   `json:"role"`
	NodeUUIDs []string `json:"node_uuids"`
}

// restoreFailovers reads the deployment's moves back once per process, on
// the first pass that looks at them. A failed read is tried on the next pass.
// units are the deployment's units now, by role.
func (c *Controller) restoreFailovers(dep *store.Deployment, deployment *unitDeployment, units map[string][]planning.PDUnit) {
	c.failover.mu.Lock()
	done := c.failover.restored[dep.ID]
	c.failover.mu.Unlock()
	if done {
		return
	}
	evs, err := c.store.DeploymentEventsByCode(dep.ID, []string{EventNodeReplaced, EventNodeReplaceReverted}, failoverRestoreEvents)
	if err != nil {
		c.log.Warn("node replacement moves could not be read back", "dep", dep.ID, "err", err)
		return
	}
	c.failover.mu.Lock()
	if c.failover.restored == nil {
		c.failover.restored = map[uint]bool{}
	}
	c.failover.restored[dep.ID] = true
	c.failover.mu.Unlock()

	moves := c.failover.get(dep.ID)
	have := map[string]bool{}
	for _, m := range moves {
		have[moveKey(m.Role, m.Unit, m.Rank)] = true
	}
	settled := map[string]bool{}
	added := 0
	for _, ev := range evs { // newest first: the newest event of a rank decides it
		var p failoverEventParams
		if json.Unmarshal([]byte(ev.Data), &p) != nil || p.Unit < 1 || len(p.NodeUUIDs) != 2 {
			continue
		}
		index := p.Unit - 1
		key := moveKey(p.Role, index, p.Rank)
		if settled[key] {
			continue
		}
		settled[key] = true
		if ev.Code != EventNodeReplaced || have[key] {
			continue
		}
		from, to := p.NodeUUIDs[0], p.NodeUUIDs[1]
		list := units[p.Role]
		if index >= len(list) {
			continue
		}
		ranks := list[index].Ranks()
		if p.Rank < 0 || p.Rank >= len(ranks) || ranks[p.Rank].NodeUUID != to {
			continue
		}
		if c.ranOnSince(dep.ID, deployment, to, ev.CreatedAt) {
			continue
		}
		moves = append(moves, failoverMove{Role: p.Role, Unit: index, Rank: p.Rank, From: from, To: to, MovedAt: ev.CreatedAt, unitAfter: list[index]})
		added++
	}
	if added > 0 {
		c.failover.set(dep.ID, moves)
		c.log.Info("node replacement moves read back from the events", "dep", dep.ID, "moves", added)
	}
}

// ranOnSince says an instance of the deployment served (or slept, having
// served) on the node at some time since at, as the residency timeline says.
// Anything it cannot tell reads as ran: the move is then not read back.
func (c *Controller) ranOnSince(depID uint, deployment *unitDeployment, nodeUUID string, at time.Time) bool {
	node := deployment.nodes[nodeUUID]
	if node == nil {
		return true
	}
	insts, events, err := c.store.ResidencyInputsOf([]uint{depID}, at)
	if err != nil {
		return true
	}
	now := c.now()
	for _, inst := range insts {
		if inst.NodeID != node.ID {
			continue
		}
		for _, seg := range store.DeriveResidency(inst, events[inst.EventsOf()], at, now) {
			if seg.State == store.ResidencyAwake || seg.State == store.ResidencyAsleep {
				return true
			}
		}
	}
	return false
}
