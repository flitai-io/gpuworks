package planning

import (
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// OnlineStorageNodes are the online nodes on which the storage is ready to
// read, or to write when write is set. online reports a node's agent
// connected; nil takes every node the store names.
func OnlineStorageNodes(st *store.Store, online func(uuid string) bool, row *store.Storage, write bool) ([]*store.Node, error) {
	ids, err := st.ReadyStorageNodes(row.ID, write)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Node, 0, len(ids))
	for _, id := range ids {
		node, err := st.NodeByID(id)
		if err != nil || (online != nil && !online(node.UUID)) {
			continue
		}
		out = append(out, node)
	}
	return out, nil
}

// PickStorageNode is the first of OnlineStorageNodes that accept takes (nil
// takes any), or the product error that says none can serve the storage.
func PickStorageNode(st *store.Store, online func(uuid string) bool, row *store.Storage, write bool, accept func(*store.Node) bool) (*store.Node, error) {
	nodes, err := OnlineStorageNodes(st, online, row, write)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		if accept == nil || accept(node) {
			return node, nil
		}
	}
	if write {
		return nil, messages.NewProductError(messages.ErrorStorageNotWritable, "no online node can write to storage "+row.Name, map[string]any{"storage": row.Name})
	}
	return nil, messages.NewProductError(messages.ErrorStorageUnavailableOnNode, "the storage is not ready on any online node", map[string]any{"storage": row.Name})
}
