package app

import (
	"fmt"
	"log/slog"

	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// newPolicyHub applies machine policy before an agent can connect. Closing the
// opened store here matches the other startup failure paths in New.
func newPolicyHub(st *store.Store, log *slog.Logger, raw serverconfig.PeerRelay, networks *network.Service, readPrivateHosts func() []string) (*hub.Hub, error) {
	policy, err := raw.Policy()
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	h := hub.New(st, log)
	if err := h.SetPeerRelayPolicy(policy); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("peer relay policy: %w", err)
	}
	h.SetOutboundPrivateHostsReader(readPrivateHosts)
	h.SetNetworks(networks)
	return h, nil
}
