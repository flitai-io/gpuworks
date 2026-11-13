package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/proto"
)

func relayEnabled(allowed bool, relays []archiveRelay) bool {
	return allowed && len(relays) > 0 && relays[0] != nil
}

// Only a negotiated, pre-TLS dial failure can switch transport. Authentication,
// archive and confirmation failures retain their original failure semantics.
func sendPeerArchive(ctx context.Context, source, method, transferID string, params any, long, allowed bool, call modelSyncCall, relays []archiveRelay) (json.RawMessage, string, error) {
	raw, err := call(ctx, source, method, params, long)
	if err != nil {
		return nil, "", err
	}
	var result proto.ModelSyncTransferResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, "", err
	}
	if result.DirectDialError == "" {
		return raw, "direct", nil
	}
	if !relayEnabled(allowed, relays) {
		return nil, "", fmt.Errorf("direct peer connection failed: %s", result.DirectDialError)
	}
	raw, err = relays[0](ctx, method, transferID, params)
	if err != nil {
		return nil, "", fmt.Errorf("relay peer connection: %w", err)
	}
	// Unmarshal into a fresh value: omitted fields must not retain direct failure.
	var confirmed proto.ModelSyncTransferResult
	if err := json.Unmarshal(raw, &confirmed); err != nil {
		return nil, "", err
	}
	if confirmed.DirectDialError != "" {
		return nil, "", errors.New("relay returned a direct dial failure")
	}
	return raw, "relay", nil
}
