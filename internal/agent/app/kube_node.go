package app

import (
	"context"
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/proto"
)

// What a node that runs on Kubernetes (the kube runtime) adds to an agent.

// KubeUnschedulable is whether this Kubernetes node is cordoned there; false
// on any other runtime, and when the API server cannot be asked (the server
// keeps the node as it was rather than act on a guess).
func (n *NodeRuntime) KubeUnschedulable(ctx context.Context) bool {
	d, ok := n.runtimeDriver().(interface {
		Unschedulable(context.Context) (bool, error)
	})
	if !ok {
		return false
	}
	v, err := d.Unschedulable(ctx)
	if err != nil {
		n.log.Debug("kubernetes node state unavailable", "err", err)
		n.kubeCordon.mu.Lock()
		defer n.kubeCordon.mu.Unlock()
		return n.kubeCordon.last
	}
	n.kubeCordon.mu.Lock()
	n.kubeCordon.last = v
	n.kubeCordon.mu.Unlock()
	return v
}

// kubeRPC answers the RPCs only a Kubernetes node's agent can.
func (n *NodeRuntime) kubeRPC(ctx context.Context, method string, params json.RawMessage) (any, error) {
	d, ok := n.runtimeDriver().(*kube.Driver)
	if !ok {
		return nil, fmt.Errorf("%s: this node does not run on Kubernetes", method)
	}
	switch method {
	default:
		var req proto.KubeNodeExistsParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, fmt.Errorf("decode %s: %w", method, err)
		}
		exists, err := d.NodeExists(ctx, req.NodeName)
		if err != nil || !exists {
			return proto.KubeNodeExistsResult{Exists: exists}, err
		}
		agent, err := d.AgentOn(ctx, req.NodeName)
		if err != nil {
			return nil, err
		}
		return proto.KubeNodeExistsResult{Exists: true, Agent: &agent}, nil
	}
}
