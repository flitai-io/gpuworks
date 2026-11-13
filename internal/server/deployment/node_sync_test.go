package deployment

// Tests supply only the sync operation their request is allowed to use.
type nodeSyncCallbacks struct {
	full    func(string) error
	network func(string)
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
