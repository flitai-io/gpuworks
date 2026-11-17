package conn

import (
	"context"
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
)

// kubeSettingsHandler is implemented by a node runtime that can run on
// Kubernetes: it applies the cluster entry's settings the server pushes
// (proto.KubeSettings) and ignores them when the node is not a Kubernetes one.
type kubeSettingsHandler interface {
	OnKubeSettings(ctx context.Context, s proto.KubeSettings)
}

// handleKubeSettings passes a kube.settings message to the node runtime. A
// body that does not decode is dropped: the settings stay what they were
// until the server sends them again.
func (c *Conn) handleKubeSettings(ctx context.Context, env proto.Envelope) {
	var s proto.KubeSettings
	if err := json.Unmarshal(env.D, &s); err != nil {
		c.log.Warn("bad kube.settings", "err", safeConnectionError(err))
		return
	}
	if h, ok := c.handlers.(kubeSettingsHandler); ok {
		h.OnKubeSettings(ctx, s)
	}
}
