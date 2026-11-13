package kube

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
)

const twoCardRegistration = `[{"id":"GPU-a","count":10,"devmem":46068,"devcore":100,"type":"NVIDIA L20","mode":"hami-core","health":true},` +
	`{"id":"GPU-b","count":10,"devmem":46068,"devcore":100,"type":"NVIDIA L20","mode":"hami-core","health":true}]`

// How the node's mode is told from what the node says: the registration HAMi
// writes, the resources ACK GPU sharing advertises, and everything else that
// hands out parts of cards.
func TestNodeModeIsReadFromWhatTheClusterRuns(t *testing.T) {
	ack := func(s *kubefake.Server) {
		s.SetCapacity(testNode, resNVIDIAGPU, "2", "0")
		s.SetCapacity(testNode, resACKMem, "88", "88")
		s.SetCapacity(testNode, resACKCount, "2", "2")
	}
	for name, tc := range map[string]struct {
		setup    func(*kubefake.Server)
		mode     cardMode
		resource string
		reason   string // part of the refusal; "" when borrowing is possible
	}{
		"plain device plugin": {func(*kubefake.Server) {}, modePlugin, "nvidia.com/gpu", ""},
		"HAMi": {func(s *kubefake.Server) {
			s.SetCapacity(testNode, resNVIDIAGPU, "20", "20")
			s.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register", twoCardRegistration)
		}, modeHAMi, "nvidia.com/gpu", ""},
		"HAMi with its memory and compute resources (older versions)": {func(s *kubefake.Server) {
			s.SetCapacity(testNode, resNVIDIAGPU, "20", "20")
			s.SetAllocatable(testNode, "nvidia.com/gpumem", "92136")
			s.SetAllocatable(testNode, "nvidia.com/gpucores", "200")
			s.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register", twoCardRegistration)
		}, modeHAMi, "nvidia.com/gpu", ""},
		"HAMi's registration left behind with the plain plugin back": {func(s *kubefake.Server) {
			s.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register", twoCardRegistration)
		}, modeFractional, "", "HAMi registered 20 shares of 2 cards on this node but the node advertises nvidia.com/gpu 2"},
		"HAMi whose plugin is not running": {func(s *kubefake.Server) {
			s.SetCapacity(testNode, resNVIDIAGPU, "0", "0")
			s.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register", twoCardRegistration)
		}, modeFractional, "", "HAMi's device plugin is not running here"},
		"HAMi beside MIG": {func(s *kubefake.Server) {
			s.SetCapacity(testNode, resNVIDIAGPU, "20", "20")
			s.SetAllocatable(testNode, "nvidia.com/mig-1g.10gb", "7")
			s.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register", twoCardRegistration)
		}, modeFractional, "", "MIG partitions"},
		"HAMi registration that cannot be read": {func(s *kubefake.Server) {
			s.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register", "{oops")
		}, modeFractional, "", "cannot be read"},
		"HAMi of another vendor": {func(s *kubefake.Server) {
			s.SetNodeAnnotation(testNode, "hami.io/node-mlu-register", "[]")
		}, modeFractional, "", "hami.io/node-mlu-register"},
		"HAMi's handshake is not a registration": {func(s *kubefake.Server) {
			s.SetNodeAnnotation(testNode, "hami.io/node-handshake", "Requesting_2026")
		}, modePlugin, "nvidia.com/gpu", ""},
		"ACK GPU sharing": {ack, modeACK, "aliyun.com/gpu-mem", ""},
		"ACK GPU sharing with HAMi's registration left on the node": {func(s *kubefake.Server) {
			ack(s)
			s.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register", twoCardRegistration)
		}, modeACK, "aliyun.com/gpu-mem", ""},
		"ACK labelled but not advertising yet": {func(s *kubefake.Server) {
			s.SetNodeLabel(testNode, "ack.node.gpu.schedule", "cgpu")
		}, modeFractional, "", `advertises aliyun.com/gpu-mem=""`},
		"ACK with cards of different sizes": {func(s *kubefake.Server) {
			ack(s)
			s.SetCapacity(testNode, resACKMem, "89", "89")
		}, modeFractional, "", "not of one size"},
		"ACK whose memory label disagrees": {func(s *kubefake.Server) {
			ack(s)
			s.SetNodeLabel(testNode, "aliyun.accelerator/nvidia_mem", "24576MiB")
		}, modeFractional, "", "says \"24576MiB\""},
		"ACK counting more cards than the node has": {func(s *kubefake.Server) {
			ack(s)
			s.SetCapacity(testNode, resACKCount, "4", "4")
			s.SetCapacity(testNode, resACKMem, "176", "176")
		}, modeFractional, "", "counts 4 cards on this node but the node's probe finds 2"},
		"time-slicing": {func(s *kubefake.Server) { s.SetAllocatable(testNode, "nvidia.com/gpu", "8") },
			modeFractional, "", "time-slicing replicas or MPS"},
		"MIG": {func(s *kubefake.Server) { s.SetAllocatable(testNode, "nvidia.com/mig-1g.10gb", "7") },
			modeFractional, "", "MIG partitions"},
		"vNPU templates without HAMi": {func(s *kubefake.Server) { s.SetAllocatable(testNode, "huawei.com/Ascend910-2c", "16") },
			modeFractional, "", "Ascend vNPU templates without HAMi's registration"},
		"HAMi-style resources without a registration": {func(s *kubefake.Server) { s.SetAllocatable(testNode, "nvidia.com/gpumem", "48000") },
			modeFractional, "", "without a hami.io/node-*-register annotation"},
		"nothing advertises cards": {func(s *kubefake.Server) { s.SetCapacity(testNode, resNVIDIAGPU, "0", "0") }, modeNone, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, 2)
			tc.setup(r.srv)
			v, err := r.d.cards.refresh(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if v.mode != tc.mode || (v.mode != modeFractional && v.resource != tc.resource) {
				t.Fatalf("mode %d resource %q (shared %q), want mode %d resource %q", v.mode, v.resource, v.shared, tc.mode, tc.resource)
			}
			if tc.reason == "" && v.shared != "" || tc.reason != "" && !strings.Contains(v.shared, tc.reason) {
				t.Fatalf("reason = %q, want it to say %q", v.shared, tc.reason)
			}
			if v.shared != "" && !strings.Contains(v.shared, "whole cards only") {
				t.Fatalf("reason %q does not say what the platform needs", v.shared)
			}
		})
	}
}

// Instances are refused with the reason, and every card is reported as used
// outside the platform, in each of the modes the platform cannot borrow in.
func TestNodeModesTheBorrowRefusesReportEveryCard(t *testing.T) {
	r := newRig(t, 2)
	r.srv.SetCapacity(testNode, resNVIDIAGPU, "2", "0")
	r.srv.SetCapacity(testNode, resACKMem, "89", "89")
	r.srv.SetCapacity(testNode, resACKCount, "2", "2")
	_, err := r.d.Run(context.Background(), spec("gpuworks-ffffffffffff", 0))
	if err == nil || !strings.Contains(err.Error(), "not of one size") {
		t.Fatalf("Run = %v", err)
	}
	held, err := r.d.ExternalCardHolders(context.Background())
	if err != nil || len(held) != 2 || !strings.Contains(held["GPU-a"][0].Name, "not of one size") {
		t.Fatalf("holders = %v, %v", held, err)
	}
	if len(r.holdPods()) != 0 {
		t.Fatalf("hold pods: %v", r.holdPods())
	}
}

// A hold pod is told from its spec: the resource it asks for and whether it
// goes through a scheduler.
func TestHoldSignatureIsReadFromThePodSpec(t *testing.T) {
	pod := func(sched string, limits map[string]string) kubeapi.Pod {
		return kubeapi.Pod{Spec: kubeapi.PodSpec{SchedulerName: sched, Containers: []kubeapi.Container{{Resources: &kubeapi.Resources{Limits: limits}}}}}
	}
	for name, tc := range map[string]struct {
		pod  kubeapi.Pod
		want string
	}{
		"plugin": {pod("", map[string]string{"nvidia.com/gpu": "1"}), "plugin:nvidia.com/gpu"},
		"HAMi": {pod("hami-scheduler", map[string]string{"nvidia.com/gpu": "1", "nvidia.com/gpucores": "100", "nvidia.com/gpumem-percentage": "100"}),
			"hami:nvidia.com/gpu"},
		"ACK":    {pod("", map[string]string{"aliyun.com/gpu-mem": "44"}), "ack-gpushare"},
		"Ascend": {pod("", map[string]string{"huawei.com/Ascend910": "1"}), "plugin:huawei.com/Ascend910"},
		"none":   {kubeapi.Pod{}, "none"},
	} {
		if got := holdSignature(tc.pod); got != tc.want {
			t.Errorf("%s: holdSignature = %q, want %q", name, got, tc.want)
		}
	}
}
