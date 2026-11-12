package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// setDeploymentRequirements replaces where a single-node deployment's
// replicas may run - its node requirements and placement strategy - without
// redeploying. Like a create, it refuses requirements no node could ever run
// a replica under (checkStoredCanEverFit). The change bounds replicas
// launched from now on: a running replica on a node the new requirements no
// longer admit is not moved.
func (a *apiHandlers) setDeploymentRequirements(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req dto.DeploymentRequirementsRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if dep.State == store.DeployDeleting {
		writeProductError(w, http.StatusConflict, ErrorDeploymentDeleting, "the deployment is being deleted", nil)
		return
	}
	pd, err := planning.PDOf(dep.Placement)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if pd != nil || dep.NNodes > 1 {
		shape := "multi_node"
		if pd != nil {
			shape = "pd"
		}
		writeProductError(w, http.StatusBadRequest, ErrorNodeRequirementsNotEditable, "a multi-node or PD deployment names its nodes per rank; it has no node requirements to edit", map[string]any{"shape": shape})
		return
	}
	strategy := dep.PlacementStrategy
	if req.PlacementStrategy != "" {
		if strategy, err = planning.PlacementStrategyOf(req.PlacementStrategy); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	stored, err := a.d.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	reqs, err := planning.EditedRequirements(req.NodeRequirements, stored)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.deployments().CheckStoredCanEverFit(r.Context(), dep, reqs); err != nil {
		status := http.StatusInternalServerError
		var refused *messages.ProductError
		if errors.As(err, &refused) {
			status = http.StatusBadRequest
		}
		writeErr(w, status, err)
		return
	}
	if err := a.d.Store.SetDeploymentRequirements(dep.ID, reqs, strategy); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound // deleted while the request was read
		}
		writeErr(w, status, err)
		return
	}
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.deploymentDTO(updated, true))
}

// requirementLabels describes a deployment's requirement values against the
// nodes there are now, for one deployment's read.
func (a *apiHandlers) requirementLabels(reqs map[string][]string) []dto.RequirementLabel {
	if len(reqs) == 0 {
		return nil
	}
	// A label's node count covers the fleet; only when the requirements name
	// nodes and nothing else does it follow from the named nodes alone.
	var nodes []*store.Node
	if planning.OnlyNamesNodes(reqs) {
		named, err := a.deployments().CandidateNodes(reqs)
		if err != nil {
			return nil
		}
		nodes = named
	} else {
		rows, err := a.d.Store.NodesForLabels(planning.RequirementsNeedCards(reqs))
		if err != nil {
			return nil
		}
		nodes = make([]*store.Node, 0, len(rows))
		for i := range rows {
			nodes = append(nodes, &rows[i])
		}
	}
	labels := planning.RequirementLabels(reqs, nodes, a.resolveForRequirements(nodes, reqs))
	out := make([]dto.RequirementLabel, 0, len(labels))
	for _, label := range labels {
		out = append(out, dto.RequirementLabel{Key: label.Key, Value: label.Value, Display: label.Display, DisplayCode: label.DisplayCode, Nodes: label.Nodes})
	}
	return out
}

// resolveForRequirements loads what matching reqs against nodes reads beyond
// the node rows: their networks and the storages that apply to them, each
// only when a requirement names one. A part that cannot be loaded stays nil,
// which leaves its requirements unmet.
func (a *apiHandlers) resolveForRequirements(nodes []*store.Node, reqs map[string][]string) planning.NodeFacts {
	rows := make([]store.Node, 0, len(nodes))
	for _, node := range nodes {
		rows = append(rows, *node)
	}
	return planning.LoadNodeFacts(a.d.Store, a.networks().Resolve, rows, reqs)
}
