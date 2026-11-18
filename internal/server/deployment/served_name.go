package deployment

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// planLaunch is planning.PlanLaunch for a preview of the command the
// controller would send req.Node. The controller decides
// LaunchRequest.ImplicitServedName from the node's agent at launch time
// (planning.TranslatesServedName); a preview that skipped it would show, and
// seed a manual command with, a --served-model-name an older host-process
// agent translates to a host path. The agent's capabilities decide the peers
// file and the refusals of facilities it lacks too (planning.peersForAgent,
// agentCapabilityRefusal).
func (s *Service) planLaunch(ctx context.Context, req planning.LaunchRequest) (planning.LaunchPlan, error) {
	if req.Node != nil {
		var caps []string
		if s.d.NodeCaps != nil {
			caps = s.d.NodeCaps(req.Node.UUID)
		}
		req.ImplicitServedName = planning.TranslatesServedName(req.Node.RuntimeDriver, caps)
		// The controller refuses a launch using a facility the node's agent
		// lacks (planning.agentCapabilityRefusal); the preview says so too.
		req.AgentCaps = caps
	}
	return planning.PlanLaunch(ctx, s.d.Store, s.networks(), req)
}

func (s *Service) implicitServedName(node *store.Node) bool {
	var caps []string
	if s.d.NodeCaps != nil {
		caps = s.d.NodeCaps(node.UUID)
	}
	return planning.TranslatesServedName(node.RuntimeDriver, caps)
}
