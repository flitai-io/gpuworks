package hub

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// RequestNetworkSync schedules the latest persisted network policy without
// making an HTTP request wait for an agent. Each session owns one coalescing
// worker, so a stalled peer cannot hold up other nodes or accumulate goroutines.
func (h *Hub) RequestNetworkSync(uuid string) {
	s := h.session(uuid)
	if s == nil {
		return
	}
	select {
	case <-s.closed:
		return
	default:
	}
	s.networkSyncOnce.Do(func() {
		s.networkSyncNotify = make(chan struct{}, 1)
		go s.runNetworkSync()
	})
	select {
	case s.networkSyncNotify <- struct{}{}:
	default:
	}
}

func (s *Session) runNetworkSync() {
	for {
		select {
		case <-s.closed:
			return
		case <-s.networkSyncNotify:
		}
		select {
		case <-s.closed:
			return
		case <-s.initialPushDone:
		}
		for {
			// Coalesce requests received before this read. A save during the
			// read/write leaves another notification for the next iteration.
			select {
			case <-s.networkSyncNotify:
			default:
			}
			if err := s.pushNetwork(); err == nil {
				break
			}
			select {
			case <-s.closed:
				return
			default:
			}
			// A transient store failure must not lose the only notification.
			// Policy contents can contain proxy credentials; do not log them.
			s.hub.log.Warn("network policy sync failed; retrying", "node", s.NodeUUID)
			timer := time.NewTimer(time.Second)
			select {
			case <-s.closed:
				timer.Stop()
				return
			case <-s.networkSyncNotify:
				timer.Stop()
			case <-timer.C:
			}
		}
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
