package deployment

import (
	"math"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// RuntimeState is the runtime_state the deployment DTO reports.
func RuntimeState(rows Rows, d *store.Deployment, insts []store.Instance, progress []dto.ModelNodePresence) string {
	state := DeriveRuntimeState(d.State, insts, progress, scheduleStalled(rows, d.ID, len(insts)))
	if state != "running" {
		return state
	}
	connected := OnlineInstances(rows, insts)
	incomplete := planning.GroupsIncomplete(connected)
	for _, inst := range connected {
		// Sleeping engines retain their runtime state; sleep is presented separately.
		if inst.State == proto.InstRunning && !inst.Headless && !incomplete[inst.UUID] {
			return state
		}
	}
	for _, inst := range insts {
		if inst.State == proto.InstRunning && !InstanceNodeOnline(rows, inst.NodeID) {
			return "offline"
		}
	}
	return state
}

// scheduleStalled reports whether a deployment with no instance has been
// refused a placement for longer than the configured timeout.
//
// It measures from the FIRST scheduling entry, not the latest: entries are
// rewritten once per timeout window, so the newest one is never older than a
// window and would report "not stalled" forever.
//
// It also does not measure from deployment creation. A 66 GiB download
// legitimately takes tens of minutes, and counting from creation would call a
// perfectly healthy download a stall - the same confusion this whole mechanism
// exists to remove.
func scheduleStalled(rows Rows, deploymentID uint, instanceCount int) bool {
	if instanceCount > 0 {
		return false
	}
	ev, found := rows.FirstScheduleEvent(deploymentID)
	if !found {
		return false
	}
	return time.Since(ev.CreatedAt) >= rows.PendingTimeout()
}

// DeriveRuntimeState is planning.DeriveRuntimeState over the model progress
// a deployment DTO shows.
func DeriveRuntimeState(desired store.DeploymentState, instances []store.Instance, modelProgress []dto.ModelNodePresence, scheduleStalled bool) string {
	states := make([]string, len(modelProgress))
	for i, mp := range modelProgress {
		states[i] = mp.State
	}
	return planning.DeriveRuntimeState(desired, instances, states, scheduleStalled)
}

func ModelNodePresence(mf *store.ModelFile, node *store.Node) dto.ModelNodePresence {
	progress := 0.0
	if mf.SizeBytes > 0 {
		progress = float64(mf.Downloaded) / float64(mf.SizeBytes)
		if progress > 1 {
			progress = 1
		}
	}
	nodeUUID := ""
	if node != nil {
		nodeUUID = node.UUID
	}
	return dto.ModelNodePresence{NodeUUID: nodeUUID, NodeName: nodeDisplayName(node), State: mf.State, Path: mf.Path, Progress: progress, SizeBytes: mf.SizeBytes, Downloaded: mf.Downloaded, TransferredBytes: mf.TransferredBytes, RateBps: mf.RateBPS, RateLimitKiB: mf.RateLimit, LastRateLimitKiB: mf.LastRateLimit, Acquisition: mf.Acquisition, TransferID: mf.TransferID, TransferErrorCode: mf.TransferErrorCode, Message: mf.Message}
}

// TransferProgress is the fraction of a model file transferred so far.
func TransferProgress(done, size int64) float64 {
	if size <= 0 {
		return 0
	}
	return math.Min(float64(done)/float64(size), 1)
}

// RuntimeStates applies the deployment read policy to an already
// loaded inventory. Deployments waiting for their first instance need model
// preparation and scheduling facts; those facts are loaded once for the batch.
func RuntimeStates(st *store.Store, preparation ModelPreparation, deployments []store.Deployment, instances []store.Instance) (map[uint]string, error) {
	byDeployment := make(map[uint][]store.Instance)
	for _, instance := range instances {
		byDeployment[instance.DeploymentID] = append(byDeployment[instance.DeploymentID], instance)
	}
	states := make(map[uint]string, len(deployments))
	var preparing []store.Deployment
	var running []store.Deployment
	for _, deployment := range deployments {
		current := byDeployment[deployment.ID]
		if len(current) != 0 || deployment.State == store.DeployStopped || deployment.State == store.DeployDeleting {
			states[deployment.ID] = DeriveRuntimeState(deployment.State, current, nil, false)
			if states[deployment.ID] == "running" {
				running = append(running, deployment)
			}
			continue
		}
		preparing = append(preparing, deployment)
	}
	if len(running) > 0 {
		nodes, err := st.ListNodes()
		if err != nil {
			return nil, err
		}
		byID := make(map[uint]*store.Node, len(nodes))
		for i := range nodes {
			byID[nodes[i].ID] = &nodes[i]
		}
		// This lookup only serves runtime availability; preparation reads remain batched below.
		rows := &ListRows{nodes: byID}
		for i := range running {
			d := &running[i]
			current := byDeployment[d.ID]
			// Runtime derivation cannot need scheduling facts when instances exist.
			states[d.ID] = RuntimeState(rows, d, current, nil)
		}
	}
	if len(preparing) == 0 {
		return states, nil
	}
	rows, err := NewListRows(st, preparation, preparing, nil)
	if err != nil {
		return nil, err
	}
	for i := range preparing {
		deployment := &preparing[i]
		states[deployment.ID] = RuntimeState(rows, deployment, nil, RecordedModelProgress(rows, deployment))
	}
	return states, nil
}
