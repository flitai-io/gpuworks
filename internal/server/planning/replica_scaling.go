package planning

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Scaling grows and shrinks a deployment's replicas: the one path an
// operator's PUT /deployments/{id}/replicas and PUT /deployments/{id}/pd
// and the automation's scheduled replica counts take. A multi-node or PD
// deployment the platform places gets the nodes of its new replicas picked
// as at creation (ChooseReplicaNodes, ChoosePDUnits).
type Scaling struct {
	Store *store.Store
	// Resolve resolves the networks of nodes (network.Service.Resolve).
	Resolve func([]store.Node) (netplan.Resolution, error)
	// Online says a node's agent is connected; nil reads the node's stored
	// state.
	Online func(nodeUUID string) bool
	// UCXNetlinkHang is the live UCX hang fact of a node (the hub's); nil
	// leaves no node out of a PD placement.
	UCXNetlinkHang UCXNetlinkHangOf
}

// ScaleErrorKind is what a scaling failure is, for the caller's response:
// the request is invalid, the cluster cannot take it now, or the deployment
// is gone. A failure of no kind is the server's own.
type ScaleErrorKind int

const (
	ScaleInvalid ScaleErrorKind = iota + 1
	ScaleConflict
	ScaleGone
)

// ScaleError is a scaling failure of a kind; Err is the product error (or the
// store's not found) it carries.
type ScaleError struct {
	Kind ScaleErrorKind
	Err  error
}

func (e *ScaleError) Error() string { return e.Err.Error() }
func (e *ScaleError) Unwrap() error { return e.Err }

func scaleErr(kind ScaleErrorKind, err error) error { return &ScaleError{Kind: kind, Err: err} }

// ScaleErrorKindOf is the kind of a scaling failure and the error it
// carries; 0 for a failure of the server's own.
func ScaleErrorKindOf(err error) (ScaleErrorKind, error) {
	var scale *ScaleError
	if errors.As(err, &scale) {
		return scale.Kind, scale.Err
	}
	return 0, err
}

// UnitPlacementNone says no group of nodes can take the request, with how
// many nodes each check left out.
func UnitPlacementNone(counts UnitSuggestCounts) error {
	params := map[string]any{
		"nodes": counts.Nodes, "unmet": counts.Unmet, "cordoned": counts.Cordoned, "offline": counts.Offline,
		"busy": counts.Busy, "no_network": counts.NoNetwork, "no_peer_memory": counts.NoPeerMemory, "largest": counts.Largest,
	}
	if counts.UCXNetlink > 0 {
		// Nodes left out because their kernel hangs UCX, and the first few
		// by name, so the operator knows which to fix (pdmode.UCXNetlinkFixes).
		params["ucx_netlink_hang"] = counts.UCXNetlink
		params["ucx_netlink_nodes"] = counts.UCXNetlinkNodes
	}
	return messages.NewProductError(messages.ErrorUnitPlacementNone, "no group of nodes can take the deployment now", params)
}

// PDPlacementNone is UnitPlacementNone for a PD mode over RDMA when the
// peer-memory module kept nodes out: the operator learns which module to load
// and how, rather than only that nothing fits.
func PDPlacementNone(mode pdmode.Mode, vendor string, counts UnitSuggestCounts) error {
	if !mode.PeerMemory || counts.NoPeerMemory == 0 {
		return UnitPlacementNone(counts)
	}
	module, fix := accelerator.PeerMemoryModule(vendor), accelerator.PeerMemoryFix(vendor)
	return messages.NewProductError(messages.ErrorPDPeerMemoryNone,
		fmt.Sprintf("no node can take the deployment: %d of %d nodes cannot register their cards' memory with the RDMA NIC (%s not loaded); run on them: %s", counts.NoPeerMemory, counts.Nodes, module, fix),
		map[string]any{"mode": mode.Name, "nodes": counts.Nodes, "no_peer_memory": counts.NoPeerMemory, "module": module, "fix": fix})
}

// ScaleScheduled sets a deployment's replica count for a caller inside the
// server (the scheduling automation) the way the API does for an operator:
// a single-node deployment records the desired count, a multi-node
// deployment placed automatically gets the units AutoReplicaUnits picks (new
// replicas on nodes chosen as at creation, a shrink dropping the newest),
// and a PD deployment's role gets its count through the PD update
// (ApplyPDUpdate), which also holds a role to its mode's minimum. A refusal
// is a product error with its code: a deployment placed by hand, no
// placement for a new replica, a role below its minimum. The count of a
// non-PD deployment is clamped to ReplicaBounds; a PD role's is refused
// outside them.
func (s Scaling) ScaleScheduled(deploymentID uint, role string, replicas int) error {
	dep, err := s.Store.GetDeployment(deploymentID)
	if err != nil {
		return err
	}
	if err := ScheduledReplicasRefusal(dep, role); err != nil {
		return err
	}
	if pd, _ := PDOf(dep.Placement); pd != nil {
		return s.scalePDRole(dep, pd, role, replicas)
	}
	lowest, highest := ReplicaBounds(dep, role)
	replicas = min(max(replicas, lowest), highest)
	if max(dep.Replicas, 1) == replicas {
		return nil
	}
	if dep.NNodes > 1 {
		units, err := s.AutoReplicaUnits(dep, replicas, nil, nil)
		if err != nil {
			return err
		}
		return s.ApplyReplicaUnits(dep, units)
	}
	return s.Store.ScaleDeploymentReplicas(dep.ID, dep.Replicas, replicas, nil)
}

// scalePDRole gives one role of a PD deployment placed by the platform a
// count of units: a shrink drops the newest, a grow adds units with no node,
// which the PD update places beside the ones that are.
func (s Scaling) scalePDRole(dep *store.Deployment, pd *PDPlacement, role string, replicas int) error {
	lowest, highest := ReplicaBounds(dep, role)
	if replicas < lowest || replicas > highest {
		return ReplicasOutOfBounds(role, replicas, lowest, highest)
	}
	units := slices.Clone(pd.Units(role))
	if len(units) == replicas {
		return nil
	}
	if len(units) > replicas {
		units = units[:replicas]
	}
	for len(units) < replicas {
		units = append(units, PDUnit{})
	}
	shape := pd.Roles[role]
	shape.Units = units
	_, err := s.ApplyPDUpdate(dep, pd, PDUpdate{Roles: map[string]PDRole{role: shape}})
	return err
}

// ApplyPDUpdate applies a PD update to a running deployment and stores the
// placement it moves to: the nodes of new units are filled in for a
// deployment the platform places (AutoPDRoles), the result is checked
// against the mode and the nodes, and the placement is written. update.Roles
// gets the nodes filled in.
func (s Scaling) ApplyPDUpdate(dep *store.Deployment, current *PDPlacement, update PDUpdate) (*PDPlacement, error) {
	if dep.UnitsAuto && len(update.Roles) > 0 {
		if err := s.AutoPDRoles(dep, current, update.Roles); err != nil {
			return nil, err
		}
	}
	applied, err := ApplyPDUpdate(current, dep.Engine, update)
	if err != nil {
		return nil, scaleErr(ScaleInvalid, err)
	}
	next := applied.Placement
	nodes := map[string]*store.Node{}
	var nodeIDs []uint
	for _, uuid := range next.NodeUUIDs(applied.Mode) {
		node, err := s.Store.NodeWithGPUs(uuid)
		if errors.Is(err, store.ErrNotFound) {
			continue // ValidatePDNodes names the missing node
		}
		if err != nil {
			return nil, err
		}
		nodes[uuid] = node
		nodeIDs = append(nodeIDs, node.ID)
	}
	if _, err := ValidatePDNodes(next, applied.Mode, nodes, false); err != nil {
		return nil, scaleErr(ScaleInvalid, err)
	}
	encoded, err := WithPD(dep.Placement, next)
	if err != nil {
		return nil, err
	}
	primary := dep.NodeID
	if len(nodeIDs) > 0 {
		primary = nodeIDs[0]
	}
	nodeIDsJSON, _ := json.Marshal(nodeIDs)
	if err := s.Store.SetDeploymentPlacement(dep.ID, encoded, string(nodeIDsJSON), primary); err != nil {
		return nil, err
	}
	return next, nil
}

// AutoPDRoles fills the nodes of the new units a PD update leaves out, for a
// deployment the platform places: beside the units that are placed, in the
// group they are in. A named node is the new unit's first placement and is
// checked with the rest by ValidatePDNodes.
func (s Scaling) AutoPDRoles(dep *store.Deployment, current *PDPlacement, roles map[string]PDRole) error {
	mode, ok := pdmode.Lookup(current.Mode)
	if !ok {
		return nil
	}
	fixed := map[string][]PDUnit{}
	var needs []PDRoleNeed
	open := false
	for _, role := range mode.Roles {
		r, stated := roles[role]
		if !stated {
			fixed[role] = append(fixed[role], current.Units(role)...)
			continue
		}
		need := PDRoleNeed{Role: role}
		for _, unit := range r.Units {
			if unit.NodeUUID != "" {
				fixed[role] = append(fixed[role], unit)
				continue
			}
			if len(unit.Followers) > 0 {
				return scaleErr(ScaleInvalid, messages.NewProductError(messages.ErrorAutoPlacementPDMultiNode, fmt.Sprintf("automatic placement takes PD units that fit one node; the %s role's units span nodes - name their nodes", role), map[string]any{"role": role}))
			}
			need.Units++
			need.GPUs = current.RankGPUs(role, unit)
			open = true
		}
		if need.Units > 0 {
			needs = append(needs, need)
		}
	}
	if !open {
		return nil
	}
	reqs, err := s.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		return err
	}
	facts, err := LoadUnitSuggestFacts(s.Store, s.Resolve, dep, reqs, ListedPDRanks(current, fixed), s.Online, s.UCXNetlinkHang, 1, 1)
	if err != nil {
		return err
	}
	chosen, counts, ok := ChoosePDUnits(facts, needs, fixed, dep.PlacementStrategy, func(map[string][]PDUnit) string { return "" })
	if !ok {
		return scaleErr(ScaleConflict, UnitPlacementNone(counts))
	}
	for role := range roles {
		r := roles[role]
		r.Units = chosen[role]
		roles[role] = r
	}
	return nil
}

// ListedPDRanks are the cards the units will take on their nodes, as ranks
// the choice must leave alone: the units that stay where they are claim their
// cards before the others are placed beside them.
func ListedPDRanks(pd *PDPlacement, units map[string][]PDUnit) []ListedRank {
	var out []ListedRank
	for role, list := range units {
		for _, unit := range list {
			gpus := pd.RankGPUs(role, unit)
			for _, rank := range unit.Ranks() {
				out = append(out, ListedRank{NodeUUID: rank.NodeUUID, GPUs: gpus})
			}
		}
	}
	return out
}
