package api

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// presenceLookup holds what showing where models' files are reads, loaded in
// a fixed number of queries whatever the number of models, nodes or
// storages: the files, the storages, how many nodes each storage is ready and
// writable on, and the nodes' identities.
type presenceLookup struct {
	preparationReads
	files    map[uint][]store.ModelFile
	storages map[uint]*store.Storage
	ready    map[uint]store.StorageReadyCount
	nodes    map[uint]*store.Node
}

// newPresenceLookup loads the presence facts of one model, or of every model
// when modelID is nil.
func (a *apiHandlers) newPresenceLookup(modelID *uint) presenceLookup {
	l := presenceLookup{preparationReads: preparationReads{a.d.Preparation}, files: map[uint][]store.ModelFile{}, storages: map[uint]*store.Storage{}, nodes: map[uint]*store.Node{}}
	var files []store.ModelFile
	if modelID != nil {
		files, _ = a.d.Store.ModelFilesForModel(*modelID)
	} else {
		files, _ = a.d.Store.ListModelFiles()
	}
	for _, mf := range files {
		l.files[mf.ModelID] = append(l.files[mf.ModelID], mf)
	}
	if storages, err := a.d.Store.ListStorages(); err == nil {
		for i := range storages {
			l.storages[storages[i].ID] = &storages[i]
		}
	}
	l.ready, _ = a.d.Store.StorageReadyCounts()
	if nodes, err := a.d.Store.NodeIdentities(); err == nil {
		for i := range nodes {
			l.nodes[nodes[i].ID] = &nodes[i]
		}
	}
	return l
}

// fillModelPresence lists where a model's files are: one entry per node for
// local storages and node caches, one entry per shared storage.
func (a *apiHandlers) fillModelPresence(d *dto.Model, modelID uint) {
	l := a.newPresenceLookup(&modelID)
	l.fill(d, modelID)
}

func (l presenceLookup) fill(d *dto.Model, modelID uint) {
	mfs := l.files[modelID]
	cached := map[uint]int{}
	for _, mf := range mfs {
		if mf.NodeID != 0 && proto.ModelFileStateReady(mf.State) {
			cached[mf.StorageID]++
		}
	}
	for _, mf := range mfs {
		st := l.storages[mf.StorageID]
		if observed, ok := l.modelCopyObservation(&mf); ok {
			mf.Downloaded, mf.SizeBytes, mf.RateBPS, mf.TransferredBytes = observed.BytesDone, observed.BytesTotal, observed.RateBytesPerS, observed.TransferBytesDone
		}
		if mf.NodeID == 0 {
			if st == nil {
				continue
			}
			count := l.ready[st.ID]
			item := dto.ModelStoragePresence{
				StorageID: st.ID, StorageName: st.Name, StorageType: st.Type, Path: mf.Path, State: mf.State,
				Acquisition: mf.Acquisition, Message: mf.Message, Revision: mf.Revision, SizeBytes: mf.SizeBytes,
				Downloaded: mf.Downloaded, Progress: transferProgress(mf.Downloaded, mf.SizeBytes), RateBps: mf.RateBPS,
				CachedNodes: cached[st.ID], ReadyNodes: count.Ready,
				Writable: store.StorageAcceptsWritesWith(st, count.Writable) == nil,
			}
			if writer := l.nodes[mf.WriterNodeID]; mf.WriterNodeID != 0 && writer != nil {
				item.WriterNode = nodeDisplayName(writer)
			}
			d.Storages = append(d.Storages, item)
			continue
		}
		node := l.nodes[mf.NodeID]
		if node == nil {
			continue
		}
		presence := dto.ModelNodePresence{
			NodeUUID: node.UUID, NodeName: nodeDisplayName(node), State: mf.State, Path: mf.Path, Revision: mf.Revision,
			Progress: transferProgress(mf.Downloaded, mf.SizeBytes), SizeBytes: mf.SizeBytes, Downloaded: mf.Downloaded,
			TransferredBytes: mf.TransferredBytes, RateBps: mf.RateBPS, RateLimitKiB: mf.RateLimit, LastRateLimitKiB: mf.LastRateLimit,
			Acquisition: mf.Acquisition, TransferID: mf.TransferID, TransferErrorCode: mf.TransferErrorCode, Message: mf.Message, LastUsedAt: utcTimePtr(mf.LastUsedAt),
		}
		if st != nil {
			presence.StorageID, presence.StorageName, presence.StorageType = st.ID, st.Name, st.Type
			presence.Cache = st.Type == proto.StorageNFSCache
			presence.Deletable = store.NodeFilesDeletable(st)
		}
		if source := l.nodes[mf.TransferSourceNodeID]; mf.TransferSourceNodeID != 0 && source != nil {
			presence.SourceNodeUUID = source.UUID
			presence.SourceNodeName = nodeDisplayName(source)
		}
		d.Nodes = append(d.Nodes, presence)
	}
}
