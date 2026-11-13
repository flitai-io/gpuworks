package kube

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
)

// detectedRig is a one-card node whose agent pod pulled with pullSecrets
// and whose cluster has the given RuntimeClasses; runtimeClass is the
// cluster's setting.
func detectedRig(t *testing.T, runtimeClass string, classes []string, pullSecrets []kubeapi.LocalRef, fail int) *rig {
	t.Helper()
	srv := kubefake.New(t, testNS, testNode)
	for _, c := range classes {
		srv.RuntimeClasses[c] = true
	}
	if fail != 0 {
		srv.FailNext["GET /apis/node.k8s.io/v1/runtimeclasses/"] = fail
	}
	r := &rig{srv: srv, images: &fakeImages{}, now: time.Unix(1_800_000_000, 0)}
	r.kubelet = &fakeKubelet{srv: srv, given: map[string]string{}, resource: "nvidia.com/gpu", devices: []string{"GPU-a"}}
	srv.SetAllocatable(testNode, "nvidia.com/gpu", "1")
	srv.Kubelet = r.kubelet.admit
	if err := srv.Client().Create(context.Background(), kubeapi.PodsPath(testNS), kubeapi.Pod{
		Metadata: kubeapi.ObjectMeta{Name: "gpuworks-agent-x1"},
		Spec: kubeapi.PodSpec{NodeName: testNode, ImagePullSecrets: pullSecrets,
			Containers: []kubeapi.Container{{Name: "agent", Image: "registry.example/gpuworks/gpuworks-agent:v1"}}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	r.d = New(Options{
		API: srv.Client(), Kubelet: r.kubelet, Images: r.images, Namespace: testNS, NodeName: testNode, PodName: "gpuworks-agent-x1",
		RuntimeClass: runtimeClass, Cards: func(context.Context) (map[int]string, error) { return map[int]string{0: "GPU-a"}, nil },
		IdleReturn: 10 * time.Minute, Log: slog.New(slog.DiscardHandler), Poll: time.Millisecond, StartTimeout: 200 * time.Millisecond,
	})
	r.d.cards.now = func() time.Time { return r.now }
	if ok, err := r.d.Detect(context.Background()); !ok || err != nil {
		t.Fatalf("Detect = %v, %v", ok, err)
	}
	return r
}

func TestInstancePodsUseTheClustersNvidiaRuntimeClassWhenItHasOne(t *testing.T) {
	cases := []struct {
		name, configured string
		classes          []string
		fail             int
		want             string // "" = no runtimeClassName
	}{
		{"k3s defines nvidia beside runc", "", []string{"nvidia"}, 0, "nvidia"},
		{"managed cluster defines none", "", nil, 0, ""},
		{"old manifest without the permission", "", []string{"nvidia"}, http.StatusForbidden, ""},
		{"configured class wins without a lookup", "nvidia-cdi", nil, 0, "nvidia-cdi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := detectedRig(t, tc.configured, tc.classes, nil, tc.fail)
			s := spec("gpuworks-rc0000000000", 0)
			s.DockerArgs = []string{"--gpus", "device=0"}
			id, err := r.d.Run(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			pod := r.srv.Pod(testNS, id)
			got := ""
			if pod.Spec.RuntimeClassName != nil {
				got = *pod.Spec.RuntimeClassName
			}
			if got != tc.want {
				t.Fatalf("runtimeClassName = %q, want %q", got, tc.want)
			}
			looked := slices.ContainsFunc(r.srv.Requests, func(q string) bool { return q == "GET /apis/node.k8s.io/v1/runtimeclasses/nvidia" })
			if looked == (tc.configured != "") {
				t.Fatalf("RuntimeClass lookup = %v with %q configured: %v", looked, tc.configured, r.srv.Requests)
			}
		})
	}
}

func TestHoldPodsPullTheAgentImageWithTheAgentsSecrets(t *testing.T) {
	secrets := []kubeapi.LocalRef{{Name: "regcred"}, {Name: "acr-default"}}
	r := detectedRig(t, "", nil, secrets, 0)
	if _, err := r.d.Run(context.Background(), spec("gpuworks-ps0000000000", 0)); err != nil {
		t.Fatal(err)
	}
	holds := r.holdPods()
	if len(holds) != 1 {
		t.Fatalf("hold pods = %v", holds)
	}
	hold := r.srv.Pod(testNS, holds[0])
	if !slices.Equal(hold.Spec.ImagePullSecrets, secrets) || hold.Spec.Containers[0].Image != "registry.example/gpuworks/gpuworks-agent:v1" {
		t.Fatalf("hold pod pulls %q with %v", hold.Spec.Containers[0].Image, hold.Spec.ImagePullSecrets)
	}

	plain := detectedRig(t, "", nil, nil, 0)
	if _, err := plain.d.Run(context.Background(), spec("gpuworks-ps1111111111", 0)); err != nil {
		t.Fatal(err)
	}
	if hold := plain.srv.Pod(testNS, plain.holdPods()[0]); hold.Spec.ImagePullSecrets != nil {
		t.Fatalf("hold pod of an agent without secrets pulls with %v", hold.Spec.ImagePullSecrets)
	}
}
