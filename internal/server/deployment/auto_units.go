package deployment

import (
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// autoPlaceRequest fills the nodes an automatic multi-node or PD request
// leaves out and reports whether the request was one; the deployment it
// creates is then placed by the platform from then on.
func (s *Service) autoPlaceRequest(req *dto.DeployRequest) (bool, error) {
	pd := placementPD(req)
	if !IsAutomaticPlacement(req) || (req.NNodes <= 1 && pd == nil) {
		return false, nil
	}
	if req.PlacementStrategy == "" {
		req.PlacementStrategy = string(store.PlacementBinpack)
	}
	strategy, err := planning.PlacementStrategyOf(req.PlacementStrategy)
	if err != nil {
		return false, err
	}
	reqs, err := requestPool(req)
	if err != nil {
		return false, err
	}
	nodes, err := s.d.Store.ListNodes()
	if err != nil {
		return false, err
	}
	var named []planning.ListedRank
	if pd != nil {
		named = planning.ListedPDRanks(pd, namedPDUnits(pd))
	}
	facts, err := planning.LoadUnitSuggestFacts(s.d.Store, s.networks().Resolve, &store.Deployment{ModelID: req.ModelID}, reqs, named, s.onlineOf(nodes), 1, 1)
	if err != nil {
		return false, err
	}
	if pd != nil {
		if err := autoPlacePD(req, pd, facts, strategy); err != nil {
			return false, err
		}
	} else if err := s.autoPlaceReplica(req, facts, strategy); err != nil {
		return false, err
	}
	// The request now names every node: from here it is checked and stored
	// like one that named them itself.
	req.PlacementMode = ""
	return true, nil
}

// autoPlaceReplica picks the ranks of a multi-node request it does not name;
// the nodes it names take the first ranks.
func (s *Service) autoPlaceReplica(req *dto.DeployRequest, facts planning.UnitSuggestFacts, strategy store.PlacementStrategy) error {
	placement, err := planning.ParsePlacement(req.Placement)
	if err != nil {
		return err
	}
	if placement.TP <= 0 {
		return productError(messages.ErrorAutoPlacementTPRequired, "automatic placement of a multi-node deployment needs placement.tp: how many GPUs each node gives a rank", nil)
	}
	parallel := placement.Parallelism(req.NNodes)
	// Named specs have a known launch contract before a node is chosen.
	// Unnamed specs depend on the proposed hardware and are checked below.
	dynamicSpec := req.TemplateID != 0 && req.TemplateSpecName == ""
	perRank := 1 // a lower bound until a hardware-dependent spec is resolved
	if !dynamicSpec {
		parallel.DP, err = s.automaticReplicaDP(req, nil)
		if err != nil {
			return err
		}
		perRank, err = parallel.GPUsPerNode()
		if err != nil {
			return err
		}
	}
	if len(req.NodeUUIDs) > req.NNodes {
		return productError(messages.ErrorDistributedNodeCount, fmt.Sprintf("%d nodes named for %d ranks", len(req.NodeUUIDs), req.NNodes), map[string]any{"count": len(req.NodeUUIDs), "nnodes": req.NNodes})
	}
	facts.Ranks, facts.PerRank = req.NNodes, perRank
	var candidateErr error
	chosen, counts, ok := planning.ChooseReplicaNodes(facts, strategy, req.NodeUUIDs, func(nodes []*store.Node) int {
		if !dynamicSpec {
			return -1
		}
		candidateErr = nil
		candidate := parallel
		candidate.DP, candidateErr = s.automaticReplicaDP(req, nodes[0])
		if candidateErr != nil {
			return 0
		}
		need, err := candidate.GPUsPerNode()
		if err != nil {
			candidateErr = err
			return 0
		}
		for rank, node := range nodes {
			if planning.FreeHealthyGPUs(node, facts.Held[node.ID])-facts.Claimed[node.UUID] < need {
				return rank
			}
		}
		return -1
	})
	if !ok {
		if candidateErr != nil {
			return candidateErr
		}
		return planning.UnitPlacementNone(counts)
	}
	req.NodeUUIDs = req.NodeUUIDs[:0]
	for _, node := range chosen {
		req.NodeUUIDs = append(req.NodeUUIDs, node.UUID)
	}
	// The leader's address on the group's network is the master address,
	// read when the request's nodes are.
	req.MasterAddr = ""
	return nil
}

// autoPlacePD picks a node for every unit of a PD request that names none;
// the units that name one stay where they are.
func autoPlacePD(req *dto.DeployRequest, pd *planning.PDPlacement, facts planning.UnitSuggestFacts, strategy store.PlacementStrategy) error {
	fixed := map[string][]planning.PDUnit{}
	var needs []planning.PDRoleNeed
	mode, ok := pdmode.Lookup(pd.Mode)
	if !ok {
		return nil // the PD validation that follows names the unknown mode
	}
	for _, role := range mode.Roles {
		need := planning.PDRoleNeed{Role: role}
		for _, unit := range pd.Units(role) {
			if unit.NodeUUID != "" {
				fixed[role] = append(fixed[role], unit)
				continue
			}
			if len(unit.Followers) > 0 {
				return productError(messages.ErrorAutoPlacementPDMultiNode, fmt.Sprintf("automatic placement takes PD units that fit one node; the %s role's units span nodes - name their nodes", role), map[string]any{"role": role})
			}
			need.Units++
			need.GPUs = pd.RankGPUs(role, unit)
		}
		if need.Units > 0 {
			needs = append(needs, need)
		}
	}
	if len(needs) == 0 {
		return nil
	}
	if mode.SameNode {
		// The KV never leaves the host: pack the roles onto one node, and
		// let the PD validation refuse a placement that still spreads.
		strategy = store.PlacementBinpack
	}
	// A mode over RDMA places only where the NICs can register the cards.
	facts.PeerMemory = mode.PeerMemory
	chosen, counts, ok := planning.ChoosePDUnits(facts, needs, fixed, strategy, func(map[string][]planning.PDUnit) string { return "" })
	if !ok {
		vendor := ""
		if len(mode.Vendors) > 0 {
			vendor = mode.Vendors[0]
		}
		return planning.PDPlacementNone(mode, vendor, counts)
	}
	for role, units := range chosen {
		r := pd.Roles[role]
		r.Units = units
		pd.Roles[role] = r
	}
	return setPlacementPD(req, pd)
}

// namedPDUnits are the units of pd that already name their node.
func namedPDUnits(pd *planning.PDPlacement) map[string][]planning.PDUnit {
	mode, ok := pdmode.Lookup(pd.Mode)
	if !ok {
		return nil
	}
	out := map[string][]planning.PDUnit{}
	for _, role := range mode.Roles {
		for _, unit := range pd.Units(role) {
			if unit.NodeUUID != "" {
				out[role] = append(out[role], unit)
			}
		}
	}
	return out
}

// setPlacementPD writes pd back into the request's placement document.
func setPlacementPD(req *dto.DeployRequest, pd *planning.PDPlacement) error {
	raw, err := json.Marshal(pd)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if req.Placement == nil {
		req.Placement = map[string]any{}
	}
	req.Placement["pd"] = doc
	return nil
}

// unitPlacementKind is how an automatic placement failure reads: a request
// the cluster cannot take now is a conflict, a malformed one a wrong request.
func unitPlacementKind(err error) error {
	if containsProductCode(err, messages.ErrorUnitPlacementNone) {
		return conflict(err)
	}
	return invalid(err)
}

// defaultUnitStrategy is binpack for a multi-node or PD request that states
// no placement strategy: such a deployment takes whole groups of cards, and
// packing them leaves whole nodes free for the next.
func defaultUnitStrategy(req *dto.DeployRequest) {
	if req.PlacementStrategy == "" && (req.NNodes > 1 || placementPD(req) != nil) {
		req.PlacementStrategy = string(store.PlacementBinpack)
	}
}
