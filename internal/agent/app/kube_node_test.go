package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The server asks a Kubernetes node's agent which nodes a new selection
// still selects; the answer covers exactly the nodes asked about.
func TestKubeNodesSelectedRPC(t *testing.T) {
	srv := kubefake.New(t, "gpuworks-system", "node-1")
	srv.AddNode("node-2", map[string]string{"pool": "np-b"})
	srv.SetNodeLabel("node-1", "pool", "np-a")
	d := kube.New(kube.Options{API: srv.Client(), Namespace: "gpuworks-system", NodeName: "node-1", Log: slog.New(slog.DiscardHandler)})
	n := newNodeRuntime(slog.New(slog.DiscardHandler), d, filepath.Join(t.TempDir(), "models"), "kube", &gpu.SMIProber{}, nil, engine.WithNetworkInterfaceResolver(nil))
	params, _ := json.Marshal(proto.KubeNodesSelectedParams{NodeNames: []string{"node-1", "node-2", "gone"}, PoolLabel: "pool", PoolIDs: []string{"np-a"}})
	out, err := n.OnRPC(context.Background(), proto.RPCKubeNodesSelected, params)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if want := `{"selected":{"gone":false,"node-1":true,"node-2":false}}`; string(raw) != want {
		t.Fatalf("answer = %s, want %s", raw, want)
	}
	if _, err := n.OnRPC(context.Background(), proto.RPCKubeNodesSelected, json.RawMessage(`{"node_names":`)); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("a bad request = %v", err)
	}
	srv.FailNext["GET /api/v1/nodes/"] = 500
	if _, err := n.OnRPC(context.Background(), proto.RPCKubeNodesSelected, params); err == nil {
		t.Fatal("an API failure was answered")
	}
}

// A node that is not on Kubernetes cannot answer.
func TestKubeNodesSelectedRPCRefusedOffKubernetes(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), recording.New(filepath.Join(t.TempDir(), "rec.jsonl")),
		filepath.Join(t.TempDir(), "models"), "docker", &gpu.SMIProber{}, nil, engine.WithNetworkInterfaceResolver(nil))
	_, err := n.OnRPC(context.Background(), proto.RPCKubeNodesSelected, json.RawMessage(`{"node_names":["a"]}`))
	if err == nil || !strings.Contains(err.Error(), "does not run on Kubernetes") {
		t.Fatalf("err = %v", err)
	}
}
