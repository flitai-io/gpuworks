package controller

import (
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// translatesServedName is planning.TranslatesServedName for node's agent, as
// the hub knows it.
func (c *Controller) translatesServedName(node *store.Node) bool {
	if node == nil {
		return false
	}
	var caps []string
	if hub, ok := c.hub.(interface{ NodeCaps(string) []string }); ok {
		caps = hub.NodeCaps(node.UUID)
	}
	return planning.TranslatesServedName(node.RuntimeDriver, caps)
}
