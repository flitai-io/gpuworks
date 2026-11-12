package deployment

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// admitDeploymentModel validates the source before a Deployment row is created.
// Local and storage facts are checked even without a metadata hook. The
// shared inspection keeps missing external observations explicit.
func (s *Service) admitDeploymentModel(ctx context.Context, model *store.Model, rules store.SourceRules, nodes []*store.Node) error {
	if model == nil || len(nodes) == 0 {
		return nil
	}
	_, err := s.inspectDeploymentModel(ctx, model, rules, nodes, true)
	return err
}
