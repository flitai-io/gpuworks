package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const (
	candidatesDefaultLimit = 50
	candidatesMaxLimit     = 200
)

var modelLocalityNames = map[planning.ModelLocality]string{
	planning.ModelPresent: "present", planning.ModelFetching: "fetching", planning.ModelAbsent: "absent",
}

// deploymentCandidates answers why a deployment's replicas land where they
// do, or nowhere: the cluster classified for its next replica exactly as the
// controller classifies it (planning.BuildPool), read live from the same bulk
// reads, and paged - a thousand nodes are never one response. It plans
// nothing: whether a candidate that looks like it has room really has is the
// controller's trial, and its latest answer is in the deployment's status.
func (a *apiHandlers) deploymentCandidates(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	offset, limit, filter, err := candidatesPage(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	pool, nodes, err := a.deploymentPool(dep)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.DeploymentCandidates{
		Nodes: nodes, Counts: map[string]int{}, UnmetByKey: pool.UnmetByKey,
		Strategy: string(dep.PlacementStrategy), Offset: offset, Limit: limit, Items: []dto.DeploymentCandidate{},
	}
	for _, status := range planning.PoolStatuses() {
		out.Counts[string(status)] = pool.Counts[status]
	}
	for _, entry := range pool.Entries {
		if filter != "" && string(entry.Status) != filter {
			continue
		}
		out.Total++
		if out.Total <= offset || len(out.Items) == limit {
			continue
		}
		var reason *dto.ScheduleReason
		if entry.Reason != nil {
			reason = &dto.ScheduleReason{Code: entry.Reason.Code, Params: entry.Reason.Params}
		}
		out.Items = append(out.Items, dto.DeploymentCandidate{
			UUID: entry.Node.UUID, Name: nodeDisplayName(entry.Node), Status: string(entry.Status), Rank: entry.Rank,
			Detail: entry.Detail, Replicas: entry.Replicas, FreeGPUs: entry.FreeGPUs, GPUs: len(entry.Node.OwnedGPUs()),
			Model: modelLocalityNames[entry.Model], Reason: reason,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// deploymentPool builds a deployment's pool from the reads the controller
// makes: nodes, held GPUs, the model's files, its own instances, and the
// networks when a requirement names one.
func (a *apiHandlers) deploymentPool(dep *store.Deployment) (*planning.Pool, int, error) {
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		return nil, 0, err
	}
	held, err := planning.HeldGPUs(a.d.Store, dep.YieldPolicy)
	if err != nil {
		return nil, 0, err
	}
	files, err := a.d.Store.ModelFilesForModel(dep.ModelID)
	if err != nil {
		return nil, 0, err
	}
	instances, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		return nil, 0, err
	}
	reqs, err := a.d.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		return nil, 0, err
	}
	facts := planning.LoadNodeFacts(a.d.Store, a.networks().Resolve, nodes, reqs)
	online := a.d.Online
	if online == nil {
		online = func(uuid string) bool {
			for i := range nodes {
				if nodes[i].UUID == uuid {
					return nodes[i].State == store.NodeStateOnline
				}
			}
			return false
		}
	}
	placement, err := planning.ParsePlacement(dep.Placement)
	if err != nil {
		return nil, 0, err
	}
	admission := planning.TargetRefusalFor(dep, reqs, func() (*store.Model, error) { return a.d.Store.GetModel(dep.ModelID) }, online)
	pool := planning.BuildPool(planning.PoolFacts{
		Nodes: nodes, Held: held, Files: files, Facts: facts, Instances: instances,
		Online: online, Requirements: reqs, Strategy: dep.PlacementStrategy, Need: planning.ReplicaGPUNeed(dep), Placement: placement,
		TargetRefusal: admission.Refusal,
	})
	if err := admission.Err(); err != nil {
		return nil, 0, err
	}
	return pool, len(nodes), nil
}

func candidatesPage(r *http.Request) (offset, limit int, status string, err error) {
	query := r.URL.Query()
	limit = candidatesDefaultLimit
	if raw := query.Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > candidatesMaxLimit {
			return 0, 0, "", productError(ErrorFieldInvalid, "limit must be between 1 and 200", map[string]any{"field": "limit", "detail": "1..200"})
		}
	}
	if raw := query.Get("offset"); raw != "" {
		if offset, err = strconv.Atoi(raw); err != nil || offset < 0 {
			return 0, 0, "", productError(ErrorFieldInvalid, "offset must not be negative", map[string]any{"field": "offset", "detail": ">=0"})
		}
	}
	status = query.Get("status")
	if status == "" || slices.Contains(planning.PoolStatuses(), planning.PoolStatus(status)) {
		return offset, limit, status, nil
	}
	return 0, 0, "", productError(ErrorFieldInvalid, "status must be room, busy, unmet, cordoned or offline", map[string]any{"field": "status", "detail": status})
}

// placementStatus counts the pool replicas no instance fills and attaches
// the controller's latest scheduling reason. Only then is the latest event
// read, so a list of healthy deployments costs no query for it.
func placementStatus(rows deployment.Rows, d *store.Deployment, view replicaView) *dto.DeploymentPlacementStatus {
	if d.State != store.DeployActive {
		return nil
	}
	unscheduled := 0
	for _, slot := range view.Match.Slots {
		if slot.Unit.Pool() && slot.Inst == nil {
			unscheduled++
		}
	}
	if unscheduled == 0 {
		return nil
	}
	out := &dto.DeploymentPlacementStatus{Unscheduled: unscheduled}
	ev, found := rows.LatestScheduleEvent(d.ID)
	if !found {
		return out
	}
	var data struct {
		Scheduler *dto.ScheduleReason `json:"scheduler"`
	}
	if json.Unmarshal([]byte(ev.Data), &data) == nil && data.Scheduler != nil {
		at := ev.CreatedAt
		currentNodeNames(rows, data.Scheduler)
		out.Reason, out.At = data.Scheduler, &at
	}
	return out
}

// currentNodeNames shows the nodes a recorded pool reason names under their
// current names: the record keeps each node's UUID beside the name it had
// then, and a node renamed since would otherwise show its old name.
func currentNodeNames(rows deployment.Rows, reason *dto.ScheduleReason) {
	closest, ok := reason.Params["closest"].([]any)
	if !ok {
		return
	}
	for i, raw := range closest {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		uuid, _ := item["node"].(string)
		node := rows.NodeByUUID(uuid)
		if node == nil {
			continue
		}
		item["name"] = node.DisplayName()
		if i == 0 {
			reason.Params["node"] = item["name"]
		}
	}
}

// nodePointers addresses each of nodes in place.
func nodePointers(nodes []store.Node) []*store.Node {
	out := make([]*store.Node, len(nodes))
	for i := range nodes {
		out[i] = &nodes[i]
	}
	return out
}
