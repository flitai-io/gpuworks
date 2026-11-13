package hub

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// RequestNetworkSync schedules the latest persisted network policy without
// making an HTTP request wait for an agent, on the session's own coalescing
// worker (sync_worker.go), so a stalled peer cannot hold up other nodes or
// accumulate goroutines.
func (h *Hub) RequestNetworkSync(uuid string) {
	if s := h.session(uuid); s != nil {
		s.request(&s.networkSync, s.pushNetwork, "network policy sync")
	}
}

func (s *Session) pushNetwork() error {
	select {
	case <-s.closed:
		return errSessionClosed
	default:
	}
	if !s.HasCap(proto.CapNetworkUpdate) {
		// Older agents require a full replay, isolated on their session worker.
		return s.pushInitialState()
	}
	s.fullPushMu.Lock()
	defer s.fullPushMu.Unlock()
	select {
	case <-s.closed:
		return errSessionClosed
	default:
	}
	cfg, err := s.loadNetworkConfig()
	if err != nil {
		return err
	}
	return s.send(proto.TStateUpdate, 0, proto.StateUpdate{Network: &cfg})
}

// Limit database work during a fleet-wide change, releasing capacity before
// transport writes so a stalled peer cannot hold up other policy snapshots.
func (s *Session) loadNetworkConfig() (proto.NetworkConfig, error) {
	select {
	case s.hub.pushLimiter <- struct{}{}:
	case <-s.closed:
		return proto.NetworkConfig{}, errSessionClosed
	}
	defer func() { <-s.hub.pushLimiter }()
	select {
	case <-s.closed:
		return proto.NetworkConfig{}, errSessionClosed
	default:
	}
	node, err := s.hub.store.NodeByUUID(s.NodeUUID)
	if err != nil {
		return proto.NetworkConfig{}, err
	}
	selection, err := planning.PullSettingsFor(s.hub.store, s.hub.nodeResolver(), node)
	if err != nil {
		return proto.NetworkConfig{}, err
	}
	cfg := selection.Network()
	if err := s.completeNetworkConfig(node, &cfg); err != nil {
		return proto.NetworkConfig{}, err
	}
	return cfg, nil
}

func (s *Session) completeNetworkConfig(node *store.Node, cfg *proto.NetworkConfig) error {
	// Server-derived values are part of the policy too; omitting them would
	// clear serving listeners on a proxy-only change.
	cfg.ServingIP = s.hub.networks.ServingIP(node.ID)
	if s.hub.enginePorts != nil {
		ports := s.hub.enginePorts()
		cfg.EnginePorts = &ports
	}
	return s.useKubeEnginePorts(node, cfg)
}
