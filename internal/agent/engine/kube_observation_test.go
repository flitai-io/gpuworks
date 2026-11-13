package engine

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
	"gpuworks.ai/gpuworks/internal/proto"
)

// kubeImages is the node's containerd: it inspects images, not containers.
type kubeImages struct {
	kube.ImageStore
}

func (kubeImages) InspectImage(_ context.Context, ref string) (runtime.ImageInspection, error) {
	return runtime.ImageInspection{ID: "sha256:" + strings.Repeat("cd", 32), Cmd: []string{"serve"}, Env: map[string]string{"LD_LIBRARY_PATH": "/usr/lib"}}, nil
}

// The kube runtime's Run returns the pod's name, not a container id, so the
// engine's identity check between Run's result and the inspected container
// cannot apply; the observation must still be made, with containerd's id.
func TestKubeObservationIsMadeForARunHandleThatIsAName(t *testing.T) {
	const ns, node = "gpuworks-system", "gpu-node-1"
	srv := kubefake.New(t, ns, node)
	d := kube.New(kube.Options{API: srv.Client(), Images: kubeImages{}, Namespace: ns, NodeName: node, PodName: "agent",
		Log: slog.New(slog.DiscardHandler), Poll: time.Millisecond, StartTimeout: time.Second})
	id, err := d.Run(context.Background(), runtime.WorkloadSpec{Name: "gpuworks-aaaaaaaaaaaa", Image: "img:1", Env: map[string]string{"A": "1"}, HostNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{}
	got := e.collectRuntimeObservation(context.Background(), d, id, id)
	if got.Runtime != "kube" || got.Container.Status != proto.ObservationObserved {
		t.Fatalf("container observation = %+v", got.Container)
	}
	pod := srv.Pod(ns, id)
	if got.Container.ContainerID != strings.TrimPrefix(pod.Status.ContainerStatuses[0].ContainerID, "containerd://") || got.Container.Env["A"] != "1" || got.Container.NetworkMode != "host" {
		t.Fatalf("container = %+v", got.Container)
	}
	// The manifest digest the kubelet reports resolves the image; its id is
	// the config digest the image inspect gives.
	if got.Image.Status != proto.ObservationObserved || got.Image.ImageID != "sha256:"+strings.Repeat("cd", 32) || got.Container.ImageID != got.Image.ImageID {
		t.Fatalf("image = %+v / %+v", got.Image, got.Container)
	}
	// A pod that is gone is "not running", not an identity mismatch.
	if err := srv.Client().Delete(context.Background(), kubeapi.PodPath(ns, id), nil); err != nil {
		t.Fatal(err)
	}
	gone := e.collectRuntimeObservation(context.Background(), d, id, id)
	if gone.Container.Status != proto.ObservationUnknown || gone.Container.Reason != proto.ObservationReasonNotRunning {
		t.Fatalf("after the pod is gone: %+v", gone.Container)
	}
}
