package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// editDeployment answers PUT /deployments/{id}: the deploy form's draft
// (GET /deployments/{id}/draft) sent back with an edit. What the deployment
// is cannot change (dto.DeploymentDraft.Locked); everything else is judged
// by the rules a create is judged by and written in one change with the way
// it reaches the instances. ?dry_run=1 answers the plan and writes nothing.
func (a *apiHandlers) editDeployment(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var body dto.DeploymentEditRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSONBodyError(w, err)
		return
	}
	if dep.State == store.DeployDeleting {
		writeProductError(w, http.StatusConflict, ErrorDeploymentDeleting, "the deployment is being deleted", nil)
		return
	}
	reqs, err := a.d.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	draft, err := a.deploymentDraft(dep, reqs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if strings.TrimSpace(body.BaseRevision) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "base_revision is required: send back the revision of the draft the edit was made on", map[string]any{"field": "base_revision", "detail": "required"})
		return
	}
	if body.BaseRevision != draft.Revision {
		writeProductError(w, http.StatusConflict, ErrorDeploymentChanged, "the deployment changed since its draft was read; read the draft again", map[string]any{"revision": draft.Revision})
		return
	}
	if field := lockedChange(draft, &body.Deployment); field != "" {
		writeProductError(w, http.StatusBadRequest, ErrorDeploymentFieldNotEditable, "the field cannot change on an existing deployment; clone it to change "+field, map[string]any{"field": field})
		return
	}
	shape, err := deploymentShape(dep)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	next, nextReqs, status, err := a.editedDeployment(r.Context(), dep, reqs, shape, &body.Deployment)
	if err != nil {
		writeErr(w, status, err)
		return
	}
	changes := deploymentChanges(a.d.Store, dep, reqs, next, nextReqs)
	plan, err := a.deploymentEditPlan(dep, changes, draft.Revision, partitionReshaped(dep, next))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if dryRun := r.URL.Query().Get("dry_run"); dryRun == "1" || dryRun == "true" {
		writeJSON(w, http.StatusOK, plan)
		return
	}
	if len(changes) == 0 {
		writeJSON(w, http.StatusOK, dto.DeploymentEditResult{Plan: plan, Deployment: a.deploymentDTO(dep, true)})
		return
	}
	edit, refusal := editApply(plan, body)
	if refusal != nil {
		writeErr(w, http.StatusBadRequest, refusal)
		return
	}
	if edit.Apply != store.EditApplyNone {
		if err := a.checkRestartStorage(dep); err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
	}
	edit.Next = next
	if !sameRequirementSets(reqs, nextReqs) {
		edit.Requirements = nextReqs
	}
	if err := a.d.Store.EditDeployment(dep, reqs, edit); err != nil {
		switch {
		case errors.Is(err, store.ErrStateChanged):
			writeProductError(w, http.StatusConflict, ErrorDeploymentChanged, "the deployment changed while the edit was written; read the draft again", nil)
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	fields := make([]string, 0, len(changes))
	for _, change := range changes {
		fields = append(fields, change.Field)
	}
	a.d.Store.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, store.EventDeploymentEdited, map[string]any{
		"fields": strings.Join(fields, ", "), "count": len(fields), "apply": string(edit.Apply),
	})
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if updatedReqs, err := a.d.Store.DeploymentRequirements(dep.ID); err == nil {
		if fresh, err := a.deploymentDraft(updated, updatedReqs); err == nil {
			plan.Revision = fresh.Revision
		}
	}
	writeJSON(w, http.StatusOK, dto.DeploymentEditResult{Plan: plan, Deployment: a.deploymentDTO(updated, true)})
}

// lockedChange names the first locked field the request changes, empty when
// it changes none. A locked field the request leaves unstated (empty, zero)
// keeps the deployment's value, so a client may send only what it edits of
// them; a stated one must match the draft.
func lockedChange(draft dto.DeploymentDraft, req *dto.DeployRequest) string {
	was := draft.Deployment
	differs := map[string]bool{
		"model_id":             req.ModelID != 0 && req.ModelID != was.ModelID,
		"template_id":          req.TemplateID != 0 && req.TemplateID != was.TemplateID,
		"template_spec_source": req.TemplateSpecSource != "" && req.TemplateSpecSource != was.TemplateSpecSource,
		"template_spec_name":   req.TemplateSpecName != "" && req.TemplateSpecName != was.TemplateSpecName,
		"scenario":             req.Scenario != "" && req.Scenario != was.Scenario,
		"engine":               req.Engine != "" && engineName(req.Engine) != engineName(was.Engine),
		"target_vendor":        req.TargetVendor != "" && accelerator.NormalizeVendor(req.TargetVendor) != was.TargetVendor,
		"name":                 strings.TrimSpace(req.Name) != "" && strings.TrimSpace(req.Name) != was.Name,
		"served_name":          strings.TrimSpace(req.ServedName) != "" && strings.TrimSpace(req.ServedName) != was.ServedName,
		"model_source":         modelSourceChanged(req.ModelSource, was.ModelSource),
		"nnodes":               req.NNodes != 0 && req.NNodes != was.NNodes,
		"node_uuid":            req.NodeUUID != "" && req.NodeUUID != was.NodeUUID,
		"node_uuids":           len(req.NodeUUIDs) > 0 && !slices.Equal(req.NodeUUIDs, was.NodeUUIDs),
		"node_requirements":    req.NodeRequirements != nil && !sameRequirementSets(req.NodeRequirements, was.NodeRequirements),
		"placement_mode":       req.PlacementMode != "" && req.PlacementMode != was.PlacementMode,
		"placement_strategy":   req.PlacementStrategy != "" && req.PlacementStrategy != was.PlacementStrategy,
		"master_addr":          req.MasterAddr != "" && req.MasterAddr != was.MasterAddr,
		"master_port":          req.MasterPort != 0 && req.MasterPort != was.MasterPort,
		"dist_group":           req.DistGroup != "" && req.DistGroup != was.DistGroup,
		"dist_role":            req.DistRole != "" && req.DistRole != was.DistRole,
		"node_rank":            req.NodeRank != 0 && req.NodeRank != was.NodeRank,
		"replicas":             req.Replicas != 0 && req.Replicas != was.Replicas,
		"vram_limit_gib":       req.VRAMLimitGiB != 0 && gibBytes(req.VRAMLimitGiB) != gibBytes(was.VRAMLimitGiB),
		"partition_backend":    req.PartitionBackend != "" && req.PartitionBackend != was.PartitionBackend,
		"placement":            req.Placement != nil && !sameJSON(req.Placement, was.Placement),
	}
	for _, field := range draft.Locked {
		if differs[field] {
			return field
		}
	}
	return ""
}

func engineName(name string) string {
	if engine, err := deploy.SelectEngine(name); err == nil {
		return engine.Name()
	}
	return name
}

func modelSourceChanged(req, was *dto.ModelSource) bool {
	if req == nil {
		return false
	}
	var storage uint
	var provider string
	if was != nil {
		storage, provider = was.StorageID, was.Provider
	}
	return req.StorageID != storage || (req.Provider != "" && req.Provider != provider)
}

// editedDeployment is the deployment as the edit leaves it, and the node
// requirements it then has, judged by the rules a create is judged by. The
// launch configuration starts from the stored one (deploy.MaterializeInput.
// Base) and the template layer from the deployment's own snapshot, so what
// the edit does not touch stays exactly as the deployment runs it.
func (a *apiHandlers) editedDeployment(ctx context.Context, dep *store.Deployment, reqs map[string][]string, shape string, req *dto.DeployRequest) (*store.Deployment, map[string][]string, int, error) {
	bad := func(err error) (*store.Deployment, map[string][]string, int, error) {
		return nil, nil, http.StatusBadRequest, err
	}
	internal := func(err error) (*store.Deployment, map[string][]string, int, error) {
		return nil, nil, http.StatusInternalServerError, err
	}
	next := *dep
	// The deployment's shape and vendor are not the edit's: an unstated
	// field reads as the deployment's own.
	req.NNodes, req.TargetVendor = max(dep.NNodes, 1), planning.RequirementVendor(reqs)
	if shape != deploymentShapeSingle {
		req.Replicas = max(dep.Replicas, 1)
	}
	if err := deployment.ValidateReplicaShape(req); err != nil {
		return bad(err)
	}
	if err := deployment.ValidateStartTimeout(req); err != nil {
		return bad(err)
	}
	if err := deployment.ValidateHealthCheckThreshold(req); err != nil {
		return bad(err)
	}
	if err := a.deployments().ValidateMountModels(req); err != nil {
		return bad(err)
	}
	model, err := a.d.Store.GetModel(dep.ModelID)
	if err != nil {
		return internal(err)
	}
	engine, err := deploy.SelectEngine(dep.Engine)
	if err != nil {
		return internal(err)
	}
	nextReqs := reqs
	node, _ := a.d.Store.NodeWithGPUs(nodeUUIDOf(a.d.Store, dep.NodeID))
	if shape == deploymentShapeSingle {
		if deployment.IsAutomaticPlacement(req) {
			// The node an automatic draft's evaluation picked is the form's
			// preview; where the replicas run is the requirements' to say.
			req.NodeUUID = ""
		}
		var strategy store.PlacementStrategy
		nextReqs, strategy, err = a.deployments().RequestPlacement(req)
		if err != nil {
			return nil, nil, useCaseStatus(err), err
		}
		next.PlacementStrategy = strategy
		if pinned, ok := planning.PinnedNode(nextReqs); ok {
			if node, err = a.deployments().RequestNode(pinned); err != nil {
				return bad(err)
			}
			next.NodeID = node.ID
		}
		next.Replicas = req.Replicas
		next.VRAMLimitBytes = gibBytes(req.VRAMLimitGiB)
		next.PartitionBackend = ""
		if node != nil {
			if next.PartitionBackend, err = deployment.ResolvePartitionBackend(node, req.VRAMLimitGiB, req.PartitionBackend); err != nil {
				return bad(err)
			}
		} else if req.VRAMLimitGiB > 0 {
			next.PartitionBackend = strings.TrimSpace(req.PartitionBackend)
		}
		placement := deployment.StoredPlacement(req.Placement, deployment.IsAutomaticPlacement(req))
		if _, err := planning.ParsePlacement(placement); err != nil {
			return bad(err)
		}
		next.Placement = keepEquivalentJSON(dep.Placement, jsonStr(placement))
	}

	base, err := storedLaunchConfig(dep)
	if err != nil {
		return internal(err)
	}
	extra, err := deploy.ParseArgsText(req.ExtraArgs)
	if err != nil {
		return bad(wrapProductError(ErrorExtraArgsInvalid, "extra_args cannot be parsed", map[string]any{"detail": err.Error()}, err))
	}
	if err := deploy.ValidateExtraArgsJSON(extra); err != nil {
		return bad(err)
	}
	edited, removed, err := deploy.EditedParams(engine.Name(), base, req.QuickParams, req.QuickParamsUnset)
	if err != nil {
		return bad(err)
	}
	if err := deployment.ValidateRequestedLaunchConfig(engine.Name(), &dto.DeployRequest{QuickParams: edited, QuickParamsUnset: removed}, extra); err != nil {
		return bad(err)
	}
	snapshot, err := planning.DeploymentTemplate(dep)
	if err != nil {
		return internal(err)
	}
	overrides, err := deployment.RequestContainerOverrides(req.ContainerOverrides)
	if err != nil {
		return bad(err)
	}
	command, err := deploy.ResolveEffectiveCommand(overrides, snapshot.Entrypoint)
	if err != nil {
		return bad(err)
	}
	launch, err := deploy.Materialize(deploy.MaterializeInput{
		Engine: engine.Name(), Command: &command, Raw: req.QuickParams, Unset: req.QuickParamsUnset,
		TemplateArgs: snapshot.ExtraArgs, ExtraArgs: extra, Cards: deployment.DeployTargetCards(node, req.Placement), Base: &base,
	})
	if err != nil {
		return bad(err)
	}
	if shape == deploymentShapeMulti {
		// Count the effective DP after applying the stored base and edits,
		// exactly as create does. An omitted field retains the stored value.
		dp, err := deploy.PlacementDataParallel(engine.Name(), launch.Quick.DP, launch.TemplateResidual, launch.UserResidual)
		if err != nil {
			return bad(err)
		}
		if _, err := a.deployments().PrepareDistributedDeployRequest(req, dp); err != nil {
			return bad(err)
		}
		next.Placement = keepEquivalentJSON(dep.Placement, jsonStr(deployment.StoredPlacement(req.Placement, false)))
	}
	if shape == deploymentShapePD {
		if err := planning.CheckPDArgs(engine.Name(), launch.TemplateResidual, launch.UserResidual); err != nil {
			return bad(err)
		}
	}
	next.QuickParams = keepEquivalentJSON(dep.QuickParams, deployment.LaunchConfigJSON(launch.Quick))
	next.ExtraArgs = deploy.FormatArgsText(launch.UserResidual)
	next.ContainerOverrides = keepEquivalentJSON(dep.ContainerOverrides, jsonStr(req.ContainerOverrides))
	if image := strings.TrimSpace(req.Image); image != "" {
		// The snapshot keeps the template's image: its pull sources apply
		// only while the deployment runs that image (ImageSourcesFor).
		next.Image = image
	}
	if sleepErr := planning.SleepDeclarable(req.Sleepable, engine.Name(), req.TargetVendor); sleepErr != nil {
		return bad(sleepErr)
	}
	next.Sleepable = req.Sleepable
	// A yield policy that gives way needs the sleep it gives way by, as on
	// create (planning.YieldPolicyRefusal); the policy is changed on its own
	// route, so the edit is refused rather than the policy dropped.
	if next.Policy().Yield != yieldpolicy.Never && planning.YieldPolicyRefusal(&next) == ErrorSleepNotEnabled {
		return bad(productError(messages.ErrorYieldPolicyNeedsSleep,
			fmt.Sprintf("the deployment gives its cards way by yield policy %s; set the policy to never before turning sleep off", next.YieldPolicy),
			map[string]any{"policy": next.YieldPolicy}))
	}
	if next.HealthCheck, next.HealthCheckThresholdSec, err = deployment.CreateHealthCheck(req, model); err != nil {
		return bad(err)
	}
	next.TokenizeDisabled = req.TokenizeEnabled != nil && !*req.TokenizeEnabled
	next.StartTimeoutSec = req.StartTimeoutSec
	next.RoutingPolicy = req.RoutingPolicy
	launchChanged := next.QuickParams != dep.QuickParams || next.ExtraArgs != dep.ExtraArgs ||
		next.ContainerOverrides != dep.ContainerOverrides || next.Image != dep.Image
	next.TemplateDeviated = dep.TemplateDeviated || launchChanged
	// A single-node deployment no node could ever run is refused, as on
	// create; a multi-node or PD one names its nodes and is judged there.
	if shape == deploymentShapeSingle && (launchChanged || !sameRequirementSets(reqs, nextReqs) || next.Placement != dep.Placement ||
		next.VRAMLimitBytes != dep.VRAMLimitBytes || next.PartitionBackend != dep.PartitionBackend || next.Sleepable != dep.Sleepable) {
		if err := a.deployments().CheckStoredCanEverFit(ctx, &next, nextReqs); err != nil {
			var refused *messages.ProductError
			if errors.As(err, &refused) {
				return bad(err)
			}
			return internal(err)
		}
	}
	return &next, nextReqs, 0, nil
}

func nodeUUIDOf(st *store.Store, id uint) string {
	if node, err := st.NodeByID(id); err == nil {
		return node.UUID
	}
	return ""
}
