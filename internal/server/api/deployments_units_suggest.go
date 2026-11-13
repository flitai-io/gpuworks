package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// unitSuggestGroups and unitSuggestTrials bound a suggestion: the best few
// groups, and a few trial plans per rank in each. A thousand-node cluster
// costs the same handful of plans as a small one.
const (
	unitSuggestGroups = 3
	unitSuggestTrials = 3
)

// suggestUnit proposes the nodes of one more replica of a multi-node
// deployment, for the operator to confirm in the replica editor: nodes that
// share a compute network and a GPU architecture, each with a rank's cards
// free, led from a node that leads no other replica, each rank trial-planned
// with the deployment's stored configuration. Nothing is written.
func (a *apiHandlers) suggestUnit(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req dto.UnitSuggestRequest
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
	if pd != nil || dep.NNodes <= 1 {
		writeProductError(w, http.StatusBadRequest, ErrorReplicasBodyShape, "only a multi-node deployment's replicas are listed as units", map[string]any{"want": "replicas"})
		return
	}
	placement, err := planning.ParsePlacement(dep.Placement)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	spec, err := planning.DeploymentParallelism(dep, placement)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	perRank, err := spec.GPUsPerNode()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	facts, err := a.unitSuggestFacts(dep, req.Units, perRank)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	groups, counts := planning.SuggestUnitGroups(facts)
	model, err := a.d.Store.GetModel(dep.ModelID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var firstRefusal map[string]any
	for _, group := range groups[:min(len(groups), unitSuggestGroups)] {
		skip := map[string]bool{}
		for trial := 0; trial < unitSuggestTrials*dep.NNodes; trial++ {
			nodes, ok := planning.UnitFromGroup(group, dep.NNodes, facts.Leaders, skip)
			if !ok {
				break
			}
			refused, reason, err := a.trialUnit(r, dep, facts, model, nodes)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if refused == nil {
				writeJSON(w, http.StatusOK, unitSuggestion(group, nodes))
				return
			}
			if firstRefusal == nil {
				firstRefusal = map[string]any{"node": refused.DisplayName(), "first_reason": reason}
			}
			skip[refused.UUID] = true
		}
	}
	if firstRefusal != nil {
		writeJSON(w, http.StatusOK, dto.UnitSuggestion{Reason: &dto.ScheduleReason{Code: "unit_no_fit", Params: firstRefusal}})
		return
	}
	writeJSON(w, http.StatusOK, dto.UnitSuggestion{Reason: &dto.ScheduleReason{Code: "unit_none", Params: map[string]any{
		"ranks": dep.NNodes, "per_rank": perRank, "nodes": counts.Nodes, "unmet": counts.Unmet, "cordoned": counts.Cordoned,
		"offline": counts.Offline, "busy": counts.Busy, "no_network": counts.NoNetwork, "largest": counts.Largest,
	}}})
}

// unitSuggestFacts reads what a suggestion is decided from in bulk
// (planning.LoadUnitSuggestFacts): the listed replicas' leaders cannot lead
// another, and the cards of a listed rank no instance of the deployment holds
// yet are claimed.
func (a *apiHandlers) unitSuggestFacts(dep *store.Deployment, listed []dto.UnitInput, perRank int) (planning.UnitSuggestFacts, error) {
	reqs, err := a.d.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		return planning.UnitSuggestFacts{}, err
	}
	var ranks []planning.ListedRank
	for _, unit := range listed {
		for i, uuid := range append([]string{unit.NodeUUID}, followerUUIDs(unit)...) {
			ranks = append(ranks, planning.ListedRank{NodeUUID: uuid, GPUs: perRank, Leader: i == 0})
		}
	}
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		return planning.UnitSuggestFacts{}, err
	}
	return planning.LoadUnitSuggestFacts(a.d.Store, a.networks().Resolve, dep, reqs, ranks, a.onlineOf(nodes), dep.NNodes, perRank)
}

// trialUnit plans every rank of a proposed replica as the controller would
// launch it: the group's shared network, the stored configuration, the
// template's hardware contract. It returns the first node that refuses and
// the scheduler's reason, or nil when every rank fits.
func (a *apiHandlers) trialUnit(r *http.Request, dep *store.Deployment, facts planning.UnitSuggestFacts, model *store.Model, nodes []*store.Node) (*store.Node, *dto.ScheduleReason, error) {
	rows := make([]store.Node, len(nodes))
	for i, node := range nodes {
		rows[i] = *node
	}
	group, err := a.networks().ComputeGroup(rows)
	if err != nil {
		return nodes[0], &dto.ScheduleReason{Code: "placement_rejected", Params: map[string]any{"detail": err.Error()}}, nil //nolint:nilerr // Network rejection is returned as a ScheduleReason, not a transport error.
	}
	for rank, node := range nodes {
		launch, layer, _, err := planning.DeploymentLaunch(dep, facts.Requirements, node, model, nil)
		if err != nil {
			return nil, nil, err
		}
		launch.Preview, launch.Topology = true, planning.PreviewRankTopology(dep.NNodes, rank, group.Ranks[node.ID])
		_, refusal, err := planning.PlanTemplateLaunch(r.Context(), a.d.Store, a.networks(), node, launch, layer)
		if err != nil {
			return nil, nil, err
		}
		if refusal != nil {
			return node, &dto.ScheduleReason{Code: refusal.Code, Params: refusal.Params}, nil
		}
	}
	return nil, nil, nil
}

func followerUUIDs(unit dto.UnitInput) []string {
	out := make([]string, 0, len(unit.Followers))
	for _, rank := range unit.Followers {
		out = append(out, rank.NodeUUID)
	}
	return out
}

func unitSuggestion(group planning.UnitGroup, nodes []*store.Node) dto.UnitSuggestion {
	free := map[uint]planning.PoolCandidate{}
	for _, candidate := range group.Nodes {
		free[candidate.Node.ID] = candidate
	}
	unit := &dto.UnitInput{NodeUUID: nodes[0].UUID}
	out := dto.UnitSuggestion{Unit: unit, Arch: group.Arch, Network: &dto.DeploymentNetworkBrief{ID: group.Network.ID, Name: group.Network.Name}}
	for i, node := range nodes {
		if i > 0 {
			unit.Followers = append(unit.Followers, dto.UnitRankInput{NodeUUID: node.UUID})
		}
		candidate := free[node.ID]
		out.Nodes = append(out.Nodes, dto.UnitSuggestedNode{UUID: node.UUID, Name: nodeDisplayName(node), FreeGPUs: candidate.FreeGPUs, Model: modelLocalityNames[candidate.Model]})
	}
	return out
}

// onlineOf is the deployment handlers' online check, or the nodes' stored
// state when the server runs without a hub.
func (a *apiHandlers) onlineOf(nodes []store.Node) func(string) bool {
	if a.d.Online != nil {
		return a.d.Online
	}
	online := make(map[string]bool, len(nodes))
	for i := range nodes {
		online[nodes[i].UUID] = nodes[i].State == store.NodeStateOnline
	}
	return func(uuid string) bool { return online[uuid] }
}
