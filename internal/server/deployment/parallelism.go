package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// requestPlacementDP uses the same request materializer as launch. In
// particular, a quick field or an explicit unset can replace a template's DP;
// reading the template flag again after that merge would undo the user's edit.
func requestPlacementDP(req *dto.DeployRequest, templateArgs []string) (int, error) {
	engine, err := deploy.SelectEngine(req.Engine)
	if err != nil {
		return 0, err
	}
	launch, err := materializeDeployRequest(engine.Name(), req, templateSelection{ExtraArgs: templateArgs}, nil, deploy.ImageEngineVersion(engine.Name(), req.Image, ""))
	if err != nil {
		return 0, err
	}
	return deploy.PlacementDataParallel(engine.Name(), launch.Quick.DP, launch.TemplateResidual, launch.UserResidual)
}

// automaticReplicaDP reads a template before choosing a multi-node group's
// capacity and validates its node limit. A named spec resolves without a node.
// An automatic spec uses the proposed leader. The draft owns its materialized
// fields so a rejected candidate cannot change the caller's request.
func (s *Service) automaticReplicaDP(req *dto.DeployRequest, node *store.Node) (int, error) {
	if req.TemplateID == 0 {
		return requestPlacementDP(req, nil)
	}
	draft := *req
	selection, err := s.applyTemplateDefaults(&draft, nodeTemplateHardware(node))
	if err != nil {
		return 0, err
	}
	if err := (deploy.CatalogSelection{Placement: selection.Placement}).ValidatePlacementNodes(draft.NNodes); err != nil {
		return 0, err
	}
	launch, err := s.materializeCreateLaunch(draft.Engine, &draft, selection, nil, s.imageEngineVersion(draft.Engine, draft.Image))
	if err != nil {
		return 0, err
	}
	return deploy.PlacementDataParallel(draft.Engine, launch.Quick.DP, launch.TemplateResidual, launch.UserResidual)
}
