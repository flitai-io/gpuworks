package api

import (
	"cmp"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// listDeployments answers GET /deployments. With page or page_size it returns
// one dto.DeploymentPage of list rows, filtered by q (a substring of the
// name, the served name or the model's name) and state (effective states,
// comma separated); without them, every deployment as a full dto.Deployment.
//
// Either way the rows are read with a fixed number of queries; only the
// requested page is turned into DTOs. A page is ordered by sort (one of
// deploymentListSorts, "-" for descending), after filtering and before paging.
func (a *apiHandlers) listDeployments(w http.ResponseWriter, r *http.Request) {
	query, err := parseListQuery(r, deploymentListStates)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	order, err := deploymentListSorts.parse(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	deps, err := a.d.Store.ListDeployments()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	instances, err := a.d.Store.ListInstances()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rows, err := deployment.NewListRows(a.d.Store, a.d.Preparation, deps, instances)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	matched := make([]*store.Deployment, 0, len(deps))
	for i := range deps {
		if deploymentMatches(rows, &deps[i], query) {
			matched = append(matched, &deps[i])
		}
	}
	deploymentListSorters(rows).apply(matched, order)
	// Without page parameters every match comes back in full; the filters
	// and order apply either way.
	if !query.paged {
		out := make([]dto.Deployment, 0, len(matched))
		for _, d := range matched {
			out = append(out, a.deploymentDTOFrom(rows, d, false))
		}
		writeJSON(w, http.StatusOK, dto.NewList(out))
		return
	}
	writeJSON(w, http.StatusOK, pageOf(matched, query, func(page []*store.Deployment) []dto.DeploymentListItem {
		items := make([]dto.DeploymentListItem, 0, len(page))
		for _, d := range page {
			full := a.deploymentDTOFrom(rows, d, false)
			items = append(items, deploymentListItem(full, deploymentPreparationSummary(d, full.ModelProgress)))
		}
		return items
	}))
}

// deploymentListSorts names the orders GET /deployments offers; the
// comparisons read the lookup, so the ones that sort are built per request.
var deploymentListSorts = deploymentListSorters(nil)

// deploymentListSorters are the orders GET /deployments offers, by the
// deployment list's column keys. A state is worked out once per deployment
// and only when the list is sorted by it.
func deploymentListSorters(rows deployment.Rows) listSorters[*store.Deployment] {
	modelName := func(d *store.Deployment) string {
		if model := rows.Model(d.ModelID); model != nil {
			return model.Name
		}
		return ""
	}
	states := map[uint]string{}
	state := func(d *store.Deployment) string {
		if s, ok := states[d.ID]; ok {
			return s
		}
		insts := rows.Instances(d.ID)
		runtime := deployment.RuntimeState(rows, d, insts, deployment.RecordedModelProgress(rows, d))
		states[d.ID] = effectiveDeploymentState(d, runtime, deploymentFactsFor(rows, d, insts))
		return states[d.ID]
	}
	return listSorters[*store.Deployment]{
		"name":  func(a, b *store.Deployment) int { return compareText(a.Name, b.Name) },
		"model": func(a, b *store.Deployment) int { return compareText(modelName(a), modelName(b)) },
		"state": func(a, b *store.Deployment) int { return cmp.Compare(state(a), state(b)) },
		"replicas": func(a, b *store.Deployment) int {
			return cmp.Compare(deploymentListReplicas(a), deploymentListReplicas(b))
		},
		"created_at": func(a, b *store.Deployment) int { return a.CreatedAt.Compare(b.CreatedAt) },
	}
}

// deploymentListReplicas counts requested service replicas, including both PD
// roles. The stored Replicas field only configures ordinary deployments.
func deploymentListReplicas(d *store.Deployment) int {
	if pd, err := planning.PDOf(d.Placement); err == nil && pd != nil {
		return pd.InstanceCount()
	}
	return d.Replicas
}

// deploymentMatches applies a list query's filters to one deployment.
func deploymentMatches(rows deployment.Rows, d *store.Deployment, query listQuery) bool {
	modelName := ""
	if model := rows.Model(d.ModelID); model != nil {
		modelName = model.Name
	}
	if !query.matchesSearch(d.Name, d.ServedName, modelName) {
		return false
	}
	if len(query.states) == 0 {
		return true
	}
	insts := rows.Instances(d.ID)
	runtime := deployment.RuntimeState(rows, d, insts, deployment.RecordedModelProgress(rows, d))
	return query.matchesState(effectiveDeploymentState(d, runtime, deploymentFactsFor(rows, d, insts)))
}

// deploymentListItem is the list row of a deployment DTO; preparation is the
// one line it shows of the DTO's model progress.
func deploymentListItem(d dto.Deployment, preparation *dto.ModelNodePresence) dto.DeploymentListItem {
	// PD readiness already accounts for sleeping instances and incomplete
	// multi-node groups. Reuse the role summary rather than count containers.
	if d.PD != nil {
		desired, ready := 0, 0
		for _, role := range d.PD.Roles {
			desired += role.Desired
			ready += role.Ready
		}
		d.Replicas, d.ReadyReplicas = desired, &ready
	}
	return dto.DeploymentListItem{
		Preparation: preparation, ID: d.ID, Name: d.Name, TemplateID: d.TemplateID, TemplateDeviated: d.TemplateDeviated,
		Engine: d.Engine, Image: d.Image, ModelID: d.ModelID, ModelName: d.ModelName, ServedName: d.ServedName,
		NodeUUID: d.NodeUUID, NodeName: d.NodeName, NodeNames: d.NodeNames,
		// The row shows where the replicas run as well as where they were
		// asked to run; both come from the DTO so the row and the drawer can
		// never disagree about either.
		RuntimeNodes: d.RuntimeNodes, RuntimeNodeNames: d.RuntimeNodeNames,
		DesiredState: d.DesiredState, RuntimeState: d.RuntimeState, State: d.State,
		AsleepReplicas: d.AsleepReplicas, SleepLevel: d.SleepLevel, Sleepable: d.Sleepable, Replicas: d.Replicas,
		ReadyReplicas: d.ReadyReplicas, Health: d.Health,
		Operations: d.Operations, CreatedAt: d.CreatedAt,
	}
}
