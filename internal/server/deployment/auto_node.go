package deployment

import (
	"context"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// leavesNodeToServer reports whether req is an automatic single-node create
// that names no node.
func leavesNodeToServer(req *dto.DeployRequest) bool {
	return IsAutomaticPlacement(req) && req.NNodes <= 1 && placementPD(req) == nil && strings.TrimSpace(req.NodeUUID) == ""
}

// pickAutomaticNode lets an automatic single-node create name no node: it
// runs the evaluation the deploy form runs before it submits and names the
// node that evaluation picked, so an API client need not evaluate first. The
// create then re-validates the pick like any other, so a request that names
// its evaluated node behaves exactly as before. When the evaluation picks no
// node, the create is refused with AUTOMATIC_PLACEMENT_UNAVAILABLE and each
// candidate's reason, the same reasons the evaluation lists.
func (s *Service) pickAutomaticNode(ctx context.Context, req *dto.DeployRequest) error {
	if !leavesNodeToServer(req) {
		return nil
	}
	// The evaluation normalizes the request it previews; it works on a copy
	// so the create goes on from the request as it was sent.
	evalReq := dto.EvaluateRequest{DeployRequest: cloneRequest(*req)}
	out := &evaluation{}
	s.evaluate(ctx, out, evalReq)
	if out.err != nil {
		// The evaluation refused the request itself; the create's own checks
		// below answer it with their usual error.
		return nil //nolint:nilerr // Creation performs its own validation when the optional placement evaluation cannot answer.
	}
	resp := out.resp
	if resp.SelectedNode != nil {
		req.NodeUUID = resp.SelectedNode.UUID
		if s.d.AfterAutomaticPick != nil {
			s.d.AfterAutomaticPick(req.NodeUUID)
		}
		return nil
	}
	if len(resp.Errors) > 0 {
		return nil // a field error: the create reports it as a wrong request
	}
	reasons := make([]string, 0, len(resp.Candidates))
	for _, candidate := range resp.Candidates {
		reasons = append(reasons, candidate.Reason.Code)
	}
	return conflict(productError(messages.ErrorAutomaticPlacementUnavailable, "no node satisfies the automatic placement", map[string]any{"candidates": len(resp.Candidates), "reasons": reasons}))
}
