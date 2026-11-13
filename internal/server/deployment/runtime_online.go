package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Read connection facts separately from the agent's last runtime observation.
func InstanceNodeOnline(rows Rows, nodeID uint) bool {
	n := rows.Node(nodeID)
	return n != nil && n.State == store.NodeStateOnline
}

func OnlineInstances(rows Rows, instances []store.Instance) []store.Instance {
	return planning.OnlineInstances(instances, func(id uint) bool { return InstanceNodeOnline(rows, id) })
}
