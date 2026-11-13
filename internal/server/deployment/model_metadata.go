package deployment

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// FetchModelMeta reads a model's metadata at the addresses the site rules
// give the management node (planning.ServerFetchCandidates); sourceURL is an
// HTTP directory's own address. It is the one way the server reads a
// model's source - registration, refresh, templates and deployment admission
// all come here. Reaching nothing is not an error: the metadata is simply
// empty, and metadataNotice says why where a person is told.
func (s *Service) FetchModelMeta(ctx context.Context, source, repo, revision, subfolder, sourceURL string) (*modelmeta.Meta, error) {
	candidates, err := planning.ServerFetchCandidates(s.d.Store, source, sourceURL)
	if containsProductCode(err, messages.ErrorModelMetadataForbidden) {
		return &modelmeta.Meta{}, nil
	}
	if err != nil {
		return nil, err
	}
	read := s.d.FetchModelMeta
	if read == nil {
		read = s.d.ReadModelMeta
	}
	if read == nil {
		return &modelmeta.Meta{}, nil
	}
	return read(ctx, source, repo, revision, subfolder, candidates)
}
