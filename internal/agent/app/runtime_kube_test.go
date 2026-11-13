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
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// kubeDriver stands in for the kube runtime: it records what it is asked
// to do and answers to its name.
type kubeDriver struct{ *recording.Driver }

func (kubeDriver) Name() string { return "kube" }

// kubeNode is a Kubernetes node, on a host that also has a working Docker
// (the host's runtimes are what an unpinned agent would move to).
func kubeNode(t *testing.T) (*NodeRuntime, *fakeDockerHost) {
	t.Helper()
	host := newFakeDockerHost(t, daemonWithNVIDIA)
	n := newNodeRuntime(slog.New(slog.DiscardHandler), kubeDriver{recording.New(filepath.Join(t.TempDir(), "rec.jsonl"))},
		filepath.Join(t.TempDir(), "models"), "kube", &gpu.SMIProber{}, nil, engine.WithNetworkInterfaceResolver(nil))
	return n, host
}

// A Kubernetes node keeps the kube runtime: switching to the host's docker,
// nerdctl or udocker would run instances outside their pods, on cards the
// kubelet can give to other pods. The refusal says why, and reaches the
// server as the RPC's error.
func TestKubeNodeRefusesToSwitchRuntime(t *testing.T) {
	n, _ := kubeNode(t)
	ctx := context.Background()
	for _, target := range []string{proto.RuntimePreferenceDocker, proto.RuntimePreferenceNerdctl, proto.RuntimePreferenceUdocker} {
		err := n.ApplyRuntimePreference(ctx, target)
		if err == nil || !strings.Contains(err.Error(), "Kubernetes node") || !strings.Contains(err.Error(), target) {
			t.Fatalf("ApplyRuntimePreference(%s) = %v", target, err)
		}
		if err := n.SwitchRuntime(ctx, target); err == nil {
			t.Fatalf("SwitchRuntime(%s) switched a Kubernetes node", target)
		}
		params, _ := json.Marshal(map[string]string{"runtime": target})
		if _, err := n.OnRPC(ctx, proto.RPCNodeSetRuntime, params); err == nil || !strings.Contains(err.Error(), "Kubernetes node") {
			t.Fatalf("the set-runtime RPC for %s = %v", target, err)
		}
		if got := n.RuntimeDriverName(); got != "kube" {
			t.Fatalf("runtime = %q after asking for %s", got, target)
		}
	}
}

// Handing the choice back to auto-detection must not find the host's Docker
// (or the nerdctl wrapper the kube runtime installs): the kube runtime
// stays, whether an operator asks or the server replays the preference on
// connect, and the watch never takes the node over.
func TestKubeNodeIgnoresAutoDetection(t *testing.T) {
	n, _ := kubeNode(t)
	ctx := context.Background()
	for _, preference := range []string{proto.RuntimePreferenceAuto, ""} {
		if err := n.ApplyRuntimePreference(ctx, preference); err != nil {
			t.Fatalf("ApplyRuntimePreference(%q) = %v", preference, err)
		}
	}
	n.OnStateSync(ctx, proto.DesiredState{RuntimePreference: proto.RuntimePreferenceDocker})
	n.recheckRuntimeFallback(ctx)
	if got := n.RuntimeDriverName(); got != "kube" {
		t.Fatalf("runtime = %q", got)
	}
	if auto, _ := n.runtimeChoice.get(); auto {
		t.Fatal("a Kubernetes node picked its runtime by auto-detection")
	}
}

// The pin is the kube node's alone: another node still switches.
func TestNonKubeNodeStillSwitchesRuntime(t *testing.T) {
	host := newFakeDockerHost(t, daemonWithNVIDIA)
	_ = host
	n := fallbackNode(t)
	if err := n.refuseRuntimeSwitch("docker"); err != nil {
		t.Fatalf("refuseRuntimeSwitch on an ordinary node = %v", err)
	}
}
