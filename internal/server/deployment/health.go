package deployment

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
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
