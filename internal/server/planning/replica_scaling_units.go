package planning

import (
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// AutoReplicaUnits are a multi-node deployment's replicas grown or shrunk to
// replicas. placeOn names nodes for the new replicas' ranks in order - the
// first new replica's ranks, then the next's - and remove names instances
// whose replicas go first when it shrinks.
func (s Scaling) AutoReplicaUnits(dep *store.Deployment, replicas int, placeOn, remove []string) ([]PDUnit, error) {
	order, err := s.deploymentNodeUUIDs(dep)
	if err != nil {
		return nil, err
	}
	current := ReplicaUnitsOf(dep, order)
	added := replicas - len(current)
	if len(placeOn) > max(added, 0)*dep.NNodes {
		return nil, scaleErr(ScaleInvalid, messages.NewProductError(messages.ErrorReplicasPlaceOnTooMany, "more nodes named than the new replicas have ranks", map[string]any{"count": len(placeOn), "maximum": max(added, 0) * dep.NNodes}))
	}
	if added <= 0 {
		return s.shrinkReplicaUnits(dep, current, replicas, remove)
	}
	if len(remove) > 0 {
		// As for a single-node deployment: instances are named only for the
		// replicas a shrink drops.
		return nil, scaleErr(ScaleInvalid, messages.NewProductError(messages.ErrorReplicasRemoveTooMany, "more instances named than replicas removed", map[string]any{"count": len(remove), "maximum": 0}))
	}
	reqs, err := s.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		return nil, err
	}
	placement, err := ParsePlacement(dep.Placement)
	if err != nil {
		return nil, err
	}
	spec, err := DeploymentParallelism(dep, placement)
	if err != nil {
		return nil, err
	}
	perRank, err := spec.GPUsPerNode()
	if err != nil || perRank < 1 {
		perRank = 1
	}
	nodes, err := s.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	units := append([]PDUnit(nil), current...)
	for k := 0; k < added; k++ {
		var listed []ListedRank
		for _, unit := range units {
			for i, rank := range unit.Ranks() {
				listed = append(listed, ListedRank{NodeUUID: rank.NodeUUID, GPUs: perRank, Leader: i == 0})
			}
		}
		facts, err := LoadUnitSuggestFacts(s.Store, s.Resolve, dep, reqs, listed, s.online(nodes), dep.NNodes, perRank)
		if err != nil {
			return nil, err
		}
		fixed := placeOn[min(k*dep.NNodes, len(placeOn)):min((k+1)*dep.NNodes, len(placeOn))]
		chosen, counts, ok := ChooseReplicaNodes(facts, dep.PlacementStrategy, fixed, func([]*store.Node) int { return -1 })
		if !ok {
			if len(fixed) > 0 {
				name := fixed[0]
				if node, err := s.Store.NodeByUUID(fixed[0]); err == nil {
					name = node.DisplayName()
				}
				return nil, scaleErr(ScaleConflict, messages.NewProductError(messages.ErrorReplicasPlaceOnRefused, fmt.Sprintf("the named nodes %v cannot carry a replica now", fixed), map[string]any{"node": name, "reason": "no_group", "detail": ""}))
			}
			return nil, scaleErr(ScaleConflict, UnitPlacementNone(counts))
		}
		unit := PDUnit{NodeUUID: chosen[0].UUID}
		for _, node := range chosen[1:] {
			unit.Followers = append(unit.Followers, PDRank{NodeUUID: node.UUID})
		}
		units = append(units, unit)
	}
	return units, nil
}

// shrinkReplicaUnits drops replicas down to replicas: those the named
// instances lead or follow in first, then the newest.
func (s Scaling) shrinkReplicaUnits(dep *store.Deployment, current []PDUnit, replicas int, remove []string) ([]PDUnit, error) {
	if replicas < 1 {
		return nil, scaleErr(ScaleInvalid, messages.NewProductError(messages.ErrorReplicasTooFew, "replicas must be at least 1; stop the deployment to run none", map[string]any{"minimum": 1}))
	}
	invalid := func(uuid string) error {
		return scaleErr(ScaleInvalid, messages.NewProductError(messages.ErrorReplicasRemoveInvalid, "a named instance does not fill a replica of this deployment", map[string]any{"instance": uuid}))
	}
	drop := map[int]bool{}
	for _, uuid := range remove {
		inst, err := s.Store.InstanceByUUID(uuid)
		if err != nil || inst.DeploymentID != dep.ID {
			return nil, invalid(uuid)
		}
		node, err := s.Store.NodeByID(inst.NodeID)
		if err != nil {
			return nil, invalid(uuid)
		}
		for i, unit := range current {
			for _, rank := range unit.Ranks() {
				if rank.NodeUUID == node.UUID {
					drop[i] = true
				}
			}
		}
	}
	if len(current)-len(drop) < replicas {
		return nil, scaleErr(ScaleInvalid, messages.NewProductError(messages.ErrorReplicasRemoveTooMany, "more instances named than replicas removed", map[string]any{"count": len(remove), "maximum": max(len(current)-replicas, 0)}))
	}
	var out []PDUnit
	for i, unit := range current {
		if !drop[i] {
			out = append(out, unit)
		}
	}
	return out[:replicas], nil
}

// deploymentNodeUUIDs are the deployment's stored nodes in order.
func (s Scaling) deploymentNodeUUIDs(dep *store.Deployment) ([]string, error) {
	var out []string
	for _, id := range dep.NodeIDList() {
		node, err := s.Store.NodeByID(id)
		if err != nil {
			return nil, err
		}
		out = append(out, node.UUID)
	}
	return out, nil
}

// ApplyReplicaUnits writes a multi-node deployment's replicas, as
// PlanReplicaUnits checks and derives them.
func (s Scaling) ApplyReplicaUnits(dep *store.Deployment, units []PDUnit) error {
	nodes := map[string]*store.Node{}
	rows := []store.Node{}
	for _, uuid := range ReplicaUnitNodes(units) {
		node, err := s.Store.NodeWithGPUs(uuid)
		if errors.Is(err, store.ErrNotFound) {
			continue // PlanReplicaUnits names the unknown node
		}
		if err != nil {
			return err
		}
		nodes[uuid] = node
		rows = append(rows, *node)
	}
	reqs, err := s.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		return err
	}
	patch, err := PlanReplicaUnits(dep, reqs, LoadNodeFacts(s.Store, s.Resolve, rows, reqs), units, nodes)
	if err != nil {
		return scaleErr(ScaleInvalid, err)
	}
	if err := s.Store.SetDeploymentReplicaUnits(dep.ID, patch); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Deleted while the request was read.
			return scaleErr(ScaleGone, err)
		}
		return err
	}
	return nil
}
