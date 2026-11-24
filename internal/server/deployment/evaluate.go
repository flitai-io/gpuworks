package deployment

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/acquisition"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// persistCatalogWeight records the catalog spec's weight total on a legacy
// model row that has none. The estimator used this value for the preview it
// just computed; writing it here - once, at the boundary that knows both the
// row and the spec - is what lets planning stay a reader.
func (s *Service) persistCatalogWeight(model *store.Model, weightBytes int64) {
	if model == nil || model.ID == 0 || weightBytes <= 0 || model.WeightBytes > 0 || s.d.Store == nil {
		return
	}
	if err := s.d.Store.SetModelWeightBytes(model.ID, weightBytes); err != nil && s.d.Log != nil {
		s.d.Log.Warn("persist catalog weight bytes", "model", model.ID, "error", err)
	}
}

// evaluateSchemaVersion is the evaluate response contract version. It is
// bumped when a fixture-rendered shape changes meaning; within this plan it
// stays 1.
const evaluateSchemaVersion = 1

// Evaluate previews a create request for the deploy form: the launch it
// assembles, where it would run and what it would fetch. A rule the form can
// show beside a field is a field error in the preview (OK false); an error is
// returned only when nothing about the draft can be previewed.
func (s *Service) Evaluate(ctx context.Context, evaluateReq dto.EvaluateRequest) (dto.EvaluateResponse, error) {
	out := &evaluation{}
	s.evaluate(ctx, out, evaluateReq)
	if out.err != nil {
		return dto.EvaluateResponse{}, out.err
	}
	return out.resp, nil
}

// evaluation is what an evaluation answers: the preview, or the error that
// stopped it before a preview meant anything. Each step that answers returns
// right after it.
type evaluation struct {
	resp         dto.EvaluateResponse
	err          error
	admissionErr error
}

func (e *evaluation) answer(resp dto.EvaluateResponse) { e.resp = resp }

func (e *evaluation) fail(err error) { e.err = invalid(err) }

// fieldErrors answers resp with the field errors collected so far.
func (e *evaluation) fieldErrors(resp *dto.EvaluateResponse, errs *evaluateFieldErrors) {
	resp.OK = false
	resp.Errors = errs.items
	resp.Warnings = uniqueWarnings(resp.Warnings)
	e.answer(*resp)
}

// evaluate runs the preview's steps in order. A step that has answered (the
// field errors so far, or the error that stops the preview) returns false and
// ends the evaluation.
func (s *Service) evaluate(ctx context.Context, out *evaluation, evaluateReq dto.EvaluateRequest) {
	// The preview works on the create body itself: one request shape, so an
	// input that reaches the create (sleepable) cannot vanish from the preview.
	// Field-attributable validation failures are collected instead of refusing
	// the request: evaluation is the interactive endpoint, and the form keeps
	// the draft while it shows each error beside its control. Create refuses.
	r := &evaluateState{s: s, ctx: ctx, out: out, req: &evaluateReq.DeployRequest, errs: &evaluateFieldErrors{},
		excludeDeployment: evaluateReq.DeploymentID, statedVendor: evaluateReq.TargetVendor}
	for _, step := range []func() bool{
		r.resolveTemplate,
		r.resolveEngineAndModel,
		r.materializeLaunch,
		r.listEligibleNodes,
		r.prepareDistributed,
		r.placeAutomatically,
		r.resolveAcquisition,
		r.planRanks,
		r.finish,
	} {
		if !step() {
			return
		}
	}
}

// evaluateState is the evaluation state the steps share: what the steps so
// far have settled about the draft and a later step reads. The request is
// rewritten as the draft is normalized (its node, ranks, placement and
// partition backend), and the response is filled in as the steps go.
type evaluateState struct {
	s    *Service
	ctx  context.Context
	out  *evaluation
	req  *dto.DeployRequest
	errs *evaluateFieldErrors
	resp dto.EvaluateResponse
	// excludeDeployment is the deployment an edit previews, whose own
	// holdings do not count against it; statedVendor is the target vendor as
	// the request wrote it.
	excludeDeployment uint
	statedVendor      string

	reqs         map[string][]string
	profileNode  *store.Node
	targetVendor string
	selection    templateSelection
	// preflightSelection is the selection as the create's launch preflight
	// sees it: before the model binds the components and its API contract
	// (evaluateLaunchPreflight).
	preflightSelection templateSelection
	pool               map[string][]string
	poolErr            error
	placement          planning.Placement
	eligibleLoaded     bool
	fleet              []*store.Node // the nodes the eligibility was read from

	eng        deploy.Engine
	model      *store.Model
	servedName string
	base       *editLaunchBase
	launch     deploy.Materialized
	quick      deploy.LaunchConfig

	rankNodes []*store.Node
	// parallelInvalid: a parallelism the nodes cannot hold is reported on
	// its field, but the image and weight acquisition still resolves: they
	// depend only on the nodes and the model, and the form shows them above
	// the parallelism.
	parallelInvalid bool
	// poolFits: the node list found a node the draft fits.
	poolFits    bool
	weightFacts acquisition.WeightFacts
	node        *store.Node
	nodeFound   bool
	image       evaluateImageResult

	// The launch inputs a request owns, as the plan read them: the container
	// override layer it states and the mounts of the preview.
	requestOverrides deploy.ContainerOverrides
	modelMount       string
	componentMounts  []proto.MountSpec
}

// loadEligible answers the node list once; the first call decides it. The
// eligibility answer rides along even when an early field error stops the
// preview: the node dropdown renders reasons for exactly those drafts.
// Requirements that cannot be read leave every node ineligible rather than
// every node eligible.
func (r *evaluateState) loadEligible(requests func(*store.Node) map[int]planning.TargetCardRequest) {
	if r.eligibleLoaded {
		return
	}
	r.resp.EligibleNodes, r.resp.PoolPartitionCapability, r.fleet = r.s.evaluateEligibleNodes(r.selection, r.pool, r.poolErr, IsAutomaticPlacement(r.req) && r.req.NNodes <= 1, requests)
	r.eligibleLoaded = true
}

// writeFieldErrors answers with the field errors collected so far.
func (r *evaluateState) writeFieldErrors() {
	r.loadEligible(nil)
	// A draft with a field error still answers whether it can be checked,
	// so the switch does not flicker while the deployer fixes the field.
	if r.resp.HealthCheckable == nil {
		r.s.noteHealthCheckable(r.ctx, &r.resp, r.req, r.selection)
	}
	r.out.fieldErrors(&r.resp, r.errs)
}

// finish runs the checks that need the previewed ranks and answers.
func (r *evaluateState) finish() bool {
	// Checked once the ranks are previewed, so the form shows the refusal
	// beside the launches and the draft that would satisfy it.
	r.errs.add(fieldContainerCmd, checkManualCommandRanks(r.req))
	// A headless rank binds no API port; which ranks are headless is the
	// rule the ranks were previewed with (planning.PreviewRankTopology), of
	// which only the role is read here.
	for rank, node := range r.rankNodes {
		if r.req.NNodes > 1 && planning.PreviewRankTopology(r.req.NNodes, rank, netplan.Resolved{}).Headless {
			continue
		}
		r.resp.Warnings = append(r.resp.Warnings, r.s.manualCommandPortTaken(r.requestOverrides, node, r.excludeDeployment)...)
	}
	// The create resolves its launch on the per-node card count the request
	// states, where the plan above used the cards the scheduler assigned; a
	// memory-limited draft can get fewer cards than it states. The same
	// preflight here makes what the create would refuse a blocking error.
	if r.errs.empty() {
		if r.errs.add("", r.s.evaluateLaunchPreflight(r.eng, r.req, r.preflightSelection, r.targetVendor, r.profileNode, r.rankNodes, &r.resp, r.base)) {
			r.writeFieldErrors()
			return false
		}
	}
	// An automatic draft with no node is refused when no node of its pool
	// could ever run it. A node the list above found to fit settles that.
	if r.errs.empty() && r.req.NNodes <= 1 && r.placement.PD == nil && IsAutomaticPlacement(r.req) && r.resp.SelectedNode == nil && !r.poolFits {
		candidateReq := cloneRequest(*r.req)
		candidateReq.Image = r.resp.Image
		admissionSelection := r.selection
		admissionSelection.ExtraArgs = r.launch.TemplateResidual
		if err := r.s.checkPoolCanEverFit(r.ctx, &candidateReq, r.reqs, r.eng, r.model, admissionSelection, r.placement, r.quick, r.launch.UserResidual, r.servedName, r.modelMount, r.componentMounts, r.requestOverrides); err != nil {
			r.out.admissionErr = err
			r.errs.add("", err)
		}
	}
	if !r.errs.empty() {
		r.out.fieldErrors(&r.resp, r.errs)
		return false
	}
	r.resp.Warnings = uniqueWarnings(r.resp.Warnings)
	r.out.answer(r.resp)
	return true
}

// distributedErrorField maps the distributed request errors onto the form
// fields that state them; placement is the degrees the request stated.
func distributedErrorField(err error, placement planning.Placement) string {
	var productErr *messages.ProductError
	if errors.As(err, &productErr) {
		switch productErr.Code() {
		case messages.ErrorMasterPortInvalid:
			return "master_port"
		case messages.ErrorDistributedTPRequired, messages.ErrorDistributedGPUCount:
			return "placement.tp"
		case messages.ErrorDistributedParallelismUnavailable:
			// Beside the degree left blank: filling it fixes the layout.
			if placement.TP > 0 {
				return "placement.pp"
			}
			return "placement.tp"
		case messages.ErrorDistributedParallelIndivisible:
			return "placement.pp"
		}
	}
	return "node_uuids"
}

// isParallelismField reports whether an evaluate error is about the
// parallelism rather than the node list; those leave acquisition resolvable.
func isParallelismField(field string) bool {
	return field == "placement.tp" || field == "placement.pp"
}

// evaluateParallelism is the split the draft launches with: the stated
// degrees, or the TP a single-node schedule settled on. Nil while neither
// is known.
func evaluateParallelism(placement planning.Placement, nnodes, scheduledTP int, scheduled bool, quick deploy.LaunchConfig, templateArgs, userArgs []string, engine string) (*dto.EvaluateParallelism, error) {
	spec := placement.Parallelism(nnodes)
	dp, err := deploy.PlacementDataParallel(engine, quick.DP, templateArgs, userArgs)
	if err != nil {
		return nil, err
	}
	spec.DP = dp
	if !spec.Stated() {
		if !scheduled {
			return nil, nil
		}
		spec.TP = scheduledTP
	}
	perNode, err := spec.GPUsPerNode()
	if err != nil {
		return nil, err
	}
	return &dto.EvaluateParallelism{TP: spec.TP, PP: spec.PP, Nodes: spec.NNodes, GPUsPerNode: perNode}, nil
}

// evaluationPlacement is the draft's placement as the evaluation plans it
// (requestPlacement), a malformed one read as empty.
func evaluationPlacement(req *dto.DeployRequest) planning.Placement {
	placement, _ := requestPlacement(req)
	return placement
}
