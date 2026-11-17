package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// rollRestart advances a rolling restart one replica at a time. While every
// replica is available (filled by a running, complete generation) it drains
// the oldest replica whose generation predates the request; the drained
// replica's slot opens and the launch below fills it with a new generation.
// Once no replica runs a generation from before the request, the restart is
// over. A replica that cannot come back holds the roll: taking another one
// down would leave two unavailable. One that spent its retry budget holds it
// until the operator retries it, and the timeline says so once per request.
func (c *Controller) rollRestart(dep *store.Deployment, insts []store.Instance, match planning.UnitMatch) error {
	since := *dep.RestartRequestedAt
	incomplete := planning.GroupsIncomplete(insts)
	var oldest *store.Instance
	var oldestUnit planning.Unit
	var stalled *planning.UnitSlot
	unavailable := 0
	for i, slot := range match.Slots {
		inst := slot.Inst
		if inst == nil || inst.State != proto.InstRunning || incomplete[inst.UUID] {
			unavailable++
			if inst == nil && len(planning.ChargedAttempts(match.Failed[slot.Key])) >= c.maxAttempts {
				stalled = &match.Slots[i]
			}
		}
		if inst != nil && inst.CreatedAt.Before(since) && (oldest == nil || inst.CreatedAt.Before(oldest.CreatedAt)) {
			oldest, oldestUnit = inst, slot.Unit
		}
	}
	if oldest == nil {
		c.log.Info("rolling restart done", "dep", dep.ID)
		return c.store.FinishRollingRestart(dep.ID, since)
	}
	if unavailable > 0 {
		if stalled != nil {
			c.recordRollStalled(dep, since, *stalled, match.Failed[stalled.Key])
		}
		c.q.addAfter(depKey(dep.ID), c.retryBackoff)
		return nil
	}
	if refusal := c.rollReplacementRefused(dep, oldestUnit, oldest); refusal != nil {
		// Its replacement could not be placed: keep the replica rather than
		// drop it, and say what would let the roll go on.
		c.recordUnschedulable(dep, *refusal)
		c.q.addAfter(depKey(dep.ID), c.retryBackoff)
		return nil
	}
	c.log.Info("rolling restart replaces a replica", "dep", dep.ID, "instance", oldest.UUID)
	// Only while it still fills its replica: an immediate restart of the same
	// replica that landed meanwhile has already removed it.
	err := c.store.TransitionInstances(dep.ID, []string{oldest.UUID}, store.ReplicaFillingStates(), proto.InstDraining, "rolling restart")
	if err != nil && !errors.Is(err, store.ErrStateChanged) {
		return err
	}
	c.q.add(depKey(dep.ID))
	return nil
}

// recordRollStalled notes, once per restart request, that the roll waits on a
// replica that spent its retry budget.
func (c *Controller) recordRollStalled(dep *store.Deployment, since time.Time, slot planning.UnitSlot, failed []store.Instance) {
	noted, err := c.store.HasDeploymentEventSince(dep.ID, store.EventDeploymentRollingStalled, since)
	if err != nil || noted {
		return
	}
	c.log.Warn("rolling restart waits on a replica that gave up", "dep", dep.ID, "role", slot.Unit.Role, "unit", slot.Unit.Index, "failed", len(failed))
	params := map[string]any{"role": slot.Unit.Role, "replica": slot.Unit.Index + 1, "attempts": len(failed)}
	// Where the failed attempts ran, read while they are still there.
	uuids := make([]string, 0, len(failed))
	for _, inst := range failed {
		uuids = append(uuids, inst.UUID)
	}
	if nodes, err := c.store.NodeUUIDsOfInstances(uuids); err == nil && len(nodes) > 0 {
		params[store.EventParamNodeUUIDs] = nodes
	}
	c.store.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, store.EventDeploymentRollingStalled, params)
}

// rollReplacementRefused checks, before a rolling restart drains a
// partition that shares its card with processes outside the platform,
// whether its replacement could be placed on its node without it. A node
// whose placement policy stopped allowing such sharing after the replica was
// placed would refuse it (ADR 0068): the refusal is returned and the replica
// kept. Anything else - another reason, a check that cannot run - lets the
// roll go on as before; only this case is one the roll itself would cause.
// The check plans one node, and only for a replica on a held card that
// cannot move: a pool replica whose requirements do not pin it to one node
// is replaced by a pick across the pool (pickPoolNode), so its own node's
// refusal says nothing about where the replacement goes.
func (c *Controller) rollReplacementRefused(dep *store.Deployment, unit planning.Unit, inst *store.Instance) *scheduler.Reason {
	if dep.VRAMLimitBytes <= 0 || dep.NNodes > 1 {
		return nil
	}
	node, err := c.store.NodeByID(inst.NodeID)
	if err != nil || !onHeldCard(node, inst) {
		return nil
	}
	if unit.Pool() {
		reqs, err := c.store.DeploymentRequirements(dep.ID)
		if pinned, ok := planning.PinnedNode(reqs); err != nil || !ok || pinned != node.UUID {
			return nil
		}
	}
	model, err := c.store.GetModel(dep.ModelID)
	if err != nil {
		return nil
	}
	req, templateLayer, _, err := c.launchRequest(dep, node, model, nil)
	if err != nil {
		return nil
	}
	topology, err := planning.RankTopology(node, nil, planning.GroupRank{Ranks: 1})
	if err != nil {
		return nil
	}
	req.Preview, req.Topology, req.ExcludeInstanceUUID = true, topology, inst.UUID
	_, refusal, err := c.planInstanceLaunch(context.Background(), dep, node, req, templateLayer)
	if err != nil || refusal == nil || refusal.Code != scheduler.ReasonHeldOutsidePlatformNotAllowed {
		return nil
	}
	return refusal
}

// onHeldCard is whether the instance runs on a card processes outside the
// platform hold.
func onHeldCard(node *store.Node, inst *store.Instance) bool {
	var indexes []int
	if json.Unmarshal([]byte(inst.GPUIndexes), &indexes) != nil {
		return false
	}
	for _, g := range node.GPUs {
		if g.HeldOutsidePlatform() && slices.Contains(indexes, g.Index) {
			return true
		}
	}
	return false
}
