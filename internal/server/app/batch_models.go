package app

import (
	"errors"
	"net/http"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// batchModels tells the batch service which served names the platform has and
// what each is to a batch. The create check (Lookup) and the list a client
// chooses from (List) both judge a name by nameEntry, so they cannot disagree.
// A deployment's state is the one the deployment list shows
// (deployment.EffectiveStates).
type batchModels struct {
	st *store.Store
	// prep reads model preparation, which tells a deployment whose download
	// failed from one still being prepared; nil reads none.
	prep deployment.ModelPreparation
	// lists is the server's list snapshot List reads; nil reads the rows
	// per call.
	lists *deployment.Lists
}

func (m batchModels) Lookup(name string) (batch.ModelEntry, error) {
	rows, err := m.st.DeploymentsServingName(name)
	if err != nil || len(rows) == 0 {
		return batch.ModelEntry{Name: name}, err
	}
	var insts []store.Instance
	for _, d := range rows {
		own, err := m.st.InstancesForDeployment(d.ID)
		if err != nil {
			return batch.ModelEntry{Name: name}, err
		}
		insts = append(insts, own...)
	}
	models, err := m.modelsOf(rows)
	if err != nil {
		return batch.ModelEntry{Name: name}, err
	}
	entries, err := m.entries(rows, insts, models)
	if err != nil || len(entries) == 0 {
		return batch.ModelEntry{Name: name}, err
	}
	return entries[0], nil
}

// List is every served name of a deployment that is not being deleted, with
// its state: the deployments, their instances and their models are each read
// once, as the monitor's inventory reads them.
func (m batchModels) List() ([]batch.ModelEntry, error) {
	var rows *deployment.ListRows
	var err error
	if m.lists != nil {
		rows, err = m.lists.Rows()
	} else {
		rows, err = deployment.ReadListRows(m.st, m.prep)
	}
	if err != nil {
		return nil, err
	}
	deployments, instances := rows.Deployments(), rows.AllInstances()
	byID := make(map[uint]*store.Model)
	for i := range deployments {
		if model := rows.Model(deployments[i].ModelID); model != nil {
			byID[model.ID] = model
		}
	}
	entries, err := m.entriesWith(deployments, instances, byID, rows.States())
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].State != entries[j].State {
			return entries[i].State.Better(entries[j].State)
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// modelsOf reads the models of rows, for their API contracts.
func (m batchModels) modelsOf(rows []store.Deployment) (map[uint]*store.Model, error) {
	out := map[uint]*store.Model{}
	for _, d := range rows {
		if d.ModelID == 0 || out[d.ModelID] != nil {
			continue
		}
		model, err := m.st.GetModel(d.ModelID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[d.ModelID] = model
	}
	return out, nil
}

// entries are the served names of deployments (none being deleted), one
// entry per name: its best state and the endpoints any of its deployments
// exposes, in no particular order.
func (m batchModels) entries(deployments []store.Deployment, instances []store.Instance, models map[uint]*store.Model) ([]batch.ModelEntry, error) {
	return m.entriesWith(deployments, instances, models, nil)
}

// entriesWith is entries with every deployment's state already worked out
// (ListRows.States); nil works the kept deployments' states out.
func (m batchModels) entriesWith(deployments []store.Deployment, instances []store.Instance, models map[uint]*store.Model, states map[uint]string) ([]batch.ModelEntry, error) {
	var kept []store.Deployment
	for _, d := range deployments {
		if d.ServedName != "" && (d.State == store.DeployActive || d.State == store.DeployStopped) {
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	if states == nil {
		var err error
		if states, err = deployment.EffectiveStates(m.st, m.prep, kept, instances); err != nil {
			return nil, err
		}
	}
	byDeployment := map[uint][]store.Instance{}
	for _, in := range instances {
		byDeployment[in.DeploymentID] = append(byDeployment[in.DeploymentID], in)
	}
	byName := map[string]*batch.ModelEntry{}
	var order []string
	for i := range kept {
		d := &kept[i]
		e := byName[d.ServedName]
		if e == nil {
			e = &batch.ModelEntry{Name: d.ServedName}
			byName[d.ServedName] = e
			order = append(order, d.ServedName)
		}
		if state := deploymentState(d, states[d.ID], byDeployment[d.ID]); state.Better(e.State) {
			e.State = state
		}
		contract := inferenceapi.ModelContract(models[d.ModelID])
		for _, endpoint := range batch.Endpoints {
			if !e.Serves(endpoint) && inferenceapi.AllowsContractRequest(contract, http.MethodPost, endpoint) {
				e.Endpoints = append(e.Endpoints, endpoint)
			}
		}
	}
	out := make([]batch.ModelEntry, 0, len(order))
	for _, name := range order {
		e := byName[name]
		sort.Slice(e.Endpoints, func(i, j int) bool { return endpointRank(e.Endpoints[i]) < endpointRank(e.Endpoints[j]) })
		out = append(out, *e)
	}
	return out, nil
}

// endpointRank orders endpoints as batch.Endpoints does.
func endpointRank(endpoint string) int {
	for i, e := range batch.Endpoints {
		if e == endpoint {
			return i
		}
	}
	return len(batch.Endpoints)
}

// deploymentState is what one deployment is to a batch, from the state the
// deployment list shows (deployment.EffectiveState): a running deployment
// serves; one the gateway refuses for sleep is woken by a request when one of
// its replicas wakes on request (planning.RequestWakes, the gateway's rule),
// and otherwise every sleep is held.
func deploymentState(d *store.Deployment, state string, insts []store.Instance) batch.ModelState {
	switch state {
	case "running":
		return batch.ModelRunning
	case deployment.StateAsleep, deployment.StateSleeping:
		if planning.RequestWakes(d, insts) {
			return batch.ModelAsleep
		}
		return batch.ModelSleepHeld
	case "offline":
		return batch.ModelOffline
	case "failed":
		return batch.ModelFailed
	case "stopped", deployment.StateDeleting:
		return batch.ModelStopped
	default: // pending, pulling, starting
		return batch.ModelStarting
	}
}
