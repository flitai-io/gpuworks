package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// getDeploymentDraft answers GET /deployments/{id}/draft.
func (a *apiHandlers) getDeploymentDraft(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
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
	writeJSON(w, http.StatusOK, draft)
}

// Shapes of a deployment, as an edit treats them.
const (
	deploymentShapeSingle = "single"
	deploymentShapeMulti  = "multi_node"
	deploymentShapePD     = "pd"
)

func deploymentShape(dep *store.Deployment) (string, error) {
	pd, err := planning.PDOf(dep.Placement)
	if err != nil {
		return "", err
	}
	switch {
	case pd != nil:
		return deploymentShapePD, nil
	case dep.NNodes > 1:
		return deploymentShapeMulti, nil
	}
	return deploymentShapeSingle, nil
}

// Request fields an edit cannot change (dto.DeploymentDraft.Locked). What a
// deployment is - its model, template, engine and names - is fixed for every
// shape; a multi-node or PD deployment also names its nodes, its replicas and
// its dGPU slice rank by rank, which PUT /deployments/{id}/replicas and
// /deployments/{id}/pd change instead.
var (
	lockedIdentityFields = []string{
		"model_id", "template_id", "template_spec_source", "template_spec_name", "scenario",
		"engine", "target_vendor", "name", "served_name", "model_source", "nnodes",
	}
	lockedPerRankFields = []string{
		"node_uuid", "node_uuids", "node_requirements", "placement_mode", "placement_strategy",
		"master_addr", "master_port", "dist_group", "dist_role", "node_rank",
		"replicas", "vram_limit_gib", "partition_backend",
	}
)

func lockedFields(shape string) []string {
	locked := slices.Clone(lockedIdentityFields)
	if shape != deploymentShapeSingle {
		locked = append(locked, lockedPerRankFields...)
	}
	if shape == deploymentShapePD {
		locked = append(locked, "placement")
	}
	return locked
}

// deploymentDraft reads a deployment back as the create request that would
// make it. It is the one projection of a stored deployment onto the request:
// the edit compares what it is sent against it, and the deploy form fills
// itself from it.
//
// The launch configuration is the stored one in full: an edit starts from
// what the deployment runs (deploy.MaterializeInput.Base), not from what its
// template says today, so nothing the edit does not touch can move.
func (a *apiHandlers) deploymentDraft(dep *store.Deployment, reqs map[string][]string) (dto.DeploymentDraft, error) {
	shape, err := deploymentShape(dep)
	if err != nil {
		return dto.DeploymentDraft{}, err
	}
	req := dto.DeployRequest{
		ModelID: dep.ModelID, TemplateID: dep.TemplateID,
		TemplateSpecSource: dep.TemplateSpecSource, TemplateSpecName: dep.TemplateSpecName, Scenario: dep.TemplateScenario,
		Name: dep.Name, ServedName: dep.ServedName, Engine: dep.Engine, Image: dep.Image,
		ExtraArgs: dep.ExtraArgs, Sleepable: dep.Sleepable,
		Replicas: max(dep.Replicas, 1), RoutingPolicy: dep.RoutingPolicy,
		StartTimeoutSec: dep.StartTimeoutSec, HealthCheckThresholdSec: dep.HealthCheckThresholdSec,
		PartitionBackend: dep.PartitionBackend, PlacementStrategy: string(dep.PlacementStrategy),
		NNodes: max(dep.NNodes, 1), NodeRank: dep.NodeRank, DistGroup: dep.DistGroup, DistRole: dep.DistRole,
		MasterAddr: dep.MasterAddr, MasterPort: dep.MasterPort,
	}
	healthCheck, tokenize := dep.HealthCheck, !dep.TokenizeDisabled
	req.HealthCheck, req.TokenizeEnabled = &healthCheck, &tokenize
	if dep.VRAMLimitBytes > 0 {
		req.VRAMLimitGiB = float64(dep.VRAMLimitBytes) / (1 << 30)
	}
	if req.QuickParams, err = storedObject(dep.QuickParams); err != nil {
		return dto.DeploymentDraft{}, err
	}
	if req.QuickParams == nil {
		req.QuickParams = map[string]any{}
	}
	if req.ContainerOverrides, err = storedObject(dep.ContainerOverrides); err != nil {
		return dto.DeploymentDraft{}, err
	}
	if req.Placement, err = storedObject(dep.Placement); err != nil {
		return dto.DeploymentDraft{}, err
	}
	snapshot, err := planning.DeploymentTemplate(dep)
	if err != nil {
		return dto.DeploymentDraft{}, err
	}
	if provider := a.draftWeightProvider(snapshot); dep.SaveToStorageID != 0 || provider != "" {
		req.ModelSource = &dto.ModelSource{StorageID: dep.SaveToStorageID, Provider: provider}
	}
	stated := planning.RequirementsOfDeployment(reqs, shape != deploymentShapeSingle)
	req.TargetVendor = stated.TargetVendor
	if shape == deploymentShapeSingle {
		req.PlacementMode = "auto"
		if stated.ManualNode != "" {
			// A manual placement is stored as a requirement naming its node.
			req.PlacementMode, req.NodeUUID = "manual", stated.ManualNode
		} else if node, err := a.d.Store.NodeByID(dep.NodeID); err == nil {
			req.NodeUUID = node.UUID
		}
	} else {
		// A multi-node or PD deployment says whether the platform places it.
		req.PlacementMode = unitsPlacementMode(dep)
	}
	if shape == deploymentShapeMulti {
		ids, err := dep.StoredNodeIDs()
		if err != nil {
			return dto.DeploymentDraft{}, err
		}
		for _, id := range ids {
			node, err := a.d.Store.NodeByID(id)
			if err != nil {
				return dto.DeploymentDraft{}, err
			}
			req.NodeUUIDs = append(req.NodeUUIDs, node.UUID)
		}
		if len(req.NodeUUIDs) > 0 {
			req.NodeUUID = req.NodeUUIDs[0]
		}
	}
	if len(stated.Stated) > 0 {
		req.NodeRequirements = stated.Stated
	}
	revision, err := draftRevision(req)
	if err != nil {
		return dto.DeploymentDraft{}, err
	}
	return dto.DeploymentDraft{Deployment: req, Revision: revision, Locked: lockedFields(shape)}, nil
}

// storedObject decodes a JSON object column; empty and null are no object.
func storedObject(column string) (map[string]any, error) {
	column = strings.TrimSpace(column)
	if column == "" || column == "null" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(column), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// draftRevision fingerprints a draft: two reads of an unchanged deployment
// give the same revision, any change to what the draft states a new one.
func draftRevision(req dto.DeployRequest) (string, error) {
	encoded, err := json.Marshal(req) // map keys marshal sorted
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8]), nil
}

// gibBytes is a vram_limit_gib as the bytes a deployment stores, rounded so
// a value read back from the draft lands on the bytes it came from.
func gibBytes(gib float64) int64 {
	return int64(math.Round(gib * (1 << 30)))
}

// storedLaunchConfig decodes a deployment's stored launch configuration.
func storedLaunchConfig(dep *store.Deployment) (deploy.LaunchConfig, error) {
	var quick deploy.LaunchConfig
	if strings.TrimSpace(dep.QuickParams) == "" {
		return quick, nil
	}
	err := json.Unmarshal([]byte(dep.QuickParams), &quick)
	return quick, err
}

// draftWeightProvider is the weight source a draft states as chosen: the
// provider the snapshot mounted, when the template spec declares it. A spec
// is satisfied by any ready copy of its repository, so the snapshot may name a
// copy's own provider instead - a model imported into a storage reads "local"
// - which is not a choice the spec offers: stating it would have the
// evaluation of the draft refuse the deployment's own source
// (ACQUISITION_CANDIDATE_STALE), and the field is locked in an edit. Such a
// draft states none and the evaluation picks the ready copy again. A spec that
// can no longer be read keeps the snapshot's provider.
func (a *apiHandlers) draftWeightProvider(snapshot deploy.CatalogSelection) string {
	provider := strings.TrimSpace(snapshot.WeightProvider)
	if provider == "" || snapshot.TemplateID == 0 {
		return provider
	}
	model, err := a.d.Store.InferenceModelByID(snapshot.TemplateID)
	if err != nil {
		return provider
	}
	for i := range model.Specs {
		spec := &model.Specs[i]
		if spec.Name != snapshot.SpecName || spec.Source != snapshot.SpecSource {
			continue
		}
		for _, source := range deployment.SpecWeightSources(spec) {
			if strings.EqualFold(source.Provider, provider) {
				return provider
			}
		}
		return ""
	}
	return provider
}
