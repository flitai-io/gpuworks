package conn

import (
	"context"

	"gpuworks.ai/gpuworks/internal/proto"
)

// addHandlerFacts adds to a status the live facts the handlers observe, each
// only when they have the check: how strictly a Kubernetes node isolates its
// NVIDIA pods, and whether the host's kernel hangs UCX (NIXL). A fact the
// handlers do not report stays absent, which is how the server learns it
// does not apply.
func (c *Conn) addHandlerFacts(ctx context.Context, st *proto.NodeStatus) {
	if h, ok := c.handlers.(interface {
		NVIDIAIsolation(context.Context) *proto.NVIDIAIsolation
	}); ok {
		st.NVIDIAIsolation = h.NVIDIAIsolation(ctx)
	}
	if h, ok := c.handlers.(interface {
		UCXNetlinkHang(context.Context) *proto.UCXNetlinkHang
	}); ok {
		st.UCXNetlinkHang = h.UCXNetlinkHang(ctx)
	}
}
