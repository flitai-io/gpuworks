package controller

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// kubeEnginePorts is the range a Kubernetes node's instance ports are
// allocated from: its entry's, which the server owns (it is no part of the
// DaemonSet manifest, so editing it needs no apply), and which wins over a
// range an older manifest still hands the agent. Every other node keeps
// fallback, the range its agent reports or the server's. The entry is read by
// primary key, one row, and only when a port is being picked. A range that
// cannot be read or is not valid leaves fallback, with a warning.
func (c *Controller) kubeEnginePorts(node *store.Node, fallback proto.PortRange) proto.PortRange {
	if node.KubeClusterID == 0 {
		return fallback
	}
	live, err := c.store.KubeClusterLiveSettings(node.KubeClusterID)
	if err != nil {
		c.log.Warn("cannot read the engine port range of a Kubernetes entry", "node", node.UUID, "cluster", node.KubeClusterID, "err", err)
		return fallback
	}
	r, err := proto.ParsePortRange(live.EnginePorts)
	if err != nil {
		c.log.Warn("a Kubernetes entry has no valid engine port range", "node", node.UUID, "cluster", node.KubeClusterID, "err", err)
		return fallback
	}
	return r
}
