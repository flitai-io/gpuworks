package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A kube.settings message from the server changes the idle return and the
// NVIDIA RuntimeClass of the running kube driver, whatever the manifest or
// enrollment gave it at start.
func TestKubeSettingsAreAppliedLive(t *testing.T) {
	srv := kubefake.New(t, "gpuworks-system", "gpu-1")
	srv.RuntimeClasses["nvidia"] = true
	driver := kube.New(kube.Options{API: srv.Client(), Namespace: "gpuworks-system", NodeName: "gpu-1", PodName: "agent",
		RuntimeClass: "from-old-manifest", IdleReturn: time.Minute, Log: slog.New(slog.DiscardHandler)})
	n := newNodeRuntime(slog.New(slog.DiscardHandler), driver, filepath.Join(t.TempDir(), "models"), "kube", &gpu.SMIProber{}, nil, engine.WithNetworkInterfaceResolver(nil))
	if driver.RuntimeClass() != "from-old-manifest" || driver.IdleReturn() != time.Minute {
		t.Fatalf("start values = %q, %s", driver.RuntimeClass(), driver.IdleReturn())
	}

	n.OnKubeSettings(context.Background(), proto.KubeSettings{IdleReturnSeconds: 300, RuntimeClass: "nvidia-cdi"})
	if driver.RuntimeClass() != "nvidia-cdi" || driver.IdleReturn() != 5*time.Minute {
		t.Fatalf("after settings = %q, %s", driver.RuntimeClass(), driver.IdleReturn())
	}
	n.OnKubeSettings(context.Background(), proto.KubeSettings{IdleReturnSeconds: -1, RuntimeClass: ""})
	if driver.RuntimeClass() != "nvidia" || driver.IdleReturn() >= 0 {
		t.Fatalf("after a keep setting with the cluster's own class = %q, %s", driver.RuntimeClass(), driver.IdleReturn())
	}
}

// Outside Kubernetes the message is ignored: the node keeps its runtime and
// nothing is asked of it.
func TestKubeSettingsAreIgnoredOutsideKubernetes(t *testing.T) {
	rec := recording.New(filepath.Join(t.TempDir(), "rec.jsonl"))
	n := newNodeRuntime(slog.New(slog.DiscardHandler), rec, filepath.Join(t.TempDir(), "models"), "", &gpu.SMIProber{}, nil, engine.WithNetworkInterfaceResolver(nil))
	before := n.RuntimeDriverName()
	n.OnKubeSettings(context.Background(), proto.KubeSettings{IdleReturnSeconds: 1, RuntimeClass: "nvidia"})
	if n.RuntimeDriverName() != before {
		t.Fatalf("driver changed from %q to %q", before, n.RuntimeDriverName())
	}
}
