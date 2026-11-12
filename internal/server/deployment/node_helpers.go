package deployment

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func uniqueWarnings(values []messages.Warning) []messages.Warning {
	seen := make(map[string]struct{}, len(values))
	out := make([]messages.Warning, 0, len(values))
	for _, value := range values {
		value.Code = strings.TrimSpace(value.Code)
		if value.Code == "" {
			continue
		}
		encoded, _ := json.Marshal(value)
		key := string(encoded)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func nodeEngineTarget(node *store.Node) (arch, archID, vendor string) {
	if node == nil {
		return "", "", ""
	}
	owned := node.OwnedGPUs()
	if len(owned) == 0 {
		return "", "", ""
	}
	return gpuEngineTarget(node, owned[0])
}

func gpuEngineTarget(node *store.Node, g store.GPU) (arch, archID, vendor string) {
	a := node.ArchOf(g)
	return a.Family, a.ID, node.VendorOf(g)
}
