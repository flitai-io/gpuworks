package deployment

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// launchPreflight is the launch a create resolves before it registers a model
// or acquires an image: on the node it names, with the per-node card count
// its stated parallelism asks for. Create runs it to refuse a request no node
// could launch, and Evaluate runs the same step on the same inputs, so a draft
// the create would refuse here is a blocking field error in the preview - the
// scheduler may place a memory-limited draft on fewer cards than it states,
// and only this step measures the request against its own statement.
type launchPreflight struct {
	modelMount      string
	componentMounts []proto.MountSpec
	placement       planning.Placement
	input           deploy.ResolveLaunchInput
}

// prepareLaunchPreflight assembles the preflight from a normalized request and
// its create materialization (materializeCreateLaunch). dp is the request's
// placement data-parallel degree; profileNode is the node it names (nil for an
// automatic draft no node was picked for yet). It reads the deployment policy
// and the components' model files on profileNode, nothing per fleet node.
func (s *Service) prepareLaunchPreflight(eng deploy.Engine, req *dto.DeployRequest, selection templateSelection, launch deploy.Materialized, dp int, targetVendor string, profileNode *store.Node, pdReq *pdRequest) (launchPreflight, error) {
	var out launchPreflight
	out.modelMount, out.componentMounts = previewComponentMounts(s.d.Store, selection.Components, profileNode)
	placement, err := planning.ParsePlacement(req.Placement)
	if err != nil {
		return out, err
	}
	out.placement = placement
	nnodes := req.NNodes
	if nnodes <= 0 {
		nnodes = 1
	}
	parallel := placement.Parallelism(nnodes)
	parallel.DP = dp
	perNode, err := parallelGPUsPerNode(parallel)
	if err != nil {
		return out, err
	}
	params := deploy.Params{
		ModelPath: out.modelMount, ServedName: req.ServedName, APIPort: planning.PreviewAPIPort,
		AssignedGPUs: perNode, TensorParallel: parallel.TP, PipelineParallel: parallel.PP, Sleepable: req.Sleepable, Quick: launch.Quick,
		TemplateArgs: launch.TemplateResidual, TemplateEnv: selection.Env, ExtraArgs: launch.UserResidual,
		Vendor: targetVendor, NNodes: nnodes, MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
	}
	if profileNode != nil {
		// The bind address the launch will use, so the preflight resolves the
		// same command (planning.PlanLaunch sets it from the node too).
		params.Host = deploy.RuntimeBindHost(profileNode.RuntimeDriver)
		params.RuntimeDriver = profileNode.RuntimeDriver
	}
	pdReq.preflight(&params)
	out.input = deploy.ResolveLaunchInput{
		StartTimeoutSec: req.StartTimeoutSec, DefaultStartTimeoutSec: s.d.Store.DeploymentPolicy().StartTimeoutSec,
		Engine: eng, Params: params, Image: req.Image,
		Entrypoint: selection.Entrypoint, PreStartArgv: selection.PreStartArgv,
		PreStartScript: selection.PreStartScript, RuntimeAssets: selection.Assets,
		ComponentMounts: out.componentMounts, APIContract: selection.APIContract,
		TemplateShmBytes: selection.shmBytes(), TemplateMemlockUnlimited: selection.memlockUnlimited(),
		AllowUnresolved: true,
	}
	return out, nil
}

// run resolves the preflight on profileNode and every distributed or PD rank
// node, each with the image imageFor resolves for it.
func (p launchPreflight) run(req *dto.DeployRequest, selection templateSelection, profileNode *store.Node, distNodes []*store.Node, pdReq *pdRequest, imageFor rankImageResolver) error {
	return preflightCreateRuntimes(p.input, req.ContainerOverrides, selection.Components, p.modelMount, profileNode, distNodes, pdReq, imageFor)
}

// evaluateLaunchPreflight runs the create's launch preflight on an evaluated
// draft: the create materialization of the same request, the same placement
// data-parallel degree, and the same nodes - the draft's node, or its ranks
// for a multi-node draft. selection is the template selection as the create
// sees it at this step, before the model binds its components and API
// contract. The rank images are the ones the evaluation already resolved
// (resp.RankCommands), so this step reads no image catalog of its own.
func (s *Service) evaluateLaunchPreflight(eng deploy.Engine, req *dto.DeployRequest, selection templateSelection, targetVendor string, profileNode *store.Node, rankNodes []*store.Node, resp *dto.EvaluateResponse) error {
	draft := cloneRequest(*req)
	launch, err := s.materializeCreateLaunch(eng.Name(), &draft, selection, DeployTargetCards(profileNode, draft.Placement), s.requestEngineVersion(eng.Name(), &draft, selection, targetVendor, profileNode))
	if err != nil {
		return err
	}
	dp, err := deploy.PlacementDataParallel(eng.Name(), launch.Quick.DP, launch.TemplateResidual, launch.UserResidual)
	if err != nil {
		return err
	}
	var distNodes []*store.Node
	if draft.NNodes > 1 {
		distNodes = rankNodes
	}
	preflight, err := s.prepareLaunchPreflight(eng, &draft, selection, launch, dp, targetVendor, profileNode, nil)
	if err != nil {
		return err
	}
	images := make(map[string]string, len(resp.RankCommands))
	for _, rank := range resp.RankCommands {
		images[rank.NodeUUID] = rank.Image
	}
	imageFor := func(node *store.Node) string {
		if image, ok := images[node.UUID]; ok && image != "" {
			return image
		}
		return s.rankImage(eng.Name(), selection, draft.Image, node).Image
	}
	return preflight.run(&draft, selection, profileNode, distNodes, nil, imageFor)
}
