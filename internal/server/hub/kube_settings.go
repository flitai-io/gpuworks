package hub

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The settings of a Kubernetes cluster entry that are not in its DaemonSet
// manifest reach the agents here (ADR 0052): the idle-return grace period and
// the NVIDIA RuntimeClass as a kube.settings message, the engine port range
// inside the node's network config. Editing them applies nothing and
// restarts nothing; an online agent has the new value as soon as the message
// arrives, an offline one gets it when it connects.

// pushKubeSettings sends the entry's current kube.settings to the node;
// clusterID 0 (any node that is not a Kubernetes node) sends nothing, so
// other nodes see no change at all. The store read and the write are one
// step under kubeSettingsMu: the initial replay and an edit's push of the same
// session cannot cross and leave the older value last.
func (s *Session) pushKubeSettings(clusterID uint) error {
	if clusterID == 0 {
		return nil
	}
	s.kubeSettingsMu.Lock()
	defer s.kubeSettingsMu.Unlock()
	select {
	case <-s.closed:
		return errSessionClosed
	default:
	}
	live, err := s.hub.store.KubeClusterLiveSettings(clusterID)
	if err != nil {
		return err
	}
	return s.send(proto.TKubeSettings, 0, proto.KubeSettings{IdleReturnSeconds: live.IdleReturnSeconds, RuntimeClass: live.RuntimeClass})
}

// useKubeEnginePorts makes the engine port range a Kubernetes node's agent
// reports its listeners in the entry's, not the server's: the server
// allocates the node's instance ports from it (controller.pickPorts).
func (s *Session) useKubeEnginePorts(node *store.Node, cfg *proto.NetworkConfig) error {
	if node.KubeClusterID == 0 {
		return nil
	}
	live, err := s.hub.store.KubeClusterLiveSettings(node.KubeClusterID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // the entry is gone: the server's range, as before
	}
	if err != nil {
		return err
	}
	if r, err := proto.ParsePortRange(live.EnginePorts); err == nil {
		cfg.EnginePorts = &r
	}
	return nil
}

// PushKubeSettings applies an edit of an entry's live settings to its agents
// that are online: each gets the entry's kube.settings and its network config
// again. It returns once both are queued; a slow or stalled agent holds up
// only itself.
func (h *Hub) PushKubeSettings(clusterID uint) {
	uuids, err := h.store.KubeNodeUUIDs(clusterID)
	if err != nil {
		h.log.Warn("kubernetes settings push: listing the entry's nodes failed", "cluster", clusterID, "err", err)
		return
	}
	for _, uuid := range uuids {
		s := h.session(uuid)
		if s == nil {
			continue
		}
		h.RequestNetworkSync(uuid)
		h.trackSessionWork(func() {
			if err := s.pushKubeSettings(clusterID); err != nil && !errors.Is(err, errSessionClosed) {
				h.log.Warn("kubernetes settings push failed", "node", uuid, "err", err)
			}
		})
	}
}
