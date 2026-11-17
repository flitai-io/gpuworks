package deployment

import (
	"errors"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func ResolvePartitionBackend(node *store.Node, limitGiB float64, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if limitGiB <= 0 {
		if requested != "" {
			return "", errors.New("partition_backend requires a positive vram_limit_gib")
		}
		return "", nil
	}
	// A quota that names no backend takes the node's own (docs/how-to/dgpu.md:
	// the platform picks it from the node's hardware). An automatic placement
	// has no node to name a backend for until a candidate is planned, so a
	// default of NVIDIA's refused every Ascend and PPU node in that mode. A
	// named backend is still held to exactly that one.
	if requested == "" {
		if preferred, ok := partitioncap.Preferred(node); ok {
			return preferred.Backend, nil
		}
		// Nothing is ready: refuse with the node's own backend's reason.
		requested = accelerator.BackendNVIDIALogical
		if capabilities := partitioncap.ForNode(node); len(capabilities) > 0 {
			requested = capabilities[0].Backend
		}
	}
	if err := partitioncap.ValidateReady(node, requested); err != nil {
		return "", err
	}
	return requested, nil
}

// distributedPartitionRefusal reports that the backend resolved on a group's
// first node is unavailable on another member. refusal is that member's
// ValidateReady error: its reason code (reason_code) and params ride along,
// so a client can say why, and node names the member.
func distributedPartitionRefusal(node *store.Node, backend string, refusal error) error {
	params := map[string]any{"node": node.UUID, "backend": backend}
	var product *messages.ProductError
	if errors.As(refusal, &product) {
		for key, value := range product.Params() {
			params[key] = value
		}
		params["reason_code"] = product.Code()
	}
	return wrapProductError(messages.ErrorDistributedPartitionBackendUnavailable, fmt.Sprintf("node %s: %s", node.UUID, refusal), params, refusal)
}
