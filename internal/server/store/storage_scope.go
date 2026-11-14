package store

import "gpuworks.ai/gpuworks/internal/proto"

// Which storages apply to which nodes, with each node's overrides merged in.

// EffectiveStorage is one storage as it applies to one node.
type EffectiveStorage struct {
	Storage       Storage
	Path          string
	CachePath     string
	CacheMaxBytes int64
	Existing      bool
}

// StoragesForNode lists the storages that apply to a node, with that node's
// overrides merged in.
func (s *Store) StoragesForNode(node *Node) ([]EffectiveStorage, error) {
	if node == nil {
		return nil, nil
	}
	storages, err := s.ListStorages()
	if err != nil {
		return nil, err
	}
	// The node's own targets and the label targets (node_id 0) are all that
	// can apply to it.
	var targets []StorageNode
	if err := s.reader().Where("node_id IN ?", []uint{0, node.ID}).Find(&targets).Error; err != nil {
		return nil, err
	}
	labels, _ := NodeLabels(node.Labels)
	byStorage := map[uint][]StorageNode{}
	for _, target := range targets {
		byStorage[target.StorageID] = append(byStorage[target.StorageID], target)
	}
	var out []EffectiveStorage
	for _, storage := range storages {
		if eff, ok := effectiveStorage(storage, byStorage[storage.ID], node.ID, labels); ok {
			out = append(out, eff)
		}
	}
	return out, nil
}

// StorageScopes lists the storages that apply to each of nodes, by node id,
// reading the storages and their targets once.
func (s *Store) StorageScopes(nodes []*Node) (map[uint][]Storage, error) {
	out := map[uint][]Storage{}
	if len(nodes) == 0 {
		return out, nil
	}
	storages, err := s.ListStorages()
	if err != nil {
		return nil, err
	}
	var targets []StorageNode
	if err := s.reader().Find(&targets).Error; err != nil {
		return nil, err
	}
	byStorage := map[uint][]StorageNode{}
	for _, target := range targets {
		byStorage[target.StorageID] = append(byStorage[target.StorageID], target)
	}
	for _, node := range nodes {
		if node == nil {
			continue
		}
		labels, _ := NodeLabels(node.Labels)
		for _, storage := range storages {
			if _, ok := effectiveStorage(storage, byStorage[storage.ID], node.ID, labels); ok {
				out[node.ID] = append(out[node.ID], storage)
			}
		}
	}
	return out, nil
}

// EffectiveStorageForNode is StoragesForNode for one storage; ok is false when
// the storage does not apply to the node.
func (s *Store) EffectiveStorageForNode(storage *Storage, node *Node) (EffectiveStorage, bool, error) {
	if storage == nil || node == nil {
		return EffectiveStorage{}, false, nil
	}
	var targets []StorageNode
	if err := s.reader().Where("storage_id = ? AND node_id IN ?", storage.ID, []uint{0, node.ID}).Order("id asc").Find(&targets).Error; err != nil {
		return EffectiveStorage{}, false, err
	}
	labels, _ := NodeLabels(node.Labels)
	eff, ok := effectiveStorage(*storage, canonicalStorageNodes(targets), node.ID, labels)
	return eff, ok, nil
}

func effectiveStorage(storage Storage, targets []StorageNode, nodeID uint, labels map[string]string) (EffectiveStorage, bool) {
	eff := EffectiveStorage{Storage: storage, Path: storage.Path, CachePath: storage.CachePath, CacheMaxBytes: storage.CacheMaxBytes}
	applies := storage.AllNodes
	for _, target := range targets {
		if target.NodeID != 0 && target.NodeID == nodeID {
			applies = applies || !target.OverrideOnly
			if target.Path != "" {
				eff.Path = target.Path
			}
			if target.CachePath != "" {
				eff.CachePath = target.CachePath
			}
			if target.CacheMaxBytes != nil {
				eff.CacheMaxBytes = *target.CacheMaxBytes
			}
			eff.Existing = target.Existing
			continue
		}
		if target.NodeID == 0 && target.LabelKey != "" {
			if value, ok := labels[target.LabelKey]; ok && (target.LabelValue == "" || value == target.LabelValue) {
				applies = true
			}
		}
	}
	return eff, applies
}

// NodesForStorage lists the nodes a storage applies to.
func (s *Store) NodesForStorage(storage *Storage) ([]Node, error) {
	nodes, err := s.FleetNodes()
	if err != nil {
		return nil, err
	}
	targets, err := s.StorageNodes(storage.ID)
	if err != nil {
		return nil, err
	}
	var out []Node
	for i := range nodes {
		if StorageAppliesTo(storage, targets, &nodes[i]) {
			out = append(out, nodes[i])
		}
	}
	return out, nil
}

// StorageAppliesTo reports whether a storage with these targets applies to
// the node, for a caller that already read both.
func StorageAppliesTo(storage *Storage, targets []StorageNode, node *Node) bool {
	labels, _ := NodeLabels(node.Labels)
	_, ok := effectiveStorage(*storage, targets, node.ID, labels)
	return ok
}

// CountModelsInStorage is how many models have a complete copy in the
// storage, on any node.
func (s *Store) CountModelsInStorage(storageID uint) (int, error) {
	var n int64
	err := s.reader().Model(&ModelFile{}).Where("storage_id = ? AND state IN ?", storageID, []string{proto.MFPresent, proto.MFRegistered}).
		Distinct("model_id").Count(&n).Error
	return int(n), err
}

// StorageHasSelectors reports whether targets contain a membership selector.
// Configuration-only rows cannot make an otherwise empty scope meaningful.
func StorageHasSelectors(targets []StorageNode) bool {
	for _, target := range targets {
		if !target.OverrideOnly && (target.NodeID != 0 || target.LabelKey != "") {
			return true
		}
	}
	return false
}
