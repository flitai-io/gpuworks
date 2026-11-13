package api

// NodeSync separates full desired-state updates from asynchronous network-only
// notifications. Saving proxy settings must not wait on an agent's control stream.
type NodeSync interface {
	SyncNode(uuid string) error
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
