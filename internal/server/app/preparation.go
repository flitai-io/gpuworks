package app

import (
	"context"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/controller"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// modelPreparation gives the API model preparation where it is produced: copy
// frames in the hub, placements, evictions and cache retries in the controller.
type modelPreparation struct {
	hub  *hub.Hub
	ctrl *controller.Controller
}

func (p modelPreparation) ModelCopyObservation(file *store.ModelFile) (proto.ModelFileStatus, bool) {
	return p.hub.ModelCopyObservation(file)
}

func (p modelPreparation) CachePhase(ref store.FileRef) string { return p.ctrl.CachePhase(ref) }
func (p modelPreparation) PreparationNodes(dep *store.Deployment) []uint {
	return p.ctrl.Placements().Nodes(dep)
}
func (p modelPreparation) PreparationGeneration() uint64 { return p.ctrl.Placements().Generation() }

func (p modelPreparation) RetryCacheCopy(ctx context.Context, ref store.FileRef) error {
	return p.ctrl.RetryCacheCopy(ctx, ref)
}
