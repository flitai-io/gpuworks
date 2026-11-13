package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// unitDeployment is an active deployment prepared for reconcile: its units
// (planning.DeploymentUnits), the nodes they name and, for a PD deployment,
// its mode and placement.
type unitDeployment struct {
	dep       *store.Deployment
	units     []planning.Unit
	nodes     map[string]*store.Node
	nodeUUIDs map[uint]string
	// members are the deployment's nodes in order. A PD deployment resolves
	// its compute network over all of them (its roles exchange KV); a
	// multi-node replica resolves its own, over its ranks.
	members []store.Node
	pd      *pdDeployment
	// requirements bound which nodes may run the deployment's replicas,
	// rechecked on every launch (MatchRequirements).
	requirements nodelabel.Requirements
	// insts are the deployment's instances this pass reconciles; pool is its
	// candidate pool, read when a pool replica first needs a node.
	insts []store.Instance
	pool  *unitPool
	// surgeUnits are, by role, the PD units a drain starts on other nodes
	// this pass: they follow the placement's own units until the move is
	// written (drainNamedUnits).
	surgeUnits map[string][]planning.PDUnit
}

// pdDeployment is what a PD deployment adds: its mode, its placement and the
// network its instances pair over.
type pdDeployment struct {
	mode      pdmode.Mode
	placement *planning.PDPlacement
	group     netplan.Group
}

// reconcileUnits keeps every unit of an active deployment filled, whatever
// its shape (ADR 0029). A unit is filled by one generation: a leader and,
// on a multi-node unit, its followers, launched together. Units do not share
// a fate: a failed generation is rebuilt on its own, with its unit's own
// retry budget, and never tears down another unit.
func (c *Controller) reconcileUnits(ctx context.Context, dep *store.Deployment, insts []store.Instance, pd *planning.PDPlacement) error {
	deployment, ok, err := c.prepareUnits(dep, pd)
	if err != nil || !ok {
		return err
	}
	deployment.insts = insts
	if waiting, err := c.settleRetiring(ctx, dep, insts); err != nil || waiting {
		return err
	}

	insts, err = c.failoverPoolReplicas(deployment, insts)
	if err != nil {
		return err
	}
	units, insts, err := c.nodeDrain(dep, deployment.units, insts)
	if err != nil {
		return err
	}
	if units, err = c.drainNamedUnits(ctx, deployment, units); err != nil {
		return err
	}
	deployment.insts = insts
	match := planning.MatchUnits(units, insts, deployment.nodeUUIDs, dep.PlacementStrategy)
	if !match.Unfilled() {
		// Nothing left to place: the deployment waits for no one's cards.
		c.waits.Done(dep.ID)
	}
	if moved, err := c.failoverNamedUnits(ctx, deployment, match); moved || err != nil {
		return err
	}
	if moved, err := c.completeDrainSurge(deployment, match); moved || err != nil {
		return err
	}
	if err := c.retire(dep, insts, match.Orphans, "unit generation has no leader any more"); err != nil {
		return err
	}
	if err := c.retire(dep, insts, match.Stale, "its unit left the placement"); err != nil {
		return err
	}
	// An instance no replica claims any more drains: marking it draining
	// takes it out of the gateway at once, and it is retired once it holds
	// no requests.
	draining := append([]store.Instance(nil), match.Draining...)
	for _, inst := range match.Excess {
		// Only while it still fills a replica: one an operator stopped or
		// removed meanwhile is not put back to draining.
		err := c.store.TransitionInstances(dep.ID, []string{inst.UUID}, store.ReplicaFillingStates(), proto.InstDraining, "its replica left the placement")
		if errors.Is(err, store.ErrStateChanged) {
			continue
		}
		if err != nil {
			return err
		}
		inst.State = proto.InstDraining
		draining = append(draining, inst)
	}
	if drained := c.drainedUnits(dep.ID, draining); len(drained) > 0 {
		if err := c.retire(dep, insts, drained, "drained"); err != nil {
			return err
		}
	}
	if dep.RestartRequestedAt != nil {
		if err := c.rollRestart(dep, insts, match); err != nil {
			return err
		}
	}

	model, err := c.store.GetModel(dep.ModelID)
	if err != nil {
		return fmt.Errorf("deployment %d: model %d missing", dep.ID, dep.ModelID)
	}
	var preparing []uint
	placementResolved := true
	defer func() { c.placements.set(dep, preparing) }()
	cleaned := map[string]bool{}
	for _, slot := range match.Slots {
		if slot.Inst != nil {
			if slot.Unit.MultiNode() {
				if err := c.checkGeneration(dep, deployment, insts, slot); err != nil {
					return err
				}
			}
			continue
		}
		attempts := match.Failed[slot.Key]
		if len(planning.ChargedAttempts(attempts)) >= c.maxAttempts {
			if done, err := c.finalizeFailedAttempts(ctx, dep, attempts); err != nil || !done {
				return err
			}
			c.recordUnitExhausted(dep, slot.Unit, attempts)
			continue
		}
		if wait := c.retryWait(attempts); wait > 0 {
			placementResolved = false
			c.q.addAfter(depKey(dep.ID), wait)
			continue
		}
		if len(attempts) > 0 && !cleaned[slot.Key] {
			// A retry gets fresh UUIDs; remove the failed attempts' containers
			// first so they do not leak. The rows stay: they are the unit's
			// spent attempts, and the last one is the evidence once the
			// budget is exhausted. This comes before a node is picked: a
			// failed attempt whose engine still holds its card (an uncertain
			// sleep transition) makes that card read as taken, and the retry
			// would wait for a node forever. An attempt on an offline node
			// does not hold the retry up: its agent drops the container with
			// the full desired state it gets on reconnect.
			var old []string
			for _, inst := range attempts {
				old = append(old, inst.UUID)
			}
			if err := c.pushRemoveOnline(insts, old); err != nil {
				c.q.addAfter(depKey(dep.ID), c.retryBackoff)
				return fmt.Errorf("remove failed attempts before retry: %w", err)
			}
			if done, err := c.finalizeFailedAttempts(ctx, dep, attempts); err != nil || !done {
				return err
			}
			cleaned[slot.Key] = true
		}
		if moved, err := c.autoPlaceUnit(ctx, deployment, model, slot); moved || err != nil {
			return err
		}
		nodes, ready := c.unitNodes(ctx, dep, deployment, model, slot.Unit)
		if !ready {
			placementResolved = false
			continue
		}
		if leaving := masterPortHolder(dep, insts, nodes[0]); leaving != nil {
			placementResolved = false
			// The generation it replaces still listens on the master port;
			// its retirement requeues the deployment.
			c.log.Info("replica waits for the generation it replaces to leave its leader node", "dep", dep.ID, "unit", slot.Key, "leaving", leaving.UUID)
			continue
		}
		for _, node := range nodes {
			preparing = append(preparing, node.ID)
		}
		c.placements.set(dep, preparing)
		ready, err := c.generationReady(ctx, dep, model, nodes)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		launched, err := c.launchGeneration(ctx, deployment, model, slot, nodes, len(attempts)+1)
		if err != nil {
			return err
		}
		if !launched {
			return nil
		}
		if len(attempts) > 0 {
			c.recordReplicaRebuilt(dep, slot.Unit, attempts, nodes[0])
		}
	}
	if placementResolved {
		return c.retainCacheCopyNodes(ctx, dep.ID, preparing)
	}
	return nil
}

// prepareUnits reads the deployment's units and the nodes they name. ok is
// false when the deployment cannot be reconciled now; the reason is recorded.
func (c *Controller) prepareUnits(dep *store.Deployment, pd *planning.PDPlacement) (*unitDeployment, bool, error) {
	out := &unitDeployment{dep: dep, nodes: map[string]*store.Node{}, nodeUUIDs: map[uint]string{}}
	// An unreadable requirement set fails the pass rather than reading as
	// "no requirements": that would launch onto nodes the deployment excludes.
	reqs, err := c.store.DeploymentRequirements(dep.ID)
	if err != nil {
		return nil, false, fmt.Errorf("deployment %d node requirements: %w", dep.ID, err)
	}
	out.requirements = reqs
	var ordered []*store.Node
	if pd != nil {
		mode, err := planning.ValidateRunningPD(pd, dep.Engine)
		if err != nil {
			c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": err.Error()}})
			return nil, false, nil //nolint:nilerr // Invalid placement is recorded as an unschedulable result.
		}
		for _, uuid := range pd.NodeUUIDs(mode) {
			node, err := c.store.NodeWithGPUs(uuid)
			if err != nil {
				c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": fmt.Sprintf("node %s: %v", uuid, err)}})
				return nil, false, nil
			}
			ordered = append(ordered, node)
		}
		out.pd = &pdDeployment{mode: mode, placement: pd}
	} else {
		nodes, err := c.deploymentNodes(dep)
		if err != nil {
			return nil, false, err
		}
		ordered = nodes
		// A deployment that lists its replicas runs them on any number of
		// nodes; one that does not is one replica over exactly its nodes.
		listed, _ := planning.ParsePlacement(dep.Placement)
		if dep.NNodes > 1 && len(listed.Units) == 0 && len(ordered) != dep.NNodes {
			c.log.Warn("multi-node deployment waiting: target node set incomplete", "dep", dep.ID, "want", dep.NNodes, "got", len(ordered))
			return nil, false, nil
		}
	}
	uuids := make([]string, 0, len(ordered))
	for _, node := range ordered {
		out.nodes[node.UUID] = node
		out.nodeUUIDs[node.ID] = node.UUID
		out.members = append(out.members, *node)
		uuids = append(uuids, node.UUID)
	}
	if out.pd != nil {
		if _, err := planning.ValidatePDNodes(pd, out.pd.mode, out.nodes, false); err != nil {
			c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": err.Error()}})
			return nil, false, nil //nolint:nilerr // Invalid PD nodes are recorded as an unschedulable result.
		}
		resolved, groupErr := c.networks.ComputeGroup(out.members)
		group, err := planning.PDGroup(out.members, resolved, groupErr)
		if err == nil {
			err = planning.CheckPDNetwork(out.pd.mode, out.members, group, dep.Sleepable)
		}
		if err != nil {
			c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": "PD compute network: " + err.Error()}})
			return nil, false, nil //nolint:nilerr // The PD network error is recorded as an unschedulable result.
		}
		out.pd.group = group
	}
	out.units = planning.DeploymentUnits(dep, pd, uuids)
	if pd == nil && dep.NNodes > 1 {
		for _, unit := range out.units {
			if len(unit.Ranks) != dep.NNodes {
				c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": fmt.Sprintf("replica %d has %d ranks, the deployment runs %d", unit.Index, len(unit.Ranks), dep.NNodes)}})
				return nil, false, nil
			}
		}
	}
	return out, true, nil
}

// settleRetiring finishes what earlier passes retired: a row the agent
// confirmed stopped is deleted, and a row still being removed is pushed
// again and waited for, so a unit is never rebuilt while its previous
// generation still holds GPUs. A row on an offline node keeps waiting for
// its node without holding the deployment up: the replica it leaves has
// already been moved (or will be), and when the node reconnects the removal
// is pushed and confirmed like any other - an agent restarted meanwhile no
// longer knows the container, and orphan cleanup is off by default. It
// reports whether rows on online nodes are still being removed.
func (c *Controller) settleRetiring(ctx context.Context, dep *store.Deployment, insts []store.Instance) (bool, error) {
	var retiring []string
	for _, inst := range insts {
		switch inst.State {
		case proto.InstStopped:
			if _, err := c.finalizeInstanceRemoval(ctx, inst, true); err != nil {
				c.q.addAfter(depKey(dep.ID), c.retryBackoff)
				return false, err
			}
		case proto.InstRemoving, proto.InstPendingRemoval:
			node, err := c.store.NodeByID(inst.NodeID)
			if err != nil {
				// The node is gone from the cluster: nothing is left to confirm.
				if err := c.store.DeleteInstance(inst.UUID); err != nil {
					return false, err
				}
				continue
			}
			if !c.hub.Online(node.UUID) {
				continue
			}
			retiring = append(retiring, inst.UUID)
		}
	}
	if len(retiring) == 0 {
		return false, nil
	}
	c.q.addAfter(depKey(dep.ID), c.retryBackoff)
	if err := c.pushRemoveOnline(insts, retiring); err != nil {
		return true, err
	}
	return true, nil
}

// retire marks instances removing and asks their nodes to stop them;
// settleRetiring confirms them, keeping a row on an offline node until it
// reconnects. A failed push leaves the rows removing, and the next pass
// pushes again.
func (c *Controller) retire(dep *store.Deployment, insts []store.Instance, remove []store.Instance, message string) error {
	if len(remove) == 0 {
		return nil
	}
	uuids := make([]string, 0, len(remove))
	for _, inst := range remove {
		if err := c.store.SetInstanceState(inst.UUID, proto.InstRemoving, message); err != nil {
			return err
		}
		uuids = append(uuids, inst.UUID)
	}
	c.q.addAfter(depKey(dep.ID), c.retryBackoff)
	return c.pushRemoveOnline(insts, uuids)
}

// checkGeneration keeps a multi-node unit's generation whole. A collective
// cannot replace one process in a live worker group, so a rank that failed,
// stopped (its row is gone) or whose node stayed offline beyond the reconnect
// grace fails the whole generation.
func (c *Controller) checkGeneration(dep *store.Deployment, deployment *unitDeployment, insts []store.Instance, slot planning.UnitSlot) error {
	ranks := append([]*store.Instance{slot.Inst}, slot.Followers...)
	for index, inst := range ranks {
		node := deployment.nodes[slot.Unit.Ranks[index].Nodes[0]]
		reason := ""
		switch {
		case inst == nil:
			reason = "the rank is gone"
		case inst.State == proto.InstFailed:
			reason = inst.Message
		case node != nil && !c.hub.Online(node.UUID):
			if remaining := c.offlineGraceRemaining(node, c.now()); remaining > 0 {
				c.q.addAfter(depKey(dep.ID), remaining)
				continue
			}
			reason = "node offline beyond reconnect grace"
		default:
			continue
		}
		nodeName := ""
		if node != nil {
			nodeName = node.UUID
		}
		return c.failGeneration(dep, insts, slot, fmt.Sprintf("rank %d on %s failed: %s", index, nodeName, reason))
	}
	return nil
}

// failGeneration ends a unit's generation: the leader becomes the failed
// attempt the unit's retry budget counts, carrying the rank's reason, its
// container is removed, and every follower goes.
func (c *Controller) failGeneration(dep *store.Deployment, insts []store.Instance, slot planning.UnitSlot, reason string) error {
	// The leader's container goes first: a failed push leaves the generation
	// as it is, and the next pass finds the same failure and tries again.
	if err := c.pushRemoveOnline(insts, []string{slot.Inst.UUID}); err != nil {
		c.q.addAfter(depKey(dep.ID), c.retryBackoff)
		return err
	}
	c.log.Warn("unit generation failed", "dep", dep.ID, "role", slot.Unit.Role, "unit", slot.Unit.Index, "leader", slot.Inst.UUID, "reason", reason)
	if err := c.store.SetInstanceState(slot.Inst.UUID, proto.InstFailed, reason); err != nil {
		return err
	}
	var followers []store.Instance
	for _, follower := range slot.Followers {
		if follower != nil {
			followers = append(followers, *follower)
		}
	}
	if err := c.retire(dep, insts, followers, "unit generation failed: "+reason); err != nil {
		return err
	}
	c.q.add(depKey(dep.ID))
	return nil
}

// recordUnitExhausted says once, on the unit's newest failed attempt, that
// the unit spent its retry budget and is no longer rebuilt: the timeline
// shows it, and the deployment response reads it (the unit waits for an
// operator's retry, ADR 0029).
func (c *Controller) recordUnitExhausted(dep *store.Deployment, unit planning.Unit, attempts []store.Instance) {
	newest := planning.NewestAttempt(attempts)
	recorded, err := c.store.InstancesWithEvent(store.EventUnitRetryExhausted, []string{newest.UUID})
	if err != nil || recorded[newest.UUID] {
		return
	}
	c.log.Warn("unit gave up after repeated failures", "dep", dep.ID, "role", unit.Role, "unit", unit.Index, "failed", len(attempts))
	c.store.AddInstanceEventCode(&newest, store.EventKindSchedule, store.EventUnitRetryExhausted, map[string]any{"attempts": len(attempts), "reason": newest.Message})
}

// rebuildReasonRunes bounds the failed attempt's message a REPLICA_REBUILT
// event carries: the full message stays on the failed attempt's own timeline.
const rebuildReasonRunes = 200

// recordReplicaRebuilt says on the deployment's timeline that a replica was
// started again after its previous attempt failed, and why it failed. The
// entry is written on the failed attempt, so it names the instance whose
// output says the rest.
func (c *Controller) recordReplicaRebuilt(dep *store.Deployment, unit planning.Unit, attempts []store.Instance, leader *store.Node) {
	failed := planning.NewestAttempt(attempts)
	reason := []rune(strings.TrimSpace(failed.Message))
	if len(reason) > rebuildReasonRunes {
		reason = append(reason[:rebuildReasonRunes], '…')
	}
	params := map[string]any{"replica": unit.Index + 1, "attempt": len(attempts) + 1, "reason": string(reason), "node": leader.DisplayName(),
		store.EventParamNodeUUIDs: []string{leader.UUID}}
	if unit.Role != "" {
		params["role"] = unit.Role
	}
	if failed.UUID == "" {
		c.store.AddDeploymentEventCode(dep.ID, store.EventKindLifecycle, store.EventReplicaRebuilt, params)
		return
	}
	failed.DeploymentID = dep.ID
	c.store.AddInstanceEventCode(&failed, store.EventKindLifecycle, store.EventReplicaRebuilt, params)
}

// retryWait is how long a unit waits before its next attempt: the retry
// backoff since its newest failed attempt, or nothing after a launch refused
// before it started anything (planning.RetryAtOnce).
func (c *Controller) retryWait(attempts []store.Instance) time.Duration {
	if planning.RetryAtOnce(attempts) {
		return 0
	}
	var newest time.Time
	for _, inst := range attempts {
		if inst.CreatedAt.After(newest) {
			newest = inst.CreatedAt
		}
	}
	if newest.IsZero() {
		return 0
	}
	if since := c.now().Sub(newest); since < c.retryBackoff {
		return c.retryBackoff - since
	}
	return 0
}

// unitNodes picks the node of every rank of a unit to launch: a pool
// replica's from its candidate pool (pickPoolNode), any other rank's own
// node, or the first of its candidates that may run it. A node that no longer
// satisfies the deployment's node requirements is rejected and recorded, and
// the next candidate is tried; a unit waits while a node it needs is offline.
func (c *Controller) unitNodes(ctx context.Context, dep *store.Deployment, deployment *unitDeployment, model *store.Model, unit planning.Unit) ([]*store.Node, bool) {
	if unit.Pool() {
		node, ok := c.pickPoolNode(ctx, deployment, model)
		if !ok {
			return nil, false
		}
		return []*store.Node{node}, true
	}
	out := make([]*store.Node, 0, len(unit.Ranks))
	for _, rank := range unit.Ranks {
		var picked *store.Node
		for _, uuid := range rank.Nodes {
			node := deployment.nodes[uuid]
			if node == nil {
				continue
			}
			if err := c.matchDeploymentRequirements(deployment, node); err != nil {
				c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonPlacementRejected, Params: map[string]any{"detail": err.Error()}})
				continue
			}
			if !node.Scheduling.TakesReplicas() {
				c.recordUnschedulable(dep, scheduler.Reason{Code: scheduler.ReasonNodeCordoned, Params: map[string]any{"node": node.DisplayName()}})
				return nil, false
			}
			if !c.hub.Online(node.UUID) {
				c.log.Debug("deployment waiting: node offline", "dep", dep.ID, "node", node.UUID)
				return nil, false
			}
			picked = node
			break
		}
		if picked == nil {
			return nil, false
		}
		out = append(out, picked)
	}
	return out, true
}

// generationReady is the start barrier of a generation: it starts only once
// the model and image are on every rank's node, so all ranks start together.
// A rank still pulling when its peers are already up misses the collective's
// rendezvous, which gives up after ten minutes (seen on two 4090 VMs whose
// image pulls finished ten minutes apart). It starts what is missing and
// reports whether everything is there; a multi-node generation also has its
// image pulled onto every node ahead of the launch.
func (c *Controller) generationReady(ctx context.Context, dep *store.Deployment, model *store.Model, nodes []*store.Node) (bool, error) {
	ready := true
	for _, node := range nodes {
		_, present, err := c.ensureDeploymentModelsPresent(ctx, node, dep, model)
		if err != nil {
			return false, err
		}
		delivery := c.deploymentImageDelivery(ctx, dep, node)
		ready = ready && present && delivery.present
		if len(nodes) > 1 && delivery.present {
			ready = ready && c.imagePrePulled(ctx, dep, node)
		}
	}
	if !ready && len(nodes) > 1 {
		c.q.addAfter(depKey(dep.ID), groupPollInterval)
	}
	return ready, nil
}

// masterPortHolder is the leaving leader of an ordinary multi-node deployment
// that may still listen on the deployment's master port on node: every
// generation of such a deployment rendezvous on that one port at its leader,
// so a replacement led from the same node waits for it. A PD deployment's
// generations each get ports of their own and never wait.
func masterPortHolder(dep *store.Deployment, insts []store.Instance, node *store.Node) *store.Instance {
	if dep.NNodes <= 1 || node == nil {
		return nil
	}
	for i := range insts {
		in := &insts[i]
		if in.Headless || in.NNodes <= 1 || in.NodeID != node.ID {
			continue
		}
		switch in.State {
		case proto.InstDraining, proto.InstRemoving, proto.InstPendingRemoval:
			return in
		}
	}
	return nil
}
