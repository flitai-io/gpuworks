package planning

import (
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// MaxReplicas is how many replicas a deployment may run.
const MaxReplicas = 1024

// parseReplicaUnits reads the "units" list of a placement document: an
// ordinary multi-node deployment's replicas, each a leader node and its
// follower nodes in rank order, the same shape a PD role's units take.
func parseReplicaUnits(document map[string]any) ([]PDUnit, error) {
	raw, present := document["units"]
	if !present || raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("placement.units: %w", err)
	}
	var units []PDUnit
	if err := json.Unmarshal(encoded, &units); err != nil {
		return nil, fmt.Errorf("placement.units: %w", err)
	}
	return units, nil
}

// ReplicaUnitsOf is the multi-node replicas a deployment lists: its
// placement's units, or one replica over its stored node list (a deployment
// from before replicas were listed). nodes are the deployment's node UUIDs in
// stored order. A single-node deployment has none.
func ReplicaUnitsOf(dep *store.Deployment, nodes []string) []PDUnit {
	if dep.NNodes <= 1 {
		return nil
	}
	if placement, err := ParsePlacement(dep.Placement); err == nil && len(placement.Units) > 0 {
		return placement.Units
	}
	if len(nodes) == 0 {
		return nil
	}
	unit := PDUnit{NodeUUID: nodes[0]}
	for _, node := range nodes[1:] {
		unit.Followers = append(unit.Followers, PDRank{NodeUUID: node})
	}
	return []PDUnit{unit}
}

// WithReplicaUnits writes an ordinary multi-node deployment's replicas into
// its stored placement, keeping everything else the placement says.
func WithReplicaUnits(stored string, units []PDUnit) (string, error) {
	document := map[string]any{}
	if stored != "" && stored != "null" {
		if err := json.Unmarshal([]byte(stored), &document); err != nil {
			return "", fmt.Errorf("decode placement: %w", err)
		}
	}
	if document == nil {
		document = map[string]any{}
	}
	document["units"] = units
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// ReplicaUnitNodes are the nodes a list of multi-node replicas runs on, each
// once, in the order they first appear (the deployment's stored node list).
func ReplicaUnitNodes(units []PDUnit) []string {
	seen := map[string]bool{}
	var out []string
	for _, unit := range units {
		for _, rank := range unit.Ranks() {
			if rank.NodeUUID != "" && !seen[rank.NodeUUID] {
				seen[rank.NodeUUID] = true
				out = append(out, rank.NodeUUID)
			}
		}
	}
	return out
}

// replicaError is a product error about the replica at index (0-based),
// naming it the way the UI counts replicas (1-based).
func replicaError(code, fallback string, index int, params map[string]any) error {
	params["index"], params["replica"] = index, index+1
	return messages.NewProductError(code, fallback, params)
}

// PlanReplicaUnits checks the replicas a multi-node deployment is to list and
// returns every column the list decides. Each replica spans exactly the
// deployment's rank count on distinct known nodes, pins no GPUs (the
// scheduler picks them on each node), and has its own leader node (a leader
// listens on the deployment's master port); the ranks listed on a node,
// whichever replicas they belong to, fit its GPUs. A replica the list adds or
// moves must also be on online nodes the deployment's node requirements
// admit (MatchRequirements, over resolved); one the deployment already lists,
// on the same nodes in the same order, is kept as it is, so a list can still
// be changed while a kept replica's node is down or its labels moved. nodes
// holds the named nodes the store knows, by UUID, with their GPUs.
func PlanReplicaUnits(dep *store.Deployment, reqs nodelabel.Requirements, facts NodeFacts, units []PDUnit, nodes map[string]*store.Node) (store.ReplicaUnitsPatch, error) {
	if len(units) > MaxReplicas {
		return store.ReplicaUnitsPatch{}, messages.NewProductError(messages.ErrorReplicasTooMany, fmt.Sprintf("replicas must be at most %d", MaxReplicas), map[string]any{"maximum": MaxReplicas})
	}
	placement, err := ParsePlacement(dep.Placement)
	if err != nil {
		return store.ReplicaUnitsPatch{}, err
	}
	spec, err := DeploymentParallelism(dep, placement)
	if err != nil {
		return store.ReplicaUnitsPatch{}, err
	}
	perRank, err := spec.GPUsPerNode()
	if err != nil {
		return store.ReplicaUnitsPatch{}, err
	}
	kept := listedReplicas(dep, placement, nodes)
	leaders := map[string]int{}
	demand := map[string]int{}
	for index, unit := range units {
		ranks := unit.Ranks()
		unchanged := kept[replicaNodesKey(ranks)]
		if len(ranks) != dep.NNodes {
			return store.ReplicaUnitsPatch{}, replicaError(messages.ErrorReplicaUnitRanks, "a replica spans the deployment's rank count", index, map[string]any{"ranks": len(ranks), "want": dep.NNodes})
		}
		seen := map[string]bool{}
		for _, rank := range ranks {
			if len(rank.GPUIndexes) > 0 {
				return store.ReplicaUnitsPatch{}, replicaError(messages.ErrorReplicaUnitGPUs, "a multi-node replica does not pin GPUs", index, map[string]any{})
			}
			node := nodes[rank.NodeUUID]
			if node == nil || seen[rank.NodeUUID] {
				return store.ReplicaUnitsPatch{}, replicaError(messages.ErrorReplicaUnitNode, "a replica's ranks run on distinct known nodes", index, map[string]any{"node": rank.NodeUUID})
			}
			seen[rank.NodeUUID] = true
			demand[rank.NodeUUID] += perRank
			if unchanged {
				continue
			}
			if node.State != store.NodeStateOnline {
				return store.ReplicaUnitsPatch{}, replicaError(messages.ErrorReplicaUnitNodeOffline, fmt.Sprintf("node %s is not online", node.DisplayName()), index, map[string]any{"node": node.DisplayName()})
			}
			if err := MatchRequirements(node, facts, reqs); err != nil {
				return store.ReplicaUnitsPatch{}, replicaError(messages.ErrorReplicaUnitRequirements, err.Error(), index, map[string]any{"node": node.DisplayName(), "detail": err.Error()})
			}
		}
		if other, taken := leaders[unit.NodeUUID]; taken {
			return store.ReplicaUnitsPatch{}, replicaError(messages.ErrorReplicaLeaderShared, "two replicas are led from one node", index, map[string]any{"node": unit.NodeUUID, "other": other, "other_replica": other + 1})
		}
		leaders[unit.NodeUUID] = index
	}
	order := ReplicaUnitNodes(units)
	for _, uuid := range order {
		if node := nodes[uuid]; demand[uuid] > node.GPUCount {
			return store.ReplicaUnitsPatch{}, messages.NewProductError(messages.ErrorReplicaUnitGPUsInsufficient, fmt.Sprintf("node %s has %d GPUs, the ranks listed on it need %d", node.DisplayName(), node.GPUCount, demand[uuid]), map[string]any{"node": node.DisplayName(), "available": node.GPUCount, "required": demand[uuid]})
		}
	}
	encoded, err := WithReplicaUnits(dep.Placement, units)
	if err != nil {
		return store.ReplicaUnitsPatch{}, err
	}
	ids := make([]uint, 0, len(order))
	for _, uuid := range order {
		ids = append(ids, nodes[uuid].ID)
	}
	nodeIDs, err := json.Marshal(ids)
	if err != nil {
		return store.ReplicaUnitsPatch{}, err
	}
	return store.ReplicaUnitsPatch{Placement: encoded, NodeIDs: string(nodeIDs), NodeID: ids[0], Replicas: len(units)}, nil
}

// listedReplicas are the replicas the deployment lists now, each by the
// nodes of its ranks in order: its placement's units, or for a deployment
// that predates them its node list read as one replica.
func listedReplicas(dep *store.Deployment, placement Placement, nodes map[string]*store.Node) map[string]bool {
	out := map[string]bool{}
	for _, unit := range placement.Units {
		out[replicaNodesKey(unit.Ranks())] = true
	}
	if len(placement.Units) > 0 {
		return out
	}
	byID := make(map[uint]string, len(nodes))
	for uuid, node := range nodes {
		byID[node.ID] = uuid
	}
	var ranks []PDRank
	for _, id := range dep.NodeIDList() {
		uuid, ok := byID[id]
		if !ok {
			return out
		}
		ranks = append(ranks, PDRank{NodeUUID: uuid})
	}
	if len(ranks) > 0 {
		out[replicaNodesKey(ranks)] = true
	}
	return out
}

// replicaNodesKey names a replica by the nodes of its ranks, in rank order.
func replicaNodesKey(ranks []PDRank) string {
	uuids := make([]string, 0, len(ranks))
	for _, rank := range ranks {
		uuids = append(uuids, rank.NodeUUID)
	}
	return strings.Join(uuids, "|")
}
