package api

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// deploymentPreparationSummary is the list row's one-line preparation: the
// first model still being prepared for an active deployment.
func deploymentPreparationSummary(dep *store.Deployment, progress []dto.ModelNodePresence) *dto.ModelNodePresence {
	if dep.State != store.DeployActive {
		return nil
	}
	for i := range progress {
		if !proto.ModelFileStateReady(progress[i].State) {
			summary := progress[i]
			return &summary
		}
	}
	return nil
}
