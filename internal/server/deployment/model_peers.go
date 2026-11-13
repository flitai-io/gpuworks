package deployment

import (
	"errors"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// modelCopyPeer returns a qualified automatic source, not a verified network
// connection. The actual copy still chooses its path and handles cooldowns at
// runtime. The source facts match the controller's local/shared peer rules.
func (s *Service) modelCopyPeer(modelID uint, target *store.Node) (string, error) {
	files, err := s.d.Store.ModelFilesForModel(modelID)
	if err != nil {
		return "", err
	}
	ids := map[uint]bool{}
	for _, file := range files {
		if !file.Ready() || strings.TrimSpace(file.Path) == "" {
			continue
		}
		if file.NodeID != 0 {
			storage, err := s.d.Store.StorageByID(file.StorageID)
			if err != nil {
				return "", err
			}
			if storage.Type == proto.StorageLocal {
				ids[file.NodeID] = true
			}
			continue
		}
		states, err := s.d.Store.StorageNodeStates(file.StorageID)
		if err != nil {
			return "", err
		}
		for _, state := range states {
			if state.State != proto.StorageStateReady {
				continue
			}
			if _, ok := store.SharedHostPath(state.Path, file.Path); ok {
				ids[state.NodeID] = true
			}
		}
	}
	delete(ids, target.ID)
	keys := make([]uint, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	nodes, err := s.d.Store.NodesForLabelsByID(keys, true)
	if err != nil {
		return "", err
	}
	sources := nodes[:0]
	for _, node := range nodes {
		if s.d.Online == nil || s.d.Online(node.UUID) {
			sources = append(sources, node)
		}
	}
	peers, err := planning.SyncPeersFor(s.d.Store, s.networks().Resolve, target, sources)
	if err != nil || len(peers) == 0 {
		return "", err
	}
	var blocked []string
	var blockedUUID string
	for _, peer := range peers {
		var source *store.Node
		for i := range sources {
			if sources[i].ID == peer.NodeID {
				source = &sources[i]
				break
			}
		}
		if source == nil {
			continue
		}
		if _, err := s.networks().StoragePath(peer.NodeID, target.ID); err != nil {
			if errors.Is(err, netplan.ErrNoStoragePath) {
				blocked = append(blocked, nodeDisplayName(source)+" ("+source.UUID+")")
				blockedUUID = source.UUID
				continue // A known policy refusal is not a facts-query failure.
			}
			return "", err
		}
		return nodeDisplayName(source), nil
	}
	if len(blocked) > 0 {
		params := map[string]any{
			"source": strings.Join(blocked, ", "), "target": nodeDisplayName(target),
			"node": nodeDisplayName(target), "node_id": target.ID, "node_uuid": target.UUID,
			"reason": "no_storage_path", "observer": "control_plane",
		}
		if len(blocked) == 1 {
			params["source_node"] = blockedUUID
		}
		return "", messages.WrapProductError(messages.ErrorNoStoragePath,
			"The candidate model holders share no network permitted for storage sync with the target node", params, netplan.ErrNoStoragePath)
	}
	return "", nil
}
