package app

import (
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
)

// A Kubernetes agent mounts storages on its node from a Pod: the mount
// manager must run its commands in the node's mount namespace and know the
// node's root, or it would again take its own directories for the node's.
// An agent on the host has neither.
func TestOnlyAKubernetesAgentMountsStoragesThroughItsNodesRoot(t *testing.T) {
	n, _ := kubeNode(t)
	run, root := n.storage.manager.NodeMount()
	if root != kube.HostRoot {
		t.Errorf("node root = %q, want %q", root, kube.HostRoot)
	}
	if reflect.ValueOf(run).Pointer() != reflect.ValueOf(kube.HostExec).Pointer() {
		t.Error("mount commands do not run in the node's mount namespace")
	}

	onHost := newNodeRuntime(slog.New(slog.DiscardHandler), recording.New(filepath.Join(t.TempDir(), "rec.jsonl")),
		filepath.Join(t.TempDir(), "models"), "", metricsOnlyProber{}, nil)
	run, root = onHost.storage.manager.NodeMount()
	if root != "" {
		t.Errorf("an agent on the host got node root %q", root)
	}
	if reflect.ValueOf(run).Pointer() == reflect.ValueOf(kube.HostExec).Pointer() {
		t.Error("an agent on the host runs mount through nsenter")
	}
}
