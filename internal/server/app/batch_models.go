package app

import (
	"errors"
	"net/http"
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
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
// (deployment.RuntimeStates), refined by sleep as the gateway judges it.
type batchModels struct {
	st *store.Store
	// prep reads model preparation, which tells a deployment whose download
	// failed from one still being prepared; nil reads none.
	prep deployment.ModelPreparation
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
	deployments, err := m.st.ListDeployments()
	if err != nil {
		return nil, err
	}
	instances, err := m.st.ListInstances()
	if err != nil {
		return nil, err
	}
	models, err := m.st.ListModels()
	if err != nil {
		return nil, err
	}
	byID := make(map[uint]*store.Model, len(models))
	for i := range models {
		byID[models[i].ID] = &models[i]
	}
	entries, err := m.entries(deployments, instances, byID)
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
	var kept []store.Deployment
	for _, d := range deployments {
		if d.ServedName != "" && (d.State == store.DeployActive || d.State == store.DeployStopped) {
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	states, err := deployment.RuntimeStates(m.st, m.prep, kept, instances)
	if err != nil {
		return nil, err
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

// deploymentState is what one deployment is to a batch, from the runtime
// state the deployment list shows: a running deployment some of whose
// running replicas are awake serves; with every running replica asleep
// (planning.InstanceAsleep on their leaders) a request wakes it when one of
// them wakes on request (planning.RequestWakes, the gateway's rule), and
// otherwise every sleep is held.
func deploymentState(d *store.Deployment, runtimeState string, insts []store.Instance) batch.ModelState {
	switch runtimeState {
	case "running":
		if anyReplicaServes(insts) || !replicasAsleep(insts) {
			return batch.ModelRunning
		}
		if planning.RequestWakes(d, insts) {
			return batch.ModelAsleep
		}
		return batch.ModelSleepHeld
	case "offline":
		return batch.ModelOffline
	case "failed":
		return batch.ModelFailed
	case "stopped":
		return batch.ModelStopped
	default: // pending, pulling, starting
		return batch.ModelStarting
	}
}

// anyReplicaServes says a running replica takes requests: its leader is not
// asleep and is not ordered to sleep.
func anyReplicaServes(insts []store.Instance) bool {
	for i := range insts {
		in := &insts[i]
		if in.State == proto.InstRunning && !in.Headless && !planning.InstanceAsleep(in) {
			return true
		}
	}
	return false
}

// replicasAsleep says the deployment has running replicas and every one is
// asleep. A follower of a multi-node replica sleeps with its leader and is not
// judged on its own.
func replicasAsleep(insts []store.Instance) bool {
	running := 0
	for i := range insts {
		in := &insts[i]
		if in.State != proto.InstRunning || in.Headless {
			continue
		}
		running++
		if !planning.InstanceAsleep(in) {
			return false
		}
	}
	return running > 0
}
