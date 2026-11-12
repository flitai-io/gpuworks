package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func preserveEngineModelName(req *dto.DeployRequest) {
	req.ServedName = strings.TrimSpace(req.ServedName)
	if req.ServedName == "" {
		if req.QuickParams == nil {
			req.QuickParams = make(map[string]any)
		}
		req.QuickParams["served_model_name"] = ""
	} else {
		delete(req.QuickParams, "served_model_name")
	}
}
