package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
)

// ascendRig is a node of two Ascend cards, NPU 5 and 6, as a VM with two
// passed-through 910B4s shows them: the Ascend device plugin names its devices
// Ascend910-<physical id>, the agent's probe ascend-<npu id>.
func ascendRig(t *testing.T) *rig {
	t.Helper()
	r := newRigFor(t, "huawei.com/Ascend910", map[int]string{5: "ascend-5", 6: "ascend-6"}, []string{"Ascend910-5", "Ascend910-6"})
	r.d.o.CardDevices = func(context.Context) (map[int][]string, error) {
		common := []string{"/dev/davinci_manager", "/dev/devmm_svm", "/dev/hisi_hdc"}
		return map[int][]string{5: append([]string{"/dev/davinci5"}, common...), 6: append([]string{"/dev/davinci6"}, common...)}, nil
	}
	return r
}

// ascendSpec is what the engine hands the kube runtime for an Ascend card:
// the nerdctl form of the injection (raw devices and driver mounts).
func ascendSpec(name string, card int) runtime.WorkloadSpec {
	s := spec(name, card)
	s.DockerArgs = []string{"--device", "/dev/davinci" + string(rune('0'+card)), "--device", "/dev/davinci_manager",
		"--device", "/dev/devmm_svm", "--device", "/dev/hisi_hdc"}
	s.Mounts = []runtime.Mount{{Source: "/usr/local/sbin/npu-smi", Target: "/usr/local/sbin/npu-smi", ReadOnly: true}}
	s.Env["ASCEND_RT_VISIBLE_DEVICES"] = "0"
	return s
}

// TestRunHandsAscendDevicesThroughThePlugin: the instance pod asks the
// agent's device plugin for its card and the shared nodes; it is not
// privileged and names no runtime class.
func TestRunHandsAscendDevicesThroughThePlugin(t *testing.T) {
	r := ascendRig(t)
	ctx := context.Background()
	id, err := r.d.Run(ctx, ascendSpec("gpuworks-aaaaaaaaaaaa", 6))
	if err != nil {
		t.Fatal(err)
	}
	pod := r.srv.Pod(testNS, id)
	c := pod.Spec.Containers[0]
	want := map[string]string{"gpuworks.ai/accelerator-6": "1", "gpuworks.ai/accelerator-shared": "1"}
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
	if pod.Metadata.Annotations[AnnotationCards] != "ascend-6" {
		t.Fatalf("cards annotation = %q", pod.Metadata.Annotations[AnnotationCards])
	}
	// The device plugin's Ascend910-6 is the probe's ascend-6: that card is
	// borrowed and the other one, taken at the same time, given back.
	holds := r.holdPods()
	if len(holds) != 1 || r.kubelet.given[testNS+"/"+holds[0]] != "Ascend910-6" {
		t.Fatalf("hold pods = %v, given %v", holds, r.kubelet.given)
	}
}

// ppuRig is a node of two PPUs, indexes 8 and 9, whose device plugin names
// its devices PPU-<index>; every card has its own alixpu_ppu node and both
// share alixpu_ctl.
func ppuRig(t *testing.T) *rig {
	t.Helper()
	r := newRigFor(t, "alibabacloud.com/ppu", map[int]string{8: "GPU-ppu-8", 9: "GPU-ppu-9"}, []string{"PPU-8", "PPU-9"})
	r.d.o.CardDevices = func(context.Context) (map[int][]string, error) {
		ctl := "/dev/alixpu_ctl:/dev/alixpu_ctl:rwm"
		return map[int][]string{
			8: {"/dev/alixpu_ppu9:/dev/alixpu_ppu9:rwm", ctl},
			9: {"/dev/alixpu_ppu10:/dev/alixpu_ppu10:rwm", ctl},
		}, nil
	}
	return r
}

// TestRunHandsPPUDevicesThroughThePlugin: a PPU workload as the engine builds
// it for the kube runtime (the PPU injection for the driver "kube": the
// selected devices, numbered from zero) becomes a pod that asks the agent's
// device plugin for its card and the shared node, unprivileged.
func TestRunHandsPPUDevicesThroughThePlugin(t *testing.T) {
	r := ppuRig(t)
	s := spec("gpuworks-dddddddddddd", 9)
	s.DockerArgs = []string{"--device", "/dev/alixpu_ppu10:/dev/alixpu_ppu10:rwm", "--device", "/dev/alixpu_ctl:/dev/alixpu_ctl:rwm"}
	s.Env["NVIDIA_VISIBLE_DEVICES"] = "void"
	s.Env["CUDA_VISIBLE_DEVICES"] = "0"
	id, err := r.d.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	pod := r.srv.Pod(testNS, id)
	c := pod.Spec.Containers[0]
	want := map[string]string{"gpuworks.ai/accelerator-9": "1", "gpuworks.ai/accelerator-shared": "1"}
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
	envs := map[string]string{}
	for _, e := range c.Env {
		envs[e.Name] = e.Value
	}
	if envs["CUDA_VISIBLE_DEVICES"] != "0" || envs["NVIDIA_VISIBLE_DEVICES"] != "void" {
		t.Fatalf("env = %v", envs)
	}
	// The plugin's PPU-9 is the probe's card 9: borrowed; PPU-8 given back.
	holds := r.holdPods()
	if len(holds) != 1 || r.kubelet.given[testNS+"/"+holds[0]] != "PPU-9" {
		t.Fatalf("hold pods = %v, given %v", holds, r.kubelet.given)
	}
}

// TestDevicesThePluginDoesNotServeRunPrivileged: an InfiniBand node is not a
// card's; the pod runs privileged, as its docker twin sees the host's /dev.
func TestDevicesThePluginDoesNotServeRunPrivileged(t *testing.T) {
	r := ascendRig(t)
	s := ascendSpec("gpuworks-bbbbbbbbbbbb", 5)
	s.DockerArgs = append(s.DockerArgs, "--device", "/dev/infiniband")
	id, err := r.d.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	c := r.srv.Pod(testNS, id).Spec.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Fatal("pod with a device outside the plugin is not privileged")
	}
	if _, asked := c.Resources.Requests["gpuworks.ai/accelerator-5"]; asked {
		t.Fatalf("privileged pod also asks the plugin: %v", c.Resources.Requests)
	}
}

func TestRunFailsWhenTheCardsDevicesAreUnknown(t *testing.T) {
	r := ascendRig(t)
	r.d.o.CardDevices = func(context.Context) (map[int][]string, error) { return nil, errors.New("npu-smi: timeout") }
	if _, err := r.d.Run(context.Background(), ascendSpec("gpuworks-cccccccccccc", 5)); err == nil || !strings.Contains(err.Error(), "npu-smi: timeout") {
		t.Fatalf("Run = %v", err)
	}
	if r.srv.Pod(testNS, "gpuworks-cccccccccccc") != nil {
		t.Fatal("a pod was created without its devices")
	}
}

func TestDetectRefusesNodesWithoutContainerd(t *testing.T) {
	r := newRig(t, 1)
	r.srv.SetRuntime(testNode, "cri-o://1.30.4")
	if ok, err := r.d.Detect(context.Background()); ok || !errors.Is(err, errNotContainerd) || !strings.Contains(err.Error(), "cri-o://1.30.4") {
		t.Fatalf("Detect on CRI-O = %v, %v", ok, err)
	}
}

// TestSharedCardsAreNotBorrowed: a device plugin that hands out parts of
// cards (MIG, HAMi, time-slicing replicas) leaves no whole card to borrow.
// The platform does not start instances on them, and reports every card as
// used outside it, with the reason.
func TestSharedCardsAreNotBorrowed(t *testing.T) {
	for name, setup := range map[string]func(*kubefake.Server){
		"mig":          func(s *kubefake.Server) { s.SetAllocatable(testNode, "nvidia.com/mig-1g.10gb", "7") },
		"hami":         func(s *kubefake.Server) { s.SetAllocatable(testNode, "nvidia.com/gpumem", "48000") },
		"time-slicing": func(s *kubefake.Server) { s.SetAllocatable(testNode, "nvidia.com/gpu", "8") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, 2)
			setup(r.srv)
			ctx := context.Background()
			_, err := r.d.Run(ctx, spec("gpuworks-ffffffffffff", 0))
			if err == nil || !strings.Contains(err.Error(), "whole cards only") {
				t.Fatalf("Run on shared cards = %v", err)
			}
			if len(r.holdPods()) != 0 || r.srv.Pod(testNS, "gpuworks-ffffffffffff") != nil {
				t.Fatalf("pods created: holds %v", r.holdPods())
			}
			held, err := r.d.ExternalCardHolders(ctx)
			if err != nil || len(held) != 2 || len(held["GPU-a"]) != 1 || !strings.Contains(held["GPU-a"][0].Name, "whole cards only") {
				t.Fatalf("holders = %v, %v", held, err)
			}
			// Kubernetes going back to whole cards is noticed without a restart.
			r.srv.SetAllocatable(testNode, "nvidia.com/mig-1g.10gb", "0")
			r.srv.SetAllocatable(testNode, "nvidia.com/gpumem", "0")
			r.srv.SetAllocatable(testNode, "nvidia.com/gpu", "2")
			if held, err := r.d.ExternalCardHolders(ctx); err != nil || len(held) != 0 {
				t.Fatalf("holders after whole cards = %v, %v", held, err)
			}
			if _, err := r.d.Run(ctx, spec("gpuworks-ffffffffffff", 0)); err != nil {
				t.Fatalf("Run on whole cards = %v", err)
			}
		})
	}
}

func TestKeepAllDoesNotBorrowSharedCards(t *testing.T) {
	r := newRig(t, 2)
	r.srv.SetAllocatable(testNode, "nvidia.com/gpu", "4")
	if err := r.d.cards.keepAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.holdPods()) != 0 {
		t.Fatalf("hold pods on shared cards: %v", r.holdPods())
	}
}

// TestBorrowReportsWhyTheKubeletRefused: a node under disk pressure evicts
// the hold pods; the error says so instead of blaming another pod.
func TestBorrowReportsWhyTheKubeletRefused(t *testing.T) {
	r := newRig(t, 1)
	r.srv.Kubelet = func(p *kubeapi.Pod) {
		if p.Metadata.Labels[LabelRole] == RoleHold {
			p.Status.Phase, p.Status.Reason, p.Status.Message = "Failed", "Evicted", "The node had condition: [DiskPressure]."
			return
		}
		r.kubelet.admit(p)
	}
	_, err := r.d.Run(context.Background(), spec("gpuworks-999999999999", 0))
	if err == nil || !strings.Contains(err.Error(), "the kubelet refused the hold pods: Evicted: The node had condition: [DiskPressure].") {
		t.Fatalf("Run = %v", err)
	}
}
