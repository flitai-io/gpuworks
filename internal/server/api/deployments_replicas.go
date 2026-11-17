package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// replicasRequest is PUT /deployments/{id}/replicas, the one scaling entry of
// an ordinary deployment. A single-node deployment sends a count and, when it
// shrinks, may name the instances whose replicas go (the rest is the
// controller's pick: not yet ready first, then the newest). A multi-node
// deployment lists every replica it wants, each a leader node and its
// follower nodes in rank order; a replica left out drains and goes. PD scales
// its roles with PUT /deployments/{id}/pd instead. A single-node deployment
// that grows may name in PlaceOn the nodes its new replicas start on first
// (lifecycle.Orders.HintPlacement): only where they first start, afterwards
// they are ordinary replicas.
type replicasRequest struct {
	Replicas *int              `json:"replicas,omitempty"`
	Remove   []string          `json:"remove,omitempty"`
	Units    []planning.PDUnit `json:"units,omitempty"`
	PlaceOn  []string          `json:"place_on,omitempty"`
}

func (a *apiHandlers) scaleReplicas(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req replicasRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	pd, err := planning.PDOf(dep.Placement)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if pd != nil {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasBodyShape, "a PD deployment scales its roles with PUT /deployments/{id}/pd", map[string]any{"want": "pd"})
		return
	}
	if dep.State == store.DeployDeleting {
		writeProductError(w, http.StatusConflict, ErrorDeploymentDeleting, "the deployment is being deleted", nil)
		return
	}
	if dep.NNodes > 1 {
		if dep.UnitsAuto && len(req.Units) == 0 && req.Replicas != nil {
			// The platform places this deployment: a count, with nodes
			// optionally named for the new replicas' first placement.
			units, err := a.scaling().AutoReplicaUnits(dep, *req.Replicas, req.PlaceOn, req.Remove)
			if err != nil {
				writeScaleErr(w, err)
				return
			}
			a.setReplicaUnits(w, r, dep, units)
			return
		}
		if req.Replicas != nil || len(req.Remove) > 0 || len(req.Units) == 0 {
			writeProductError(w, http.StatusBadRequest, ErrorReplicasBodyShape, "a multi-node deployment's replicas are listed as units", map[string]any{"want": "units"})
			return
		}
		a.setReplicaUnits(w, r, dep, req.Units)
		return
	}
	if len(req.Units) > 0 {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasBodyShape, "a single-node deployment's replicas are a count", map[string]any{"want": "replicas"})
		return
	}
	if len(req.PlaceOn) > 0 && dep.NNodes > 1 {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasBodyShape, "a multi-node deployment lists its replicas' nodes as units", map[string]any{"want": "units"})
		return
	}
	if req.Replicas == nil || *req.Replicas < 1 {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasTooFew, "replicas must be at least 1; stop the deployment to run none", map[string]any{"minimum": 1})
		return
	}
	replicas := *req.Replicas
	if replicas > planning.MaxReplicas {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasTooMany, fmt.Sprintf("replicas must be at most %d", planning.MaxReplicas), map[string]any{"maximum": planning.MaxReplicas})
		return
	}
	current := max(dep.Replicas, 1)
	if shrink := current - replicas; len(req.Remove) > max(shrink, 0) {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasRemoveTooMany, "more instances named than replicas removed", map[string]any{"count": len(req.Remove), "maximum": max(shrink, 0)})
		return
	}
	if len(req.Remove) > 0 {
		insts, err := a.d.Store.InstancesForDeployment(dep.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		match, err := a.deploymentUnitMatch(dep, insts)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		for i, uuid := range req.Remove {
			slot := match.SlotOf(uuid)
			if slot == nil || slot.Inst.UUID != uuid || slices.Contains(req.Remove[:i], uuid) {
				writeProductError(w, http.StatusBadRequest, ErrorReplicasRemoveInvalid, "a named instance does not fill a replica of this deployment", map[string]any{"instance": uuid})
				return
			}
		}
	}
	if len(req.PlaceOn) > 0 {
		if grow := replicas - current; len(req.PlaceOn) > max(grow, 0) {
			writeProductError(w, http.StatusBadRequest, ErrorReplicasPlaceOnTooMany, "more nodes named than replicas added", map[string]any{"count": len(req.PlaceOn), "maximum": max(grow, 0)})
			return
		}
		if !a.checkPlaceOn(w, r, dep, req.PlaceOn) {
			return
		}
	}
	// This records desired state, including for stopped deployments. Capacity
	// is checked by the controller for each launch: pinned cards remain pinned,
	// and a replica waiting for them is explained by placement_status/events.
	err = a.d.Store.ScaleDeploymentReplicas(dep.ID, dep.Replicas, replicas, req.Remove)
	if errors.Is(err, store.ErrStateChanged) {
		writeProductError(w, http.StatusConflict, ErrorReplicasChanged, "the replicas changed since the request was read; read them again", nil)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		// Deleted while the request was read.
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(req.PlaceOn) > 0 {
		a.orders().HintPlacement(dep.ID, req.PlaceOn)
	}
	a.recordReplicasChanged(r, dep.ID, current, replicas)
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.deploymentDTO(updated, true))
}

// setReplicaUnits writes a multi-node deployment's listed replicas and
// answers with the deployment.
func (a *apiHandlers) setReplicaUnits(w http.ResponseWriter, r *http.Request, dep *store.Deployment, units []planning.PDUnit) {
	if err := a.scaling().ApplyReplicaUnits(dep, units); err != nil {
		writeScaleErr(w, err)
		return
	}
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.recordReplicasChanged(r, dep.ID, max(dep.Replicas, 1), max(updated.Replicas, 1))
	writeJSON(w, http.StatusOK, a.deploymentDTO(updated, true))
}

// recordReplicasChanged puts an operator's change of the replica count on the
// deployment's timeline; an order that changes nothing leaves no entry.
func (a *apiHandlers) recordReplicasChanged(r *http.Request, id uint, from, to int) {
	if from != to {
		a.operatorEvent(r, id, store.EventReplicasChanged, map[string]any{"from": from, "to": to})
	}
}

// checkPlaceOn checks the nodes a growing single-node deployment names for
// its new replicas: each is known, online, takes replicas, satisfies the
// deployment's node requirements, and has room for a replica now (the same
// trial plan the candidate pool runs, counting a node named twice twice). It
// writes the response on refusal.
func (a *apiHandlers) checkPlaceOn(w http.ResponseWriter, r *http.Request, dep *store.Deployment, uuids []string) bool {
	reqs, err := a.d.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	var model *store.Model
	refuse := func(node, reason string, params map[string]any) bool {
		if params == nil {
			params = map[string]any{}
		}
		params["node"], params["reason"] = node, reason
		writeProductError(w, http.StatusConflict, ErrorReplicasPlaceOnRefused, fmt.Sprintf("node %s cannot take a new replica: %s", node, reason), params)
		return false
	}
	seen := map[string]bool{}
	for _, uuid := range uuids {
		node, err := a.d.Store.NodeWithGPUs(uuid)
		if errors.Is(err, store.ErrNotFound) {
			return refuse(uuid, "unknown", nil)
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return false
		}
		name := node.DisplayName()
		switch {
		case !a.onlineOf([]store.Node{*node})(node.UUID):
			return refuse(name, "offline", nil)
		case !node.Scheduling.TakesReplicas():
			return refuse(name, "cordoned", nil)
		}
		if err := planning.MatchRequirements(node, a.resolveForRequirements([]*store.Node{node}, reqs), reqs); err != nil {
			return refuse(name, "requirements", map[string]any{"detail": err.Error()})
		}
		if seen[uuid] {
			continue // a second replica on the same node: the controller's pool counts it
		}
		seen[uuid] = true
		if model == nil {
			if model, err = a.d.Store.GetModel(dep.ModelID); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return false
			}
		}
		launch, layer, _, err := planning.DeploymentLaunch(dep, reqs, node, model, nil)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return false
		}
		launch.Preview = true
		_, refusal, err := planning.PlanTemplateLaunch(r.Context(), a.d.Store, a.networks(), node, launch, layer)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return false
		}
		if refusal != nil {
			return refuse(name, "no_room", map[string]any{"code": refusal.Code, "params": refusal.Params})
		}
	}
	return true
}

// replicaUnitsDTO is a multi-node deployment's replicas as the response lists
// them: each one's leader and follower nodes, named, with its spent retry
// budget; the shape a PD role's units take.
func replicaUnitsDTO(rows deployment.Rows, d *store.Deployment, view replicaView) []dto.DeploymentPDUnit {
	replicas := planning.ReplicaUnitsOf(d, view.ordered)
	out := make([]dto.DeploymentPDUnit, 0, len(replicas))
	for index, unit := range replicas {
		item := dto.DeploymentPDUnit{NodeUUID: unit.NodeUUID, NodeName: view.names[unit.NodeUUID]}
		if index < len(view.Match.Slots) {
			slot := view.Match.Slots[index]
			unitAttempts(rows, &item, slot, view.Match.Failed[slot.Key])
		}
		for _, rank := range unit.Followers {
			item.Followers = append(item.Followers, dto.DeploymentPDRank{NodeUUID: rank.NodeUUID, NodeName: view.names[rank.NodeUUID]})
		}
		out = append(out, item)
	}
	return out
}
