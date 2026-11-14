package controller

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Timeline events of node replacement, on the deployment.
const (
	EventNodeReplaced            = "NODE_REPLACED"
	EventNodeReplaceReverted     = "NODE_REPLACE_REVERTED"
	EventNodeReplaceNoCandidate  = "NODE_REPLACE_NO_CANDIDATE"
	nodeReplaceEventKind         = store.EventKindAutomation
	nodeReplacedMessage          = "its node was offline beyond the replacement wait; replaced on another node"
	nodeReplaceCandidateTrials   = 3
	nodeReplaceNoCandidateRepeat = 10 * time.Minute
)

// failoverMove is one rank a lost node made the controller move, kept until
// the unit runs on its new node: if the lost node returns first, the move is
// undone. It lives in memory; a server that has just started reads it back
// from the NODE_REPLACED events (node_failover_restore.go).
type failoverMove struct {
	Role      string
	Unit      int
	Rank      int
	From, To  string // node UUIDs
	MovedAt   time.Time
	unitAfter planning.PDUnit
}

// nodeFailover is the controller's memory of node replacement.
type nodeFailover struct {
	mu        sync.Mutex
	moves     map[uint][]failoverMove // by deployment
	noneSince map[uint]time.Time      // last NODE_REPLACE_NO_CANDIDATE, by deployment
	drains    map[uint][]drainSurge   // units moving off draining nodes (node_failover_drain.go)
	repicked  map[uint]time.Time      // last automatic re-pick that found no nodes, by deployment (unit_auto.go)
	restored  map[uint]bool           // deployments whose moves were read back from the events (node_failover_restore.go)
}

func (f *nodeFailover) get(dep uint) []failoverMove {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]failoverMove(nil), f.moves[dep]...)
}

func (f *nodeFailover) set(dep uint, moves []failoverMove) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.moves == nil {
		f.moves = map[uint][]failoverMove{}
	}
	if len(moves) == 0 {
		delete(f.moves, dep)
		return
	}
	f.moves[dep] = moves
}

// replaceAfter is the deployment's node replacement wait; 0 never replaces.
func replaceAfter(dep *store.Deployment) time.Duration {
	return time.Duration(dep.NodeReplaceAfterSec) * time.Second
}

// failoverPoolReplicas retires the single-node replicas whose node is lost:
// the rows of an offline node are dropped (its agent removes the containers
// when it reconnects) and the candidate pool starts the replicas elsewhere.
// The replicas did not fail, so no retry is spent. A node offline but not yet
// lost brings the deployment back when it would be. A deployment no other
// online node may run (one pinned to its node) keeps its replicas: there is
// nowhere to move them, and they come back with their node.
func (c *Controller) failoverPoolReplicas(deployment *unitDeployment, insts []store.Instance) ([]store.Instance, error) {
	dep := deployment.dep
	after := replaceAfter(dep)
	if after <= 0 || len(deployment.units) == 0 || !deployment.units[0].Pool() {
		return insts, nil
	}
	now := c.now()
	nodes := map[uint]*store.Node{}
	var lost []store.Instance
	for _, inst := range insts {
		if inst.Headless || !planning.InstanceActive(inst.State) || inst.State == proto.InstDraining {
			continue
		}
		node, ok := nodes[inst.NodeID]
		if !ok {
			n, err := c.store.NodeByID(inst.NodeID)
			if err != nil {
				continue
			}
			node, nodes[inst.NodeID] = n, n
		}
		if c.hub.Online(node.UUID) {
			continue
		}
		if seen := c.sighted(node); !planning.NodeLost(seen, false, after, now) {
			if at := planning.LostSince(seen, after); !at.IsZero() {
				c.q.addAfter(depKey(dep.ID), at.Sub(now))
			}
			continue
		}
		lost = append(lost, inst)
	}
	if len(lost) == 0 || !c.anotherNodeMayRun(deployment, lost) {
		return insts, nil
	}
	for _, inst := range lost {
		node := nodes[inst.NodeID]
		c.log.Warn("replica's node lost; replacing it on another node", "dep", dep.ID, "instance", inst.UUID, "node", node.UUID)
		c.store.AddDeploymentEventCode(dep.ID, nodeReplaceEventKind, EventNodeReplaced, map[string]any{
			"from": node.DisplayName(), "instances": []string{inst.UUID}, "after_sec": dep.NodeReplaceAfterSec,
			store.EventParamNodeUUIDs: []string{node.UUID},
		})
	}
	if err := c.retire(dep, insts, lost, nodeReplacedMessage); err != nil {
		return insts, err
	}
	out := append([]store.Instance(nil), insts...)
	gone := map[string]bool{}
	for _, inst := range lost {
		gone[inst.UUID] = true
	}
	for i := range out {
		if gone[out[i].UUID] {
			out[i].State = proto.InstRemoving
		}
	}
	return out, nil
}

// anotherNodeMayRun reports whether an online, schedulable node other than
// the lost replicas' own satisfies the deployment's node requirements.
func (c *Controller) anotherNodeMayRun(deployment *unitDeployment, lost []store.Instance) bool {
	gone := map[uint]bool{}
	for _, inst := range lost {
		gone[inst.NodeID] = true
	}
	nodes, err := c.store.ListNodes()
	if err != nil {
		return false
	}
	for i := range nodes {
		node := &nodes[i]
		if gone[node.ID] || !c.hub.Online(node.UUID) || !node.Scheduling.TakesReplicas() {
			continue
		}
		if c.matchDeploymentRequirements(deployment, node) == nil {
			return true
		}
	}
	return false
}

// failoverNamedUnits moves the ranks of a multi-node or PD deployment off
// lost nodes, and undoes a move whose lost node came back before the unit
// ran on its new one. It reports whether it rewrote the placement: the pass
// then ends, and the next one reconciles the new placement.
func (c *Controller) failoverNamedUnits(ctx context.Context, deployment *unitDeployment, match planning.UnitMatch) (bool, error) {
	dep := deployment.dep
	if len(deployment.units) == 0 || deployment.units[0].Pool() {
		return false, nil
	}
	if reverted, err := c.revertFailovers(dep, deployment, match); reverted || err != nil {
		return reverted, err
	}
	after := replaceAfter(dep)
	if after <= 0 {
		return false, nil
	}
	now := c.now()
	lost := map[string]bool{}
	for uuid, node := range deployment.nodes {
		if c.hub.Online(uuid) {
			continue
		}
		seen := c.sighted(node)
		if planning.NodeLost(seen, false, after, now) {
			lost[uuid] = true
		} else if at := planning.LostSince(seen, after); !at.IsZero() {
			c.q.addAfter(depKey(dep.ID), at.Sub(now))
		}
	}
	if len(lost) == 0 {
		return false, nil
	}
	plan, err := c.planFailover(ctx, deployment, lost)
	if err != nil || plan == nil {
		return false, err
	}
	if err := c.writeUnits(dep, deployment, plan.units); err != nil {
		c.log.Warn("node replacement could not be written", "dep", dep.ID, "err", err)
		return false, nil
	}
	moves := c.failover.get(dep.ID)
	for _, move := range plan.moves {
		move.MovedAt = now
		moves = append(moves, move)
		params := map[string]any{"from": c.nodeName(deployment, move.From), "to": c.nodeName(deployment, move.To), "after_sec": dep.NodeReplaceAfterSec, "unit": move.Unit + 1, "rank": move.Rank,
			store.EventParamNodeUUIDs: nodeUUIDList(move.From, move.To)}
		if move.Role != "" {
			params["role"] = move.Role
		}
		c.store.AddDeploymentEventCode(dep.ID, nodeReplaceEventKind, EventNodeReplaced, params)
		c.log.Warn("rank's node lost; unit moved to another node", "dep", dep.ID, "role", move.Role, "unit", move.Unit, "rank", move.Rank, "from", move.From, "to", move.To)
	}
	c.failover.set(dep.ID, moves)
	c.q.add(depKey(dep.ID))
	return true, nil
}

// revertFailovers undoes a move whose lost node is back online while the
// moved unit has not run on its new node yet, and forgets a move once the
// unit runs there. It reports whether it rewrote the placement.
func (c *Controller) revertFailovers(dep *store.Deployment, deployment *unitDeployment, match planning.UnitMatch) (bool, error) {
	units := map[string][]planning.PDUnit{}
	if deployment.pd != nil {
		for _, role := range deployment.pd.mode.Roles {
			units[role] = append([]planning.PDUnit(nil), deployment.pd.placement.Units(role)...)
		}
	} else {
		units[""] = planning.ReplicaUnitsOf(dep, deploymentNodeOrder(deployment))
	}
	c.restoreFailovers(dep, deployment, units)
	moves := c.failover.get(dep.ID)
	if len(moves) == 0 {
		return false, nil
	}
	var keep, reverted []failoverMove
	for _, move := range moves {
		list := units[move.Role]
		if move.Unit >= len(list) || !sameUnit(list[move.Unit], move.unitAfter) {
			continue // the unit was edited since: the move is history
		}
		if unitRunning(match, move.Role, move.Unit) {
			continue // it runs on its new node: no going back
		}
		if !c.hub.Online(move.From) {
			keep = append(keep, move)
			continue
		}
		list[move.Unit] = planning.WithRankOn(list[move.Unit], move.Rank, move.From)
		reverted = append(reverted, move)
	}
	if len(reverted) == 0 {
		c.failover.set(dep.ID, keep)
		return false, nil
	}
	if err := c.writeUnits(dep, deployment, units); err != nil {
		c.log.Warn("node replacement could not be undone", "dep", dep.ID, "err", err)
		c.failover.set(dep.ID, append(keep, reverted...))
		return false, nil
	}
	for _, move := range reverted {
		params := map[string]any{"from": c.nodeName(deployment, move.To), "to": c.nodeName(deployment, move.From), "unit": move.Unit + 1, "rank": move.Rank,
			store.EventParamNodeUUIDs: nodeUUIDList(move.To, move.From)}
		if move.Role != "" {
			params["role"] = move.Role
		}
		c.store.AddDeploymentEventCode(dep.ID, nodeReplaceEventKind, EventNodeReplaceReverted, params)
	}
	c.failover.set(dep.ID, keep)
	c.q.add(depKey(dep.ID))
	return true, nil
}

// unitRunning reports whether the unit at index of role has a running leader.
func unitRunning(match planning.UnitMatch, role string, index int) bool {
	for _, slot := range match.Slots {
		if slot.Unit.Role == role && slot.Unit.Index == index {
			return slot.Inst != nil && slot.Inst.State == proto.InstRunning
		}
	}
	return false
}

func sameUnit(a, b planning.PDUnit) bool {
	ra, rb := a.Ranks(), b.Ranks()
	if len(ra) != len(rb) {
		return false
	}
	for i := range ra {
		if ra[i].NodeUUID != rb[i].NodeUUID {
			return false
		}
	}
	return true
}

// recordNoCandidate says, at most every ten minutes, that a lost node's ranks
// found no node to move to.
func (c *Controller) recordNoCandidate(dep *store.Deployment, lost map[string]bool, deployment *unitDeployment, facts planning.UnitSuggestFacts) {
	c.failover.mu.Lock()
	if c.failover.noneSince == nil {
		c.failover.noneSince = map[uint]time.Time{}
	}
	last := c.failover.noneSince[dep.ID]
	now := c.now()
	if !last.IsZero() && now.Sub(last) < nodeReplaceNoCandidateRepeat {
		c.failover.mu.Unlock()
		c.q.addAfter(depKey(dep.ID), time.Minute)
		return
	}
	c.failover.noneSince[dep.ID] = now
	c.failover.mu.Unlock()
	uuids := make([]string, 0, len(lost))
	for uuid := range lost {
		uuids = append(uuids, uuid)
	}
	sort.Strings(uuids)
	names := make([]string, 0, len(uuids))
	for _, uuid := range uuids {
		names = append(names, c.nodeName(deployment, uuid))
	}
	params := map[string]any{"nodes": names, store.EventParamNodeUUIDs: uuids}
	// Nodes left out only because their kernel hangs UCX are the ones the
	// operator can bring back: name them (planning.UCXNetlinkBlocks). Read
	// here, after the throttle, so a deployment that keeps finding none does
	// not walk the nodes again on every pass.
	if _, counts := planning.SuggestUnitGroups(facts); counts.UCXNetlink > 0 {
		params["ucx_netlink_hang"] = counts.UCXNetlink
		params["ucx_netlink_nodes"] = counts.UCXNetlinkNodes
	}
	c.store.AddDeploymentEventCode(dep.ID, nodeReplaceEventKind, EventNodeReplaceNoCandidate, params)
	c.q.addAfter(depKey(dep.ID), time.Minute)
}

// deploymentNodeOrder is the deployment's nodes in stored order, by UUID.
func deploymentNodeOrder(deployment *unitDeployment) []string {
	out := make([]string, 0, len(deployment.members))
	for _, node := range deployment.members {
		out = append(out, node.UUID)
	}
	return out
}

func (c *Controller) nodeName(deployment *unitDeployment, uuid string) string {
	if node := deployment.nodes[uuid]; node != nil {
		return node.DisplayName()
	}
	// A replacement node is usually not one the deployment already ran on.
	if node, err := c.store.NodeByUUID(uuid); err == nil {
		return node.DisplayName()
	}
	return uuid
}

// nodeUUIDList is the node UUIDs a scheduling event concerns
// (store.EventParamNodeUUIDs), in the order given, each once and none empty.
func nodeUUIDList(uuids ...string) []string {
	out := make([]string, 0, len(uuids))
	for _, uuid := range uuids {
		if uuid != "" && !slices.Contains(out, uuid) {
			out = append(out, uuid)
		}
	}
	return out
}

// rankUUIDs is the node UUID of each of a unit's ranks.
func rankUUIDs(unit planning.PDUnit) []string {
	out := make([]string, 0, 1+len(unit.Followers))
	for _, rank := range unit.Ranks() {
		out = append(out, rank.NodeUUID)
	}
	return out
}
