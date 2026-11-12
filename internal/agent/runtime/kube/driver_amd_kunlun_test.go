package kube

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/podresources"
)

// amdRig is a node of two AMD cards, DRM cards 7 and 8 (the agent's index),
// whose ROCm device plugin names its devices by PCI address; the agent's
// probe names a card amd-<unique_id>. /dev/kfd is every card's, each card
// has its own render node.
func amdRig(t *testing.T) *rig {
	t.Helper()
	r := newRigFor(t, "amd.com/gpu", map[int]string{7: "amd-1234", 8: "amd-5678"}, []string{"0000:43:00.0", "0000:44:00.0"})
	r.d.cards.cardBDFs = func(context.Context) (map[int]string, error) {
		return map[int]string{7: "0000:43:00.0", 8: "0000:44:00.0"}, nil
	}
	r.d.o.CardDevices = func(context.Context) (map[int][]string, error) {
		kfd := "/dev/kfd:/dev/kfd:rwm"
		return map[int][]string{
			7: {kfd, "/dev/dri/renderD135:/dev/dri/renderD135:rwm"},
			8: {kfd, "/dev/dri/renderD136:/dev/dri/renderD136:rwm"},
		}, nil
	}
	return r
}

// TestRunHandsAMDDevicesThroughThePlugin: an AMD workload as the engine
// builds it for the kube runtime (the whole-card injection: /dev/kfd and the
// card's render node, no visible-device variable) becomes a pod that asks
// the agent's device plugin for its card and the shared node, unprivileged.
// The cards are borrowed from AMD's plugin, which names them by PCI address.
func TestRunHandsAMDDevicesThroughThePlugin(t *testing.T) {
	r := amdRig(t)
	s := spec("gpuworks-aaaaaaaaaaa1", 8)
	s.DockerArgs = []string{"--device", "/dev/kfd:/dev/kfd:rwm", "--device", "/dev/dri/renderD136:/dev/dri/renderD136:rwm"}
	id, err := r.d.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	pod := r.srv.Pod(testNS, id)
	c := pod.Spec.Containers[0]
	want := map[string]string{"gpuworks.ai/accelerator-8": "1", "gpuworks.ai/accelerator-shared": "1"}
	for res, n := range want {
		if c.Resources.Requests[res] != n || c.Resources.Limits[res] != n {
			t.Fatalf("resources = %+v", c.Resources)
		}
	}
	if len(c.Resources.Limits) != len(want) {
		t.Fatalf("limits = %v", c.Resources.Limits)
	}
	if sc := c.SecurityContext; sc != nil && sc.Privileged != nil && *sc.Privileged {
		t.Fatal("the pod is privileged: it would see every card")
	}
	if pod.Spec.RuntimeClassName != nil {
		t.Fatalf("runtime class = %q", *pod.Spec.RuntimeClassName)
	}
	if pod.Metadata.Annotations[AnnotationCards] != "amd-5678" {
		t.Fatalf("cards annotation = %q", pod.Metadata.Annotations[AnnotationCards])
	}
	// The plugin's 0000:44:00.0 is the probe's card 8 (amd-5678): that card is
	// borrowed and the other one, taken at the same time, given back.
	holds := r.holdPods()
	if len(holds) != 1 || r.kubelet.given[testNS+"/"+holds[0]] != "0000:44:00.0" {
		t.Fatalf("hold pods = %v, given %v", holds, r.kubelet.given)
	}
}

// TestAMDCardsAreMatchedByPCIAddress: whatever case or domain form the
// plugin writes an address in, it names the card at that address; an address
// no card has names none.
func TestAMDCardsAreMatchedByPCIAddress(t *testing.T) {
	r := amdRig(t)
	byIndex := map[int]string{7: "amd-1234", 8: "amd-5678"}
	for id, want := range map[string]string{
		"0000:43:00.0":  "amd-1234",
		"43:00.0":       "amd-1234",
		"0000:44:00.0":  "amd-5678",
		"0000:44:00.0 ": "",
		"0000:45:00.0":  "",
		"0001:43:00.0":  "",
		"amd-1234":      "amd-1234",
		"opaque":        "",
	} {
		if got := r.d.cards.cardOf(context.Background(), id, byIndex); got != want {
			t.Errorf("cardOf(%q) = %q, want %q", id, got, want)
		}
	}
	// Upper-case hex and an upper-case address on the probe's side.
	r.d.cards.cardBDFs = func(context.Context) (map[int]string, error) { return map[int]string{7: "0000:4A:00.0"}, nil }
	if got := r.d.cards.cardOf(context.Background(), "0000:4a:00.0", byIndex); got != "amd-1234" {
		t.Errorf("cardOf(lower case) = %q", got)
	}
	// A node whose probe gives no addresses matches none.
	r.d.cards.cardBDFs = nil
	if got := r.d.cards.cardOf(context.Background(), "0000:43:00.0", byIndex); got != "" {
		t.Errorf("cardOf without addresses = %q", got)
	}
}

// TestAMDCardsHeldOutsideThePlatformAreReported: another pod that the
// kubelet gave an AMD device is reported as holding that card.
func TestAMDCardsHeldOutsideThePlatformAreReported(t *testing.T) {
	r := amdRig(t)
	r.kubelet.extra = []podresources.Allocation{{Namespace: "ml", Pod: "trainer", Container: "main", Resource: "amd.com/gpu", DeviceID: "0000:43:00.0"}}
	held, err := r.d.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || len(held["amd-1234"]) != 1 || held["amd-1234"][0].Name != "Pod ml/trainer" {
		t.Fatalf("holders = %v", held)
	}
}

// kunlunRig is a node of two Kunlun cards. The platform's device plugin
// hands out /dev/xpuctrl to every card and /dev/xpuN to card N.
func kunlunRig(t *testing.T) *rig {
	t.Helper()
	r := newRigFor(t, "kunlunxin.com/xpu", map[int]string{0: "GPU-kl-0", 1: "GPU-kl-1"}, []string{"GPU-kl-0", "GPU-kl-1"})
	r.d.o.CardDevices = func(context.Context) (map[int][]string, error) {
		ctrl := "/dev/xpuctrl:/dev/xpuctrl:rwm"
		return map[int][]string{
			0: {"/dev/xpu0:/dev/xpu0:rwm", ctrl},
			1: {"/dev/xpu1:/dev/xpu1:rwm", ctrl},
		}, nil
	}
	return r
}

// TestRunHandsKunlunDevicesThroughThePlugin: the Kunlun injection for the
// driver "kube" names no --runtime, so the pod has no runtime class (a
// "runc" class does not exist); it asks the plugin for its card and the
// shared control node, unprivileged.
func TestRunHandsKunlunDevicesThroughThePlugin(t *testing.T) {
	r := kunlunRig(t)
	s := spec("gpuworks-bbbbbbbbbbb1", 1)
	s.DockerArgs = []string{"--device", "/dev/xpu1:/dev/xpu1:rwm", "--device", "/dev/xpuctrl:/dev/xpuctrl:rwm"}
	s.Env["XPU_VISIBLE_DEVICES"] = "0"
	id, err := r.d.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	pod := r.srv.Pod(testNS, id)
	c := pod.Spec.Containers[0]
	want := map[string]string{"gpuworks.ai/accelerator-1": "1", "gpuworks.ai/accelerator-shared": "1"}
	for res, n := range want {
		if c.Resources.Requests[res] != n || c.Resources.Limits[res] != n {
			t.Fatalf("resources = %+v", c.Resources)
		}
	}
	if len(c.Resources.Limits) != len(want) {
		t.Fatalf("limits = %v", c.Resources.Limits)
	}
	if sc := c.SecurityContext; sc != nil && sc.Privileged != nil && *sc.Privileged {
		t.Fatal("the pod is privileged: it would see every card")
	}
	if pod.Spec.RuntimeClassName != nil {
		t.Fatalf("runtime class = %q", *pod.Spec.RuntimeClassName)
	}
	envs := map[string]string{}
	for _, e := range c.Env {
		envs[e.Name] = e.Value
	}
	if envs["XPU_VISIBLE_DEVICES"] != "0" {
		t.Fatalf("env = %v", envs)
	}
	holds := r.holdPods()
	if len(holds) != 1 || r.kubelet.given[testNS+"/"+holds[0]] != "GPU-kl-1" {
		t.Fatalf("hold pods = %v, given %v", holds, r.kubelet.given)
	}
}

// TestKunlunDockerRuntimeOptionBecomesARuntimeClass pins why the Kunlun
// injection leaves --runtime out for Kubernetes: a pod made from the docker
// form names a RuntimeClass "runc", which a cluster does not define.
func TestKunlunDockerRuntimeOptionBecomesARuntimeClass(t *testing.T) {
	r := kunlunRig(t)
	s := spec("gpuworks-bbbbbbbbbbb2", 0)
	s.DockerArgs = []string{"--runtime", "runc", "--device", "/dev/xpu0:/dev/xpu0:rwm", "--device", "/dev/xpuctrl:/dev/xpuctrl:rwm"}
	id, err := r.d.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if rc := r.srv.Pod(testNS, id).Spec.RuntimeClassName; rc == nil || *rc != "runc" {
		t.Fatalf("runtime class = %v", rc)
	}
}
