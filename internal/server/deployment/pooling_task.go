package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// Creation preflights before resolving/registering template weights. Defer
// contract-derived defaults until the existing resolver has selected the actual
// model (including ready mirrors and primary component substitutions).
func (s *Service) materializeCreateLaunch(engine string, req *dto.DeployRequest, selection templateSelection, cards []deploy.CardFacts, version string) (deploy.Materialized, error) {
	selection.APIContract = nil
	return materializeDeployRequest(engine, req, selection, cards, version)
}
