package api

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func diagnosticsFromError(err error) *dto.DiagnosticReport {
	var explained interface{ Diagnostics() *dto.DiagnosticReport }
	if errors.As(err, &explained) {
		return explained.Diagnostics()
	}
	return nil
}
