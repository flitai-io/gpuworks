package api

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// nodeDrainFeedback reads the deployment's existing scheduler observations and
// failed attempts. Keep their time visible: a historical refusal is useful
// context, but does not prove that a fresh scheduling attempt will fail.
func (a *apiHandlers) nodeDrainFeedback(deploymentID, nodeID uint, source store.Instance) (dto.NodeReplica, error) {
	out := dto.NodeReplica{}
	event, found, err := a.d.Store.LatestScheduleEvent(deploymentID)
	if err != nil {
		return out, err
	}
	if found && !event.CreatedAt.Before(source.CreatedAt) {
		var data struct {
			Scheduler *dto.ScheduleReason `json:"scheduler"`
		}
		if json.Unmarshal([]byte(event.Data), &data) == nil && data.Scheduler != nil {
			at := event.CreatedAt
			out.DrainReason, out.DrainReasonAt = data.Scheduler, &at
		}
	}
	failed, err := a.d.Store.LatestDrainFailure(deploymentID, nodeID, source.ID)
	if err != nil || failed == nil {
		return out, err
	}
	out.DrainMessage, out.DrainFailure = failed.Message, instanceRuntimeFailure(failed)
	return out, nil
}
