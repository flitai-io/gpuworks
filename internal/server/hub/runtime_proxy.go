package hub

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// runtimeRestartTimeout bounds a runtime.restart: the agent restarts the
// daemon and waits for it to answer again.
const runtimeRestartTimeout = 2 * time.Minute

// RuntimeProxy is the proxy a node's container runtime daemon uses, as its
// agent last reported it, or nil when the node is offline or its runtime
// pulls in the agent's own process.
func (h *Hub) RuntimeProxy(uuid string) *proto.RuntimeProxyStatus {
	s := h.session(uuid)
	if s == nil {
		return nil
	}
	return s.runtimeProxy.Load()
}

// NVIDIAIsolation is how strictly a Kubernetes node keeps its NVIDIA instance
// pods to their own card, as its agent last reported it; nil when it is
// strict, not reported, or the node is offline.
func (h *Hub) NVIDIAIsolation(uuid string) *proto.NVIDIAIsolation {
	s := h.session(uuid)
	if s == nil {
		return nil
	}
	return s.nvidiaIsolation.Load()
}

// RestartRuntime asks a node to restart its container runtime daemon so a
// changed proxy takes effect, and records the proxy it reports afterwards.
func (h *Hub) RestartRuntime(ctx context.Context, uuid string) (*proto.RuntimeProxyStatus, error) {
	s := h.session(uuid)
	if s == nil {
		return nil, errors.New("node not connected: " + uuid)
	}
	ctx, cancel := context.WithTimeout(ctx, runtimeRestartTimeout)
	defer cancel()
	raw, err := s.CallLong(ctx, proto.RPCRuntimeRestart, struct{}{})
	if err != nil {
		return nil, err
	}
	var result proto.RuntimeRestartResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.Proxy != nil {
		s.runtimeProxy.Store(result.Proxy)
	}
	return result.Proxy, nil
}
