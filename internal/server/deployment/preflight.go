package deployment

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

type DistributedPreflightRequest struct {
	NodeUUIDs  []string `json:"node_uuids"`
	Port       int      `json:"port"`
	MasterAddr string   `json:"master_addr"`
	Bandwidth  bool     `json:"bandwidth"`
	Seconds    int      `json:"seconds"`
}

func ValidatePreflightRequest(req *DistributedPreflightRequest) error {
	if len(req.NodeUUIDs) < 2 {
		return productError(messages.ErrorPreflightMinimumNodes, "at least two nodes are required", map[string]any{"minimum": 2})
	}
	if len(req.NodeUUIDs) > 16 {
		return productError(messages.ErrorPreflightMaximumNodes, "at most 16 nodes are supported", map[string]any{"maximum": 16})
	}
	seen := make(map[string]struct{}, len(req.NodeUUIDs))
	for _, uuid := range req.NodeUUIDs {
		trimmed := strings.TrimSpace(uuid)
		if trimmed == "" {
			return productError(messages.ErrorPreflightNodeRequired, "node UUID must not be empty", nil)
		}
		if trimmed != uuid {
			return productError(messages.ErrorPreflightNodeWhitespace, "node UUID must not contain surrounding whitespace", map[string]any{"node": uuid})
		}
		if _, duplicate := seen[uuid]; duplicate {
			return productError(messages.ErrorPreflightDuplicateNode, fmt.Sprintf("duplicate node UUID %s", uuid), map[string]any{"node": uuid})
		}
		seen[uuid] = struct{}{}
	}
	if req.Port == 0 {
		req.Port = deploy.DefaultMasterPort
	}
	if req.Port < 1024 || req.Port > 65535 {
		return productError(messages.ErrorPreflightPortInvalid, "preflight port must be between 1024 and 65535", map[string]any{"minimum": 1024, "maximum": 65535})
	}
	if req.Seconds == 0 {
		req.Seconds = 3
	}
	if req.Seconds < 1 || req.Seconds > 10 {
		return productError(messages.ErrorPreflightDurationInvalid, "bandwidth duration must be between 1 and 10 seconds", map[string]any{"minimum": 1, "maximum": 10})
	}
	return nil
}
