package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// rankImage resolves the runtime image a launch runs on one node: the catalog
// resolution of the requested (or template) image against that node's hardware.
// Evaluate shows it, create preflights and persists it, so every stage judges
// the image the launch will actually use rather than the request's raw field.
func (s *Service) rankImage(engine string, selection templateSelection, requested string, node *store.Node) deploy.ImageResolution {
	target := deploymentImageTarget(engine, "", node)
	target.Override = requested
	target.TrustedOverride = templateOwnsImage(selection, requested)
	return imagecatalog.Resolve(s.d.Store, target)
}

// admitImagePlatforms checks every concrete rank before downloads or model
// registration can start. An automatic deployment without a node waits until
// its placement is known.
func (s *Service) admitImagePlatforms(engine string, selection templateSelection, requested string, nodes []*store.Node) error {
	for _, node := range nodes {
		if node == nil {
			continue
		}
		resolution := s.rankImage(engine, selection, requested, node)
		if resolution.Blocking {
			code := messages.ErrorAcquisitionImageRequired
			params := map[string]any{"image": resolution.Image, "node": node.UUID}
			if len(resolution.Warnings) > 0 {
				code = resolution.Warnings[0].Code
			}
			return messages.NewProductError(code, "the runtime image is incompatible with the target node", params)
		}
	}
	return nil
}
