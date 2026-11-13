package api

// Tests supply only the sync operation their request is allowed to use.
type nodeSyncCallbacks struct {
	full    func(string) error
	request func(string)
	network func(string)
	kube    func(uint)
}

func (c nodeSyncCallbacks) RequestSync(uuid string) {
	if c.request == nil {
		panic("unexpected requested full node sync")
	}
	c.request(uuid)
}

func (c nodeSyncCallbacks) SyncNode(uuid string) error {
	if c.full == nil {
		panic("unexpected full node sync")
	}
	return c.full(uuid)
}
func (c nodeSyncCallbacks) RequestNetworkSync(uuid string) {
	if c.network == nil {
		panic("unexpected network-only node sync")
	}
	c.network(uuid)
}
func (c nodeSyncCallbacks) PushKubeSettings(clusterID uint) {
	if c.kube == nil {
		panic("unexpected Kubernetes settings push")
	}
	c.kube(clusterID)
}
