package deployment

import (
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// IsAutomaticPlacement reports whether the request asked the server to pick
// the node (placement_mode "auto") instead of naming one explicitly. It is a
// request-only word: what a deployment stores is its node requirements, and
// a manual placement is a requirement naming the node.
func IsAutomaticPlacement(req *dto.DeployRequest) bool {
	return req != nil && strings.EqualFold(strings.TrimSpace(req.PlacementMode), "auto")
}

// requestRequirements is where the request says the deployment may run, as
// the requirements the deployment stores: node_requirements as stated,
// target_vendor as the vendor requirement, and a manually placed single-node
// request pinned to its node.
func requestRequirements(req *dto.DeployRequest) (map[string][]string, error) {
	return requirementsOf(req, true)
}

// requestPool is the requirements without the pin a manual request adds for
// the node it names: the pool the node is chosen from. The deploy form's node
// dropdown is judged against it, since it is where that node gets picked.
func requestPool(req *dto.DeployRequest) (map[string][]string, error) {
	return requirementsOf(req, false)
}

func requirementsOf(req *dto.DeployRequest, pinManual bool) (map[string][]string, error) {
	perRank := req.NNodes > 1 || placementPD(req) != nil
	manual := ""
	if pinManual && !perRank && !IsAutomaticPlacement(req) {
		manual = strings.TrimSpace(req.NodeUUID)
	}
	// An unsupported target_vendor is reported as itself (TARGET_VENDOR_INVALID)
	// by the create and the evaluation, so it does not also fail here.
	return planning.RequestRequirements(planning.RequirementsRequest{
		Stated:       req.NodeRequirements,
		TargetVendor: accelerator.NormalizeVendor(req.TargetVendor),
		ManualNode:   manual,
		PerRank:      perRank,
	})
}

// RequestPlacement reads where a create request may run - its requirements
// and placement strategy - and checks the nodes it names against them. An
// automatic create whose evaluated node no longer satisfies the requirements
// is the same conflict as any other stale evaluation, not a wrong request.
func (s *Service) RequestPlacement(req *dto.DeployRequest) (map[string][]string, store.PlacementStrategy, error) {
	strategy, err := planning.PlacementStrategyOf(req.PlacementStrategy)
	if err != nil {
		return nil, "", invalid(err)
	}
	reqs, err := requestRequirements(req)
	if err != nil {
		return nil, "", invalid(err)
	}
	if err := s.validateRequestNodes(req, reqs); err != nil {
		if IsAutomaticPlacement(req) && req.NodeUUID != "" {
			return nil, "", conflict(wrapProductError(messages.ErrorAutomaticPlacementStale, "the evaluated node no longer satisfies the node requirements", map[string]any{"node": req.NodeUUID, "detail": err.Error()}, err))
		}
		return nil, "", invalid(err)
	}
	return reqs, strategy, nil
}

// validateRequestNodes checks every node the request names against its
// requirements. An automatic request that names no node yet has nothing to
// check: its candidates are judged by evaluateEligibleNodes. A PD request's
// nodes are checked where they are read (preparePDDeployRequest).
func (s *Service) validateRequestNodes(req *dto.DeployRequest, reqs map[string][]string) error {
	if placementPD(req) != nil {
		return nil
	}
	uuids := req.NodeUUIDs
	if len(uuids) == 0 && req.NodeUUID != "" {
		uuids = []string{req.NodeUUID}
	}
	nodes := make([]*store.Node, 0, len(uuids))
	for _, uuid := range uuids {
		node, err := s.d.Store.NodeWithGPUs(uuid)
		if errors.Is(err, store.ErrNotFound) {
			continue // the node lookup that follows names the missing node
		}
		if err != nil {
			return err
		}
		nodes = append(nodes, node)
	}
	return s.matchNodes(nodes, reqs)
}

// matchNodes checks each node against requirements, resolving the nodes'
// networks once when a requirement names one. A resolution that cannot be
// read leaves every network requirement unmet.
func (s *Service) matchNodes(nodes []*store.Node, reqs map[string][]string) error {
	if len(reqs) == 0 || len(nodes) == 0 {
		return nil
	}
	resolved := s.resolveForRequirements(nodes, reqs)
	for _, node := range nodes {
		if err := planning.MatchRequirements(node, resolved, reqs); err != nil {
			return planning.UnmetRequirementsError(err)
		}
	}
	return nil
}

// resolveForRequirements loads what matching reqs against nodes reads beyond
// the node rows: their networks and the storages that apply to them, each
// only when a requirement names one. A part that cannot be loaded stays nil,
// which leaves its requirements unmet.
func (s *Service) resolveForRequirements(nodes []*store.Node, reqs map[string][]string) planning.NodeFacts {
	rows := make([]store.Node, 0, len(nodes))
	for _, node := range nodes {
		rows = append(rows, *node)
	}
	return planning.LoadNodeFacts(s.d.Store, s.networks().Resolve, rows, reqs)
}

// StoredPlacement is the placement document a deployment keeps. The two
// top-level keys nothing reads - "mode" and "node_uuid", left over from an
// older form - are dropped, so they are never persisted as if they meant
// something; the nested node_uuid of PD and multi-node units is kept. An
// automatic single-node create states the cards its evaluation chose only so
// the create can recheck them: its replicas let the scheduler pick cards on
// whichever node they land, so the cards are not kept either - even when its
// requirements name one node, which would otherwise read as a pin.
func StoredPlacement(placement map[string]any, automatic bool) map[string]any {
	if placement == nil {
		return nil
	}
	out := make(map[string]any, len(placement))
	for key, value := range placement {
		if key == "mode" || key == "node_uuid" {
			continue
		}
		if automatic && (key == "gpus" || key == "gpu_indexes") {
			continue
		}
		out[key] = value
	}
	return out
}

// RequestNode is the node a single-node create names, with its GPUs.
// Automatic placement names the node its evaluation picked, so a create names
// one either way; naming none, or a node that does not exist, is a field
// error rather than the store's bare "not found".
func (s *Service) RequestNode(uuid string) (*store.Node, error) {
	node, err := s.d.Store.NodeWithGPUs(uuid)
	if errors.Is(err, store.ErrNotFound) {
		return nil, wrapProductError(messages.ErrorFieldInvalid, "node_uuid must name an existing node", map[string]any{"field": "node_uuid", "detail": uuid}, err)
	}
	return node, err
}
