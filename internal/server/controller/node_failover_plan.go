package controller

import (
	"context"
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// failoverPlan is the units a deployment moves to and the moves that get it
// there.
type failoverPlan struct {
	units map[string][]planning.PDUnit // by role ("" for an ordinary multi-node deployment)
	moves []failoverMove
}

// planFailover chooses a node for every rank on a lost node. A rank that finds
// none leaves its unit as it is (recorded once in a while); a multi-node unit
// whose lost ranks cannot all be replaced one by one is chosen whole again.
func (c *Controller) planFailover(ctx context.Context, deployment *unitDeployment, lost map[string]bool) (*failoverPlan, error) {
	dep := deployment.dep
	units := map[string][]planning.PDUnit{}
	roles := []string{""}
	if deployment.pd != nil {
		roles = deployment.pd.mode.Roles
		for _, role := range roles {
			units[role] = append([]planning.PDUnit(nil), deployment.pd.placement.Units(role)...)
		}
	} else {
		units[""] = planning.ReplicaUnitsOf(dep, deploymentNodeOrder(deployment))
	}
	var listed []planning.ListedRank
	for _, role := range roles {
		for _, unit := range units[role] {
			gpus := c.rankGPUs(deployment, role, unit)
			for i, rank := range unit.Ranks() {
				listed = append(listed, planning.ListedRank{NodeUUID: rank.NodeUUID, GPUs: gpus, Leader: i == 0 && deployment.pd == nil})
			}
		}
	}
	model, err := c.store.GetModel(dep.ModelID)
	if err != nil {
		return nil, err
	}
	facts, err := c.unitFacts(deployment, listed, 1)
	if err != nil {
		return nil, err
	}
	facts.Strategy = dep.PlacementStrategy
	nodesByUUID := map[string]*store.Node{}
	for i := range facts.Nodes {
		nodesByUUID[facts.Nodes[i].UUID] = &facts.Nodes[i]
	}
	plan := &failoverPlan{units: units}
	noCandidate := false
	for _, role := range roles {
		for index, unit := range units[role] {
			ranks := unit.Ranks()
			var lostRanks []int
			for i, rank := range ranks {
				if lost[rank.NodeUUID] {
					lostRanks = append(lostRanks, i)
				}
			}
			if len(lostRanks) == 0 {
				continue
			}
			perRank := c.rankGPUs(deployment, role, unit)
			facts.PerRank = perRank
			moved, moves, ok := c.replaceRanks(ctx, deployment, model, facts, nodesByUUID, role, index, unit, lostRanks, lost)
			if !ok && deployment.pd == nil && len(ranks) > 1 {
				moved, moves, ok = c.replaceUnitWhole(ctx, deployment, model, facts, role, index, unit, lost, false)
			}
			if !ok {
				noCandidate = true
				continue
			}
			plan.units[role][index] = moved
			plan.moves = append(plan.moves, moves...)
		}
	}
	if noCandidate {
		c.recordNoCandidate(dep, lost, deployment, facts)
	}
	if len(plan.moves) == 0 {
		return nil, nil
	}
	return plan, nil
}

// replaceRanks moves each lost rank of a unit to a candidate on the network
// the unit's other members use and of the lost node's GPU architecture,
// trying the best few until one plans.
func (c *Controller) replaceRanks(ctx context.Context, deployment *unitDeployment, model *store.Model, facts planning.UnitSuggestFacts, nodes map[string]*store.Node, role string, index int, unit planning.PDUnit, lostRanks []int, lost map[string]bool) (planning.PDUnit, []failoverMove, bool) {
	moved := unit
	var moves []failoverMove
	used := map[string]bool{}
	for _, rank := range unit.Ranks() {
		used[rank.NodeUUID] = true
	}
	for _, rankIndex := range lostRanks {
		from := unit.Ranks()[rankIndex].NodeUUID
		lostNode := deployment.nodes[from]
		if lostNode == nil {
			return unit, nil, false
		}
		network, ok := c.failoverNetwork(deployment, unit, lost, facts)
		if !ok {
			return unit, nil, false
		}
		candidates := planning.ReplacementCandidates(facts, network, planning.NodeArch(lostNode), used, rankIndex == 0 && deployment.pd == nil)
		placed := false
		for _, candidate := range candidates[:min(len(candidates), nodeReplaceCandidateTrials)] {
			trial := planning.WithRankOn(moved, rankIndex, candidate.UUID)
			if !c.trialUnitPlan(ctx, deployment, model, role, unit, trial, nodes) {
				continue
			}
			moved = trial
			used[candidate.UUID] = true
			moves = append(moves, failoverMove{Role: role, Unit: index, Rank: rankIndex, From: from, To: candidate.UUID, unitAfter: trial})
			placed = true
			break
		}
		if !placed {
			return unit, nil, false
		}
	}
	for i := range moves {
		moves[i].unitAfter = moved
	}
	return moved, moves, true
}

// replaceUnitWhole chooses every rank of an ordinary multi-node replica
// again, the way the replica suggestion does, when its lost ranks could not
// be replaced one by one.
//
// keepLeader says the unit keeps running while its replacement starts (a
// drain): its leader still holds the master port, so the replacement may not
// lead from it.
func (c *Controller) replaceUnitWhole(ctx context.Context, deployment *unitDeployment, model *store.Model, facts planning.UnitSuggestFacts, role string, index int, unit planning.PDUnit, lost map[string]bool, keepLeader bool) (planning.PDUnit, []failoverMove, bool) {
	ranks := unit.Ranks()
	whole := facts
	whole.Ranks = len(ranks)
	// A failed replica's own leader no longer leads it once it moves.
	whole.Leaders = map[string]bool{}
	for uuid := range facts.Leaders {
		if keepLeader || uuid != unit.NodeUUID {
			whole.Leaders[uuid] = true
		}
	}
	nodes := map[string]*store.Node{}
	for i := range facts.Nodes {
		nodes[facts.Nodes[i].UUID] = &facts.Nodes[i]
	}
	groups, _ := planning.SuggestUnitGroups(whole)
	for _, group := range groups[:min(len(groups), nodeReplaceCandidateTrials)] {
		skip := map[string]bool{}
		for uuid := range lost {
			skip[uuid] = true
		}
		for trial := 0; trial < nodeReplaceCandidateTrials; trial++ {
			picked, ok := planning.UnitFromGroup(group, len(ranks), whole.Leaders, skip)
			if !ok {
				break
			}
			candidate := planning.PDUnit{NodeUUID: picked[0].UUID}
			for _, node := range picked[1:] {
				candidate.Followers = append(candidate.Followers, planning.PDRank{NodeUUID: node.UUID})
			}
			if !c.trialUnitPlan(ctx, deployment, model, role, unit, candidate, nodes) {
				skip[picked[len(picked)-1].UUID] = true
				continue
			}
			var moves []failoverMove
			for i, rank := range ranks {
				if rank.NodeUUID != candidate.Ranks()[i].NodeUUID {
					moves = append(moves, failoverMove{Role: role, Unit: index, Rank: i, From: rank.NodeUUID, To: candidate.Ranks()[i].NodeUUID, unitAfter: candidate})
				}
			}
			return candidate, moves, true
		}
	}
	return unit, nil, false
}

// failoverNetwork is the compute network a replacement must be on: the one
// the unit's surviving ranks use (for PD, the deployment's surviving
// members, which exchange KV over it), else the lost nodes' own.
func (c *Controller) failoverNetwork(deployment *unitDeployment, unit planning.PDUnit, lost map[string]bool, facts planning.UnitSuggestFacts) (uint, bool) {
	var members []string
	if deployment.pd != nil {
		for _, node := range deployment.members {
			members = append(members, node.UUID)
		}
	} else {
		for _, rank := range unit.Ranks() {
			members = append(members, rank.NodeUUID)
		}
	}
	var fallback *uint
	for _, uuid := range members {
		node := deployment.nodes[uuid]
		if node == nil {
			continue
		}
		resolved, ok := facts.Facts.Networks.For(node.ID, netplan.RoleCompute)
		if !ok {
			continue
		}
		id := resolved.Network.ID
		if !lost[uuid] {
			return id, true
		}
		if fallback == nil {
			fallback = &id
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return 0, false
}

// rankGPUs is how many GPUs each rank of a unit takes on its node.
func (c *Controller) rankGPUs(deployment *unitDeployment, role string, unit planning.PDUnit) int {
	if deployment.pd != nil {
		return deployment.pd.placement.RankGPUs(role, unit)
	}
	placement, err := planning.ParsePlacement(deployment.dep.Placement)
	if err != nil {
		return 1
	}
	perRank, err := placement.Parallelism(deployment.dep.NNodes).GPUsPerNode()
	if err != nil || perRank < 1 {
		return 1
	}
	return perRank
}

// unitTrial is the trial plan of the deployment's units (planning.UnitTrial).
func (c *Controller) unitTrial(deployment *unitDeployment, model *store.Model) planning.UnitTrial {
	trial := planning.UnitTrial{Store: c.store, Networks: c.networks, Deployment: deployment.dep, Requirements: deployment.requirements, Model: model}
	if deployment.pd != nil {
		trial.PD, trial.Mode = deployment.pd.placement, deployment.pd.mode
	}
	return trial
}

// trialUnitPlan plans the ranks a proposed unit moves (a rank the unit keeps
// on its node is not planned: its cards may still be its own) and reports
// whether every one would launch.
func (c *Controller) trialUnitPlan(ctx context.Context, deployment *unitDeployment, model *store.Model, role string, original, unit planning.PDUnit, nodes map[string]*store.Node) bool {
	refused, _, err := c.unitTrial(deployment, model).Unit(ctx, role, original, unit, nodes)
	return err == nil && refused == ""
}

// writeUnits writes a deployment's units as an operator's edit of its
// replica list (ordinary multi-node) or of its PD roles would, with the same
// checks.
func (c *Controller) writeUnits(dep *store.Deployment, deployment *unitDeployment, units map[string][]planning.PDUnit) error {
	nodes := map[string]*store.Node{}
	load := func(uuids []string) error {
		for _, uuid := range uuids {
			if nodes[uuid] != nil {
				continue
			}
			node, err := c.store.NodeWithGPUs(uuid)
			if err != nil {
				return err
			}
			nodes[uuid] = node
		}
		return nil
	}
	if deployment.pd == nil {
		list := units[""]
		if err := load(planning.ReplicaUnitNodes(list)); err != nil {
			return err
		}
		var named []store.Node
		for _, node := range nodes {
			named = append(named, *node)
		}
		facts := planning.LoadNodeFacts(c.store, c.networks.Resolve, named, deployment.requirements)
		patch, err := planning.PlanReplicaUnits(dep, deployment.requirements, facts, list, nodes)
		if err != nil {
			return err
		}
		return c.store.SetDeploymentReplicaUnits(dep.ID, patch)
	}
	roles := map[string]planning.PDRole{}
	for _, role := range deployment.pd.mode.Roles {
		roles[role] = planning.PDRole{Units: units[role]}
	}
	applied, err := planning.ApplyPDUpdate(deployment.pd.placement, dep.Engine, planning.PDUpdate{Roles: roles})
	if err != nil {
		return err
	}
	var nodeIDs []uint
	uuids := applied.Placement.NodeUUIDs(applied.Mode)
	if err := load(uuids); err != nil {
		return err
	}
	for _, uuid := range uuids {
		nodeIDs = append(nodeIDs, nodes[uuid].ID)
	}
	if _, err := planning.ValidatePDNodes(applied.Placement, applied.Mode, nodes, false); err != nil {
		return err
	}
	encoded, err := planning.WithPD(dep.Placement, applied.Placement)
	if err != nil {
		return err
	}
	ids, err := json.Marshal(nodeIDs)
	if err != nil {
		return err
	}
	primary := dep.NodeID
	if len(nodeIDs) > 0 {
		primary = nodeIDs[0]
	}
	return c.store.SetDeploymentPlacement(dep.ID, encoded, string(ids), primary)
}

// unitFacts reads what a choice of nodes for one rank of a deployment is
// decided from, with the hub's live facts - which nodes are connected, and
// which have a kernel that hangs UCX (planning.UCXNetlinkBlocks) - so a
// re-pick, a failover and a drain choose among the same nodes that creating
// the deployment did.
func (c *Controller) unitFacts(deployment *unitDeployment, listed []planning.ListedRank, perRank int) (planning.UnitSuggestFacts, error) {
	// A reconcile plans on the server's node snapshot: the status batch that
	// enqueued it refreshed the snapshot first (settleNodeStatus).
	return planning.LoadUnitSuggestFactsFromSnapshot(c.store, c.networks.Resolve, deployment.dep, deployment.requirements, listed, c.hub.Online, c.hub.UCXNetlinkHang, 1, perRank)
}
