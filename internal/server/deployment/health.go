package deployment

import (
	"context"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func HealthThresholdError(seconds int) error {
	return productError(messages.ErrorFieldInvalid, "health_check_threshold_sec is out of range", map[string]any{
		"field":  "health_check_threshold_sec",
		"detail": fmt.Sprintf("health_check_threshold_sec must be between %d and %d seconds (got %d)", store.HealthCheckThresholdSecLo, store.HealthCheckThresholdSecHi, seconds),
	})
}

func HealthUncheckableError() error {
	return productError(messages.ErrorFieldInvalid, "the model serves neither chat nor embeddings", map[string]any{
		"field":  "health_check",
		"detail": "the health check sends a chat or an embedding request; this model serves neither",
	})
}

// ValidateHealthCheckThreshold gives evaluation and create the same rule for
// a requested threshold; 0 asks for the default.
func ValidateHealthCheckThreshold(req *dto.DeployRequest) error {
	if req.HealthCheckThresholdSec == 0 || store.ValidHealthCheckThresholdSec(req.HealthCheckThresholdSec) {
		return nil
	}
	return HealthThresholdError(req.HealthCheckThresholdSec)
}

// HealthCheckable reports whether a deployment of model can be checked: it
// serves chat or embeddings.
func HealthCheckable(model *store.Model) bool {
	if model == nil {
		return false
	}
	_, ok := inferenceapi.ModelContract(model).CheckOperation()
	return ok
}

// CreateHealthCheck is the health check a new deployment starts with: as
// the request says, and on when it says nothing and the model serves chat
// or embeddings.
func CreateHealthCheck(req *dto.DeployRequest, model *store.Model) (bool, int, error) {
	threshold := req.HealthCheckThresholdSec
	if threshold == 0 {
		threshold = store.DefaultHealthCheckThresholdSec
	}
	checkable := HealthCheckable(model)
	if req.HealthCheck == nil {
		return checkable, threshold, nil
	}
	if *req.HealthCheck && !checkable {
		return false, threshold, HealthUncheckableError()
	}
	return *req.HealthCheck, threshold, nil
}

// draftHealthCheckable is the answer to "can a deployment of this draft be
// checked" that create enforces (CreateHealthCheck) and evaluation reports: it
// judges the candidate contract the deployment will get, so a model whose
// operations are still empty is judged by the operations its template
// declares, exactly as the create seeds them.
func (s *Service) draftHealthCheckable(selection *templateSelection, model *store.Model) (bool, error) {
	candidate, _, err := s.initTemplateModelInferenceConfig(selection, model)
	if err != nil {
		return false, err
	}
	return HealthCheckable(candidate), nil
}

// noteHealthCheckableOf records draftHealthCheckable on the evaluation; a
// contract that cannot be built leaves the answer absent (unknown).
func (s *Service) noteHealthCheckableOf(resp *dto.EvaluateResponse, selection *templateSelection, model *store.Model) {
	if checkable, err := s.draftHealthCheckable(selection, model); err == nil {
		resp.HealthCheckable = &checkable
	}
}

// noteHealthCheckable resolves the draft's model on a copy of the selection
// (resolution binds components) for an evaluation that stopped on a field
// error before it got to the model.
func (s *Service) noteHealthCheckable(ctx context.Context, resp *dto.EvaluateResponse, req *dto.DeployRequest, selection templateSelection) {
	selection.Components = append([]deploy.ComponentBinding(nil), selection.Components...)
	model, err := s.resolveDeploymentModelFromTemplate(ctx, req.ModelID, &selection, req.TemplateID)
	if err != nil {
		return
	}
	s.noteHealthCheckableOf(resp, &selection, model)
}
