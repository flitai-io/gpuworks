package store

// ModelSourceSnapshot is a request-scoped, batch-loaded view of the exact
// facts ModelSourceOn uses. Selection remains nodeModelFacts.source.
type ModelSourceSnapshot struct {
	scopes   map[uint][]EffectiveStorage
	states   map[uint]map[uint]*StorageNodeState
	files    map[uint]map[uint]map[uint]*ModelFile // model, node, storage
	storages map[uint]Storage
}

func (s *Store) ModelSourcesFor(modelIDs []uint, nodes []*Node) (*ModelSourceSnapshot, error) {
	out := &ModelSourceSnapshot{scopes: map[uint][]EffectiveStorage{}, states: map[uint]map[uint]*StorageNodeState{}, files: map[uint]map[uint]map[uint]*ModelFile{}, storages: map[uint]Storage{}}
	if len(modelIDs) == 0 || len(nodes) == 0 {
		return out, nil
	}
	nodeIDs := []uint{0}
	for _, node := range nodes {
		nodeIDs = append(nodeIDs, node.ID)
	}
	storages, err := s.ListStorages()
	if err != nil {
		return nil, err
	}
	var files []ModelFile
	if err := s.reader().Where("model_id IN ? AND node_id IN ?", modelIDs, nodeIDs).Find(&files).Error; err != nil {
		return nil, err
	}
	return s.ModelSourcesFrom(nodes, files, storages)
}

// ModelSourcesFrom reuses rows a list has already loaded; only node-scoped
// targets and health need additional reads.
func (s *Store) ModelSourcesFrom(nodes []*Node, files []ModelFile, storages []Storage) (*ModelSourceSnapshot, error) {
	out := &ModelSourceSnapshot{scopes: map[uint][]EffectiveStorage{}, states: map[uint]map[uint]*StorageNodeState{}, files: map[uint]map[uint]map[uint]*ModelFile{}, storages: map[uint]Storage{}}
	if len(nodes) == 0 {
		return out, nil
	}
	nodeIDs := []uint{0}
	for _, node := range nodes {
		nodeIDs = append(nodeIDs, node.ID)
	}
	storageIDs := make([]uint, 0, len(storages))
	for _, storage := range storages {
		storageIDs = append(storageIDs, storage.ID)
	}
	var targets []StorageNode
	// The existing scope indexes begin with storage_id. Include that prefix
	// so a node-scoped read uses them without adding a redundant schema index.
	if err := s.reader().Where("storage_id IN ? AND node_id IN ?", storageIDs, nodeIDs).Find(&targets).Error; err != nil {
		return nil, err
	}
	byStorage := map[uint]map[uint][]StorageNode{}
	for _, target := range targets {
		if byStorage[target.StorageID] == nil {
			byStorage[target.StorageID] = map[uint][]StorageNode{}
		}
		byNode := byStorage[target.StorageID]
		byNode[target.NodeID] = append(byNode[target.NodeID], target)
	}
	labels := make(map[uint]map[string]string, len(nodes))
	for _, node := range nodes {
		labels[node.ID], _ = NodeLabels(node.Labels)
	}
	for _, storage := range storages {
		out.storages[storage.ID] = storage
		byNode := byStorage[storage.ID]
		for _, node := range nodes {
			// Keep the common scope rule, but never scan every other node's
			// explicit targets for each member of a large fleet.
			scoped := append([]StorageNode(nil), byNode[0]...)
			scoped = append(scoped, byNode[node.ID]...)
			if eff, ok := effectiveStorage(storage, scoped, node.ID, labels[node.ID]); ok {
				out.scopes[node.ID] = append(out.scopes[node.ID], eff)
			}
		}
	}
	var states []StorageNodeState
	if err := s.reader().Where("storage_id IN ? AND node_id IN ?", storageIDs, nodeIDs).Find(&states).Error; err != nil {
		return nil, err
	}
	for i := range states {
		row := &states[i]
		if out.states[row.NodeID] == nil {
			out.states[row.NodeID] = map[uint]*StorageNodeState{}
		}
		out.states[row.NodeID][row.StorageID] = row
	}
	for i := range files {
		row := &files[i]
		if out.files[row.ModelID] == nil {
			out.files[row.ModelID] = map[uint]map[uint]*ModelFile{}
		}
		if out.files[row.ModelID][row.NodeID] == nil {
			out.files[row.ModelID][row.NodeID] = map[uint]*ModelFile{}
		}
		out.files[row.ModelID][row.NodeID][row.StorageID] = row
	}
	return out, nil
}

func (s *ModelSourceSnapshot) Source(modelID, nodeID uint, rules SourceRules) ModelSource {
	facts := nodeModelFacts{storages: s.scopes[nodeID], states: s.states[nodeID], shared: s.files[modelID][0], own: s.files[modelID][nodeID]}
	result := facts.source(rules)
	if result.Location.Storage.Name == "" {
		result.Location.Storage = s.storages[result.Location.Storage.ID]
	}
	return result
}

// WithModelFiles is s with the files of modelIDs on nodeIDs replaced by
// files: the same storages and node states, those models' rows on those
// nodes as read again, every other row as it was. s is unchanged.
func (s *ModelSourceSnapshot) WithModelFiles(modelIDs, nodeIDs []uint, files []ModelFile) *ModelSourceSnapshot {
	out := *s
	out.files = make(map[uint]map[uint]map[uint]*ModelFile, len(s.files)+len(modelIDs))
	for id, byNode := range s.files {
		out.files[id] = byNode
	}
	for _, modelID := range modelIDs {
		byNode := make(map[uint]map[uint]*ModelFile, len(s.files[modelID])+len(nodeIDs))
		for nodeID, byStorage := range s.files[modelID] {
			byNode[nodeID] = byStorage
		}
		for _, nodeID := range nodeIDs {
			delete(byNode, nodeID)
		}
		out.files[modelID] = byNode
	}
	for i := range files {
		row := &files[i]
		if out.files[row.ModelID][row.NodeID] == nil {
			out.files[row.ModelID][row.NodeID] = map[uint]*ModelFile{}
		}
		out.files[row.ModelID][row.NodeID][row.StorageID] = row
	}
	return &out
}
