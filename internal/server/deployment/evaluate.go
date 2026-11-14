package deployment

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/acquisition"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func acquisitionDTO(r acquisition.Result) dto.AcquisitionResult {
	to := func(c acquisition.Candidate) dto.AcquisitionCandidate {
		return dto.AcquisitionCandidate{Key: c.Key, Label: c.Label, DisplayName: c.DisplayName, Reason: c.Reason, Recommended: c.Recommended,
			StorageID: c.StorageID, StorageType: c.StorageType, SizeBytes: c.SizeBytes, CacheBytes: c.CacheBytes,
			OnNode: c.OnNode, Unavailable: c.Unavailable, Default: c.Default}
	}
	out := dto.AcquisitionResult{Image: dto.AcquisitionImage{Image: r.Image.Canonical, Cached: r.Image.Cached}}
	for _, c := range r.Present {
		out.Present = append(out.Present, to(c))
	}
	for _, c := range r.Targets {
		out.Targets = append(out.Targets, to(c))
	}
	for _, c := range r.Providers {
		out.Providers = append(out.Providers, to(c))
	}
	return out
}

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
	resp dto.EvaluateResponse
	err  error
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

func (s *Service) evaluate(ctx context.Context, out *evaluation, evaluateReq dto.EvaluateRequest) {
	// The preview works on the create body itself: one request shape, so an
	// input that reaches the create (sleepable) cannot vanish from the preview.
	req := &evaluateReq.DeployRequest
	// Field-attributable validation failures are collected instead of refusing
	// the request: evaluation is the interactive endpoint, and the form keeps
	// the draft while it shows each error beside its control. Create refuses.
	errs := &evaluateFieldErrors{}
	// An automatic multi-node or PD request is previewed on the nodes the
	// server would pick now; the create picks again.
	autoPlaced, autoErr := s.autoPlaceRequest(req)
	if autoErr != nil {
		errs.add("placement_mode", autoErr)
		out.fieldErrors(&dto.EvaluateResponse{SchemaVersion: evaluateSchemaVersion, Replicas: req.Replicas, AutoPlaced: true}, errs)
		return
	}
	if err := ValidateReplicaShape(req); err != nil {
		errs.add("replicas", err)
	}
	if err := ValidateStartTimeout(req); err != nil {
		errs.add("start_timeout_sec", err)
	}
	if err := ValidateHealthCheckThreshold(req); err != nil {
		errs.add("health_check_threshold_sec", err)
	}
	if _, err := s.preparePDDeployRequest(req, ""); err != nil {
		errs.add(fieldPDPlacement, err)
	}
	if _, err := planning.ParsePlacement(req.Placement); err != nil {
		errs.add("placement", err)
	}
	reqs, reqsErr := requestRequirements(req)
	if reqsErr != nil {
		errs.add("node_requirements", reqsErr)
	} else if err := s.validateRequestNodes(req, reqs); err != nil {
		errs.add("node_requirements", err)
	}
	if _, err := planning.PlacementStrategyOf(req.PlacementStrategy); err != nil {
		errs.add("placement_strategy", err)
	}
	profileNode, _ := s.d.Store.NodeWithGPUs(templateTargetNodeUUID(req.NodeUUID, req.NodeUUIDs))
	targetVendor := accelerator.NormalizeVendor(evaluateReq.TargetVendor)
	if strings.TrimSpace(evaluateReq.TargetVendor) != "" && targetVendor == "" {
		errs.add("target_vendor", productError(messages.ErrorTargetVendorInvalid, "target_vendor is not supported", map[string]any{"vendor": evaluateReq.TargetVendor}))
	}
	if targetVendor == "" {
		targetVendor = planning.RequirementVendor(reqs)
	}
	hardware := nodeTemplateHardware(profileNode)
	// An explicitly named spec is a complete execution contract and resolves
	// without hardware facts; injecting the vendor hint here would turn the
	// cc-less GPU into a chipmatch rejection and make automatic placement with
	// a selected template impossible. The vendor hint serves the unnamed-spec
	// matching path only.
	if hardware.GPU.Vendor == "" && req.TemplateSpecName == "" {
		hardware.GPU.Vendor = targetVendor
	}
	selection, err := s.applyTemplateDefaults(req, hardware)
	if err != nil {
		out.fail(err)
		return
	}
	if err := checkPDSpec(selection, placementPD(req)); err != nil {
		errs.add(fieldPDPlacement, err)
	}
	preserveEngineModelName(req)
	// The mounts the request states are judged against the assembled launch's
	// own rules here, so the form learns about a duplicate or nested target at
	// the same moment it learns everything else about the draft.
	if err := validateMountRowRequest(req, mountRowTargets(selection.Components, requestModelMount(req))); err != nil {
		errs.add(FieldContainerMounts, err)
	} else if err := s.ValidateMountModels(req); err != nil {
		errs.add(FieldContainerMounts, err)
	}
	// Each replica is placed as its own unit; this tree admits no joint
	// multi-replica group, so the scope is always per-replica. The response
	// exists from here on so every field-error exit carries the template facts
	// it already resolved.
	resp := dto.EvaluateResponse{SchemaVersion: evaluateSchemaVersion, OK: true, Replicas: req.Replicas, AutoPlaced: autoPlaced, PlanScope: "per_replica", TemplateMatch: selection.Match, MatchedBy: selection.MatchedBy, TemplateDeviated: selection.Deviated}
	if selection.Spec != nil {
		resp.TemplateSpecSource = selection.Spec.Source
		resp.TemplateSpecName = selection.Spec.Name
	}
	resp.Warnings = append(resp.Warnings, selection.Warnings...)
	// The eligibility answer rides along even when an early field error stops
	// the preview: the node dropdown renders reasons for exactly those
	// drafts. Requirements that cannot be read leave every node ineligible
	// rather than every node eligible.
	pool, poolErr := requestPool(req)
	placement, _ := planning.ParsePlacement(req.Placement)
	eligibleLoaded := false
	loadEligible := func(requests func(*store.Node) map[int]planning.TargetCardRequest) {
		if eligibleLoaded {
			return
		}
		resp.EligibleNodes, resp.PoolPartitionCapability = s.evaluateEligibleNodes(selection, pool, poolErr, IsAutomaticPlacement(req) && req.NNodes <= 1, requests)
		eligibleLoaded = true
	}
	writeFieldErrors := func() { loadEligible(nil); out.fieldErrors(&resp, errs) }

	// A placement read failure was collected above; the zero placement keeps
	// the rest of the answer renderable.
	eng, err := deploy.SelectEngine(req.Engine)
	if err != nil {
		errs.add("engine", productError(messages.ErrorEngineInvalid, "unknown engine", map[string]any{"engine": req.Engine}))
		writeFieldErrors()
		return
	}
	// Whether the form may offer a yield policy other than never: the create's
	// own check, answered even when a field error stops the preview below.
	yieldEligible := dto.EligibilityOf(planning.DraftYieldPolicyRefusal(req.Sleepable, eng.Name(), req.NNodes, placementPD(req) != nil))
	resp.YieldPolicyEligible = &yieldEligible
	if !errs.empty() {
		// A draft with an early field error is what the form must fix before a
		// preview means anything; continuing would only re-report the same
		// rules from the assembled launch.
		writeFieldErrors()
		return
	}
	profile, err := deploy.ProfileForTarget(eng.Name(), targetVendor)
	if err != nil {
		out.fail(err)
		return
	}
	resp.Image = req.Image
	if resp.Image == "" {
		resp.Image = profile.DefaultImage
	}
	// The selection as the create's launch preflight sees it: before the model
	// binds the components and its API contract (evaluateLaunchPreflight).
	preflightSelection := selection
	preflightSelection.Components = append([]deploy.ComponentBinding(nil), selection.Components...)
	model, err := s.resolveDeploymentModelFromTemplate(ctx, req.ModelID, &selection, req.TemplateID)
	if err != nil {
		errs.add("model_id", err)
		writeFieldErrors()
		return
	}
	// A quick registration may not have metadata yet. Keep evaluation and
	// scheduling usable, but tell clients that the VRAM estimate is incomplete.
	if model.WeightBytes <= 0 && (selection.Spec == nil || (selection.Spec.WeightBytes <= 0 && selection.Spec.RuntimeWeightBytes <= 0)) {
		resp.Skipped = true
		resp.SkipReason = "model weight size is unknown"
	}
	servedName := strings.TrimSpace(req.ServedName)
	if strings.Contains(strings.ToLower(strings.TrimSpace(req.Image)), "@sha256:") {
		resp.Warnings = append(resp.Warnings, messages.NewWarning(messages.WarningImageDigestUnverified, map[string]any{"image": req.Image}))
	}
	// One materialization decides this deployment's launch configuration. The
	// preview below, the estimate, and a create submitted from this form all
	// read the same object.
	launch, err := materializeDeployRequest(eng.Name(), req, selection, DeployTargetCards(profileNode, req.Placement), s.requestEngineVersion(eng.Name(), req, selection, targetVendor, profileNode))
	if err != nil {
		// ProductErrors that name a key land on that key's control; the
		// extra-args parse error is the one that starts on the text box.
		errs.add(fieldExtraArgs, err)
		writeFieldErrors()
		return
	}
	quick := launch.Quick
	userExtraArgs := launch.UserResidual
	resp.EffectiveParams = launchConfigMap(quick)
	// FormValues is the same configuration read as a display instruction: the
	// effective values plus the argument text after the moved flags left it.
	// The form sets its fields from this and derives nothing itself.
	formValues := launchConfigMap(quick)
	formValues["extra_args"] = deploy.FormatArgsText(launch.UserResidual)
	resp.FormValues = formValues
	resp.Defaults = &dto.EvaluateDefaults{ModelMount: deploy.DefaultModelMount, MasterPort: deploy.DefaultMasterPort, Engine: deploy.DefaultEngine}
	resp.EngineDecidesParamKeys = launch.EngineDecidesParamKeys
	resp.Moved = launch.Moved
	resp.TemplateParams = launch.TemplateParams
	resp.TemplateResidualArgs = launch.TemplateResidual
	if requestOverrides, overrideErr := RequestContainerOverrides(req.ContainerOverrides); overrideErr == nil && placement.PD == nil {
		mount, components := previewComponentMounts(nil, selection.Components, nil)
		draft := evaluationLaunchRequest(evaluatePlanEnv{request: req, excludeDeployment: evaluateReq.DeploymentID, response: &resp, engine: eng, model: model, selection: selection, placement: placement, quick: quick, userExtraArgs: userExtraArgs, servedName: servedName, requestOverrides: requestOverrides, modelMount: mount, componentMounts: components, templateResidual: launch.TemplateResidual}, nil)
		// Choosing another node also chooses its cards; explicit indexes are
		// validated by the displayed plan for the selected node, not this dropdown.
		draft.Placement.GPUIndexes = nil
		if requests, requestErr := planning.TargetRequestsForLaunch(draft); requestErr == nil {
			loadEligible(requests)
		}
	}
	loadEligible(nil)
	if placement.PD != nil {
		requestOverrides, _ := RequestContainerOverrides(req.ContainerOverrides)
		s.evaluatePD(evaluatePDEnv{ctx: ctx, out: out, request: req, excludeDeployment: evaluateReq.DeploymentID, response: &resp, engine: eng, model: model,
			selection: selection, launch: launch, requestOverrides: requestOverrides, servedName: servedName, errs: errs,
			targetVendor: targetVendor, profileNode: profileNode})
		return
	}
	if req.NNodes <= 0 {
		req.NNodes = 1
	}
	// A multi-node draft gets the structural preflight inline, computed before
	// the distributed validation can stop the response, so the summary rides
	// along with the field errors it explains. Deep network probing stays with
	// the preflight RPC.
	if req.NNodes > 1 {
		resp.Preflight = s.evaluateInlinePreflight(req)
		if !resp.Preflight.OK {
			resp.OK = false
		}
	}
	var rankNodes []*store.Node
	// A parallelism the nodes cannot hold is reported on its field, but the
	// image and weight acquisition below still resolves: they depend only on
	// the nodes and the model, and the form shows them above the parallelism.
	parallelInvalid := false
	if req.NNodes > 1 {
		normalized := dto.DeployRequest{
			NodeUUID: req.NodeUUID, NodeUUIDs: req.NodeUUIDs, NNodes: req.NNodes,
			NodeRank: req.NodeRank, MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
			Placement: req.Placement,
		}
		dp, err := deploy.PlacementDataParallel(eng.Name(), quick.DP, launch.TemplateResidual, launch.UserResidual)
		if err != nil {
			errs.add("data_parallel_size", err)
			writeFieldErrors()
			return
		}
		nodes, err := s.PrepareDistributedDeployRequest(&normalized, dp)
		if err != nil {
			field := distributedErrorField(err)
			errs.add(field, err)
			if !isParallelismField(field) {
				writeFieldErrors()
				return
			}
			parallelInvalid = true
			// The form keeps what the deployer typed: form_values echoes the
			// stated degrees, since a key missing from it clears the field.
			if placement.TP > 0 {
				resp.FormValues["tp"] = placement.TP
			}
			if placement.PP > 0 {
				resp.FormValues["pp"] = placement.PP
			}
			// Rank zero is the node the acquisition facts describe.
			if len(req.NodeUUIDs) > 0 {
				req.NodeUUID = req.NodeUUIDs[0]
			}
		} else {
			req.NodeUUID = normalized.NodeUUID
			req.NodeRank = normalized.NodeRank
			req.MasterAddr = normalized.MasterAddr
			req.MasterPort = normalized.MasterPort
			// The parallelism that takes effect, defaulted from the nodes
			// when the draft stated none, is what the form shows and what
			// the plan below runs with.
			req.Placement = normalized.Placement
			placement, _ = planning.ParsePlacement(req.Placement)
			resp.FormValues["tp"], resp.FormValues["pp"] = placement.TP, placement.PP
		}
		for _, n := range nodes {
			withGPUs, err := s.d.Store.NodeWithGPUs(n.UUID)
			if err != nil {
				errs.add("node_uuids", productError(messages.ErrorDeploymentNodeInvalid, fmt.Sprintf("node %s: %v", n.UUID, err), map[string]any{"node": n.UUID, "detail": err.Error()}))
				writeFieldErrors()
				return
			}
			rankNodes = append(rankNodes, withGPUs)
		}
	}

	// Automatic placement: every eligible node is planned, the candidates list
	// records each answer, and the best feasible node becomes the draft's node
	// for the rest of this evaluation. A create repeating the draft re-validates
	// the choice and answers AUTOMATIC_PLACEMENT_STALE when the world moved.
	if IsAutomaticPlacement(req) && req.NNodes <= 1 {
		requestOverrides, overrideErr := RequestContainerOverrides(req.ContainerOverrides)
		if overrideErr == nil {
			automaticSelection := selection
			automaticSelection.ExtraArgs = launch.TemplateResidual
			selected, candidates := s.evaluateAutomaticPlacement(ctx, req, eng, model, automaticSelection, placement, quick, userExtraArgs, servedName, resp.Image, requestOverrides, resp.EligibleNodes, evaluateReq.DeploymentID)
			resp.SelectedNode = selected
			resp.Candidates = candidates
			if selected != nil {
				req.NodeUUID = selected.UUID
				// The facts the acquisition panel narrows to "the node this
				// draft lands on" follow the automatic pick too: profileNode
				// was resolved from a request that names no node, and leaving
				// it nil answers node_unselected beside a selected_node that
				// already names the node.
				if withGPUs, nodeErr := s.d.Store.NodeWithGPUs(selected.UUID); nodeErr == nil {
					profileNode = withGPUs
				}
				if resp.OK && !resp.Skipped {
					resp.OK = true
				}
			}
		}
	}
	// Computed after automatic placement so the weight networks, peers and
	// local-path facts are the selected node's, not the unnamed request's.
	weightFacts, weightFactsErr := s.evaluateWeightFacts(model, selection, profileNode)
	if weightFactsErr != nil {
		resp.Diagnostics = diagnosticsFromError(weightFactsErr)
		errs.add("model_id", weightFactsErr)
		writeFieldErrors()
		return
	}
	// The template's weight sources with the suggested and registered facts,
	// so the form's chooser renders a server answer too.
	resp.WeightSources = s.evaluateWeightSourceOptions(selection)

	node, nerr := s.d.Store.NodeWithGPUs(req.NodeUUID)
	nodeFound := nerr == nil
	if req.NNodes == 1 && nerr == nil {
		rankNodes = []*store.Node{node}
	}
	if nerr == nil {
		req.PartitionBackend, err = ResolvePartitionBackend(node, req.VRAMLimitGiB, req.PartitionBackend)
		if err != nil {
			errs.add("partition_backend", err)
			writeFieldErrors()
			return
		}
	}
	image := s.evaluateImageFacts(evaluateImageEnv{
		ctx: ctx, request: req, response: &resp, engine: eng, selection: selection,
		targetVendor: targetVendor, profileNode: profileNode, node: node, nodeFound: nodeFound, weightFacts: weightFacts,
	})
	if resp.Acquisition != nil {
		if pinned := pinnedNodeUUIDs(req); len(pinned) > 0 {
			known := map[string]*bool{}
			if profileNode != nil {
				known[profileNode.UUID] = resp.Acquisition.Image.Cached
			}
			resp.Acquisition.Nodes = s.evaluateNodeAcquisition(ctx, pinned, resp.Image, templateImageSources(selection, resp.Image), model, store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements), SaveTo: s.previewSaveToStorageID(req.ModelSource, model, profileNode)}, known)
		}
	}
	resp.Diagnostics, err = s.inspectDeploymentModel(ctx, model, store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements), SaveTo: s.previewSaveToStorageID(req.ModelSource, model, profileNode)}, rankNodes, false)
	if err != nil {
		errs.add("model_id", err)
		writeFieldErrors()
		return
	}
	if resp.Acquisition != nil {
		resp.Acquisition.Components = s.evaluateComponentAcquisition(req.ModelSource, model, selection.Components, store.StoragePins(req.NodeRequirements), rankNodes)
	}
	if parallelInvalid {
		writeFieldErrors()
		return
	}
	arch, archID, vendor := image.arch, image.archID, image.vendor
	imageCached := image.imageCached
	// nodeImageCached is the same fact narrowed to the node the estimate lands
	// on; evaluatePlan fills it in for the per-rank assembly.
	var nodeImageCached *bool

	// Estimate + schedule (shared with the controller).
	gpuCount := len(placement.GPUIndexes)
	resolver := templatePlacementResolver(selection.Placement)
	templateArgs := append([]string(nil), launch.TemplateResidual...)
	templateEnv := cloneStringMap(selection.Env)
	templateEntrypoint := append([]string(nil), selection.Entrypoint...)
	// The launch inputs a request owns: the container override layer it states,
	// and the template layer the write boundary would keep (the residual, not
	// the spec's raw argument list).
	requestOverrides, overrideErr := RequestContainerOverrides(req.ContainerOverrides)
	if overrideErr != nil {
		// The override layer is a section, not one control: the page-level
		// line carries its decode failures.
		errs.add("", overrideErr)
		writeFieldErrors()
		return
	}
	previewModelMount, previewMounts := previewComponentMounts(s.d.Store, selection.Components, node)
	plan, ok := s.evaluatePlan(evaluatePlanEnv{
		ctx: ctx, out: out, request: req, excludeDeployment: evaluateReq.DeploymentID, response: &resp, engine: eng, model: model,
		node: node, nodeFound: nodeFound, profileNode: profileNode, imageCached: imageCached,
		nodeImageCached: nodeImageCached, selection: selection, placement: placement, quick: quick,
		userExtraArgs: userExtraArgs, servedName: servedName, requestOverrides: requestOverrides,
		modelMount: previewModelMount, componentMounts: previewMounts, gpuCount: gpuCount,
		resolver: resolver, templateResidual: launch.TemplateResidual, errs: errs,
	})
	if !ok {
		return
	}
	nodeImageCached, quick = plan.nodeImageCached, plan.quick
	tp := plan.tp
	parallelismSummary, parallelismErr := evaluateParallelism(placement, req.NNodes, tp, plan.placementResolved, quick, launch.TemplateResidual, launch.UserResidual, eng.Name())
	if parallelismErr != nil {
		errs.add("data_parallel_size", parallelismErr)
		writeFieldErrors()
		return
	}
	resp.Parallelism = parallelismSummary
	placementVariables, allowUnresolvedPlacement := plan.placementVariables, plan.allowUnresolved
	dynamicTPPreview := plan.dynamicTPPreview

	rankSelection := snapshotTemplateSelection(selection, resp.Image)
	// The template layer a launch resolves against is the residual the write
	// boundary kept, not the spec's own argument list: dictionary flags are
	// edited through the form and the engine dictionary, and a spec token that
	// happens to name one is what the stored row no longer carries either.
	rankSelection.ExtraArgs = launch.TemplateResidual
	if !s.evaluateRanks(evaluateRankEnv{
		ctx: ctx, out: out, request: req, excludeDeployment: evaluateReq.DeploymentID, response: &resp, engine: eng, model: model, launch: launch,
		selection: selection, rankSelection: rankSelection, rankNodes: rankNodes, placement: placement,
		quick: quick, userExtraArgs: userExtraArgs, dynamicTPPreview: dynamicTPPreview,
		nodeImageCached: nodeImageCached, requestOverrides: requestOverrides,
		arch: arch, archID: archID, vendor: vendor, servedName: servedName,
		placementVariables: placementVariables, allowUnresolved: allowUnresolvedPlacement,
		modelWeightBytes: plan.modelWeightBytes, templateArgs: templateArgs, templateEnv: templateEnv,
		templateEntrypoint: templateEntrypoint, tp: tp, errs: errs,
	}) {
		return
	}
	// Checked once the ranks are previewed, so the form shows the refusal
	// beside the launches and the draft that would satisfy it.
	if err := checkManualCommandRanks(req); err != nil {
		errs.add(fieldContainerCmd, err)
	}
	// The create resolves its launch on the per-node card count the request
	// states, where the plan above used the cards the scheduler assigned; a
	// memory-limited draft can get fewer cards than it states. The same
	// preflight here makes what the create would refuse a blocking error.
	if errs.empty() {
		if err := s.evaluateLaunchPreflight(eng, req, preflightSelection, targetVendor, profileNode, rankNodes, &resp); err != nil {
			errs.add("", err)
			writeFieldErrors()
			return
		}
	}
	if !errs.empty() {
		resp.OK = false
		resp.Errors = errs.items
	}
	resp.Warnings = uniqueWarnings(resp.Warnings)
	out.answer(resp)
}

// distributedErrorField maps the distributed request errors onto the form
// fields that state them.
func distributedErrorField(err error) string {
	var productErr *messages.ProductError
	if errors.As(err, &productErr) {
		switch productErr.Code() {
		case messages.ErrorMasterPortInvalid:
			return "master_port"
		case messages.ErrorDistributedTPRequired, messages.ErrorDistributedGPUCount:
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
