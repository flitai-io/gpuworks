package deployment

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/planning"
)

// planLaunch is planning.PlanLaunch for a preview of the command the
// controller would send req.Node. The controller decides
// LaunchRequest.ImplicitServedName from the node's agent at launch time
// (planning.TranslatesServedName); a preview that skipped it would show, and
// seed a manual command with, a --served-model-name an older host-process
// agent translates to a host path.
func (s *Service) planLaunch(ctx context.Context, req planning.LaunchRequest) (planning.LaunchPlan, error) {
	if req.Node != nil {
		var caps []string
		if s.d.NodeCaps != nil {
			caps = s.d.NodeCaps(req.Node.UUID)
		}
		req.ImplicitServedName = planning.TranslatesServedName(req.Node.RuntimeDriver, caps)
	}
	return planning.PlanLaunch(ctx, s.d.Store, s.networks(), req)
}
