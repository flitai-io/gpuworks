package api

// NodeSync separates the full desired-state replay a request waits for from
// the ones it only asks for. SyncNode replays to one node and returns when it
// is written; RequestSync and RequestNetworkSync schedule a replay (full, or of
// the network policy alone) on the node's own coalescing worker and return at
// once. A change that reaches many nodes asks: saving it must not wait on every
// agent's control stream in turn. A change for one node may wait for it, and
// does where the caller reports the result or orders a later step after the
// replay.
type NodeSync interface {
	SyncNode(uuid string) error
	RequestSync(uuid string)
	RequestNetworkSync(uuid string)
	// PushKubeSettings hands a Kubernetes entry's live settings (idle return,
	// RuntimeClass, engine ports) to its online agents without waiting for them.
	PushKubeSettings(clusterID uint)
}

func (a *apiHandlers) requestNetworkSync(uuid string) {
	if a.d.NodeSync != nil && (a.d.Online == nil || a.d.Online(uuid)) {
		a.d.NodeSync.RequestNetworkSync(uuid)
	}
}

// requestSyncNodes asks for a full replay to each of the nodes that is online.
func (a *apiHandlers) requestSyncNodes(uuids []string) {
	if a.d.NodeSync == nil {
		return
	}
	for _, uuid := range uuids {
		if a.d.Online == nil || a.d.Online(uuid) {
			a.d.NodeSync.RequestSync(uuid)
		}
	}
}

// requestSyncAllNodes asks for a full replay to every online node.
func (a *apiHandlers) requestSyncAllNodes() {
	if a.d.NodeSync == nil {
		return
	}
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		return
	}
	uuids := make([]string, 0, len(nodes))
	for i := range nodes {
		uuids = append(uuids, nodes[i].UUID)
	}
	a.requestSyncNodes(uuids)
}
