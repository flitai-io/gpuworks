package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
)

// fakeSched stands in for the scheduler of HAMi or of ACK GPU sharing and for
// the device plugin behind it, with the behavior seen on a real ACK cluster
// (HAMi 2.11 and ack-ai-installer 1.13.7, k8s 1.36): the scheduler assigns
// the card, writes its books to annotations on the pod, binds the pod to the
// node, and the kubelet lists the devices in the plugin's own ID format. A
// pod no card fits stays Pending with PodScheduled False and the scheduler's
// message. A pod that names a scheduler nobody serves stays Pending without
// a condition.
type fakeSched struct {
	mu   sync.Mutex
	r    *rig
	node string
	// base is the hook the rig had: pods that are not the scheduler's.
	base func(*kubeapi.Pod)

	hami struct {
		on        bool
		scheduler string // the name this HAMi serves
		devmem    int    // MiB per card
		shares    int    // shares HAMi registers per card
		// failNext makes the next n schedulings Unschedulable, as when the
		// scheduler still counts a pod that was just deleted.
		failNext int
		// give overrides what HAMi allocates: the card and the memory and
		// cores, for a misbehaving scheduler.
		give *hamiDevice
	}
	ack struct {
		on      bool
		cardGiB int
		// failNext makes the next n schedulings Unschedulable.
		failNext int
	}
	// created is every pod as it was created, before anything scheduled it.
	created []kubeapi.Pod
}

// installSched hooks a scheduler into the rig's API server.
func installSched(r *rig) *fakeSched {
	s := &fakeSched{r: r, node: testNode, base: r.srv.Kubelet}
	r.srv.Kubelet = s.admit
	return s
}

func (s *fakeSched) admit(p *kubeapi.Pod) {
	var snapshot kubeapi.Pod
	raw, _ := json.Marshal(p) // a deep copy: the scheduler writes to the pod's maps
	_ = json.Unmarshal(raw, &snapshot)
	s.mu.Lock()
	s.created = append(s.created, snapshot)
	s.mu.Unlock()
	limits := map[string]string{}
	for _, c := range p.Spec.Containers {
		if c.Resources != nil {
			for k, v := range c.Resources.Limits {
				limits[k] = v
			}
		}
	}
	switch {
	case limits[resACKMem] != "":
		s.admitACK(p, limits)
	case p.Spec.SchedulerName != "":
		s.admitHAMi(p, limits)
	case s.ack.on && limits[resNVIDIAGPU] != "":
		// ACK's sharing mode advertises nvidia.com/gpu allocatable 0.
		unschedulable(p, "0/1 nodes are available: 1 Insufficient nvidia.com/gpu. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.")
	default:
		s.base(p)
	}
}

func unschedulable(p *kubeapi.Pod, msg string) {
	p.Status.Phase = "Pending"
	p.Status.Conditions = []kubeapi.PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable", Message: msg}}
}

func (s *fakeSched) livePods() []kubeapi.Pod {
	var out []kubeapi.Pod
	for _, p := range s.r.srv.Pods() {
		if p.Status.Phase != "Failed" && p.Status.Phase != "Succeeded" {
			out = append(out, p)
		}
	}
	return out
}

func (s *fakeSched) registered() []hamiRegistered {
	cards := s.r.cardsByIndex()
	idx := make([]int, 0, len(cards))
	for i := range cards {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var out []hamiRegistered
	for _, i := range idx {
		out = append(out, hamiRegistered{ID: cards[i], Count: s.hami.shares, Devmem: s.hami.devmem, Devcore: 100, Type: "NVIDIA L20", Health: true})
	}
	return out
}

// admitHAMi is hami-scheduler followed by HAMi's device plugin.
func (s *fakeSched) admitHAMi(p *kubeapi.Pod, limits map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hami.on || p.Spec.SchedulerName != s.hami.scheduler {
		p.Status.Phase = "Pending" // nobody serves that scheduler name
		return
	}
	if p.Spec.NodeSelector[labelHostname] != s.node {
		unschedulable(p, "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector.")
		return
	}
	if s.hami.failNext > 0 {
		s.hami.failNext--
		unschedulable(p, "0/1 nodes are available: 1 1/1 CardInsufficientMemory. no new claims to deallocate, preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.")
		return
	}
	want := p.Metadata.Annotations[annNVIDIAUseUUID]
	mem, cores := s.hami.devmem, 0
	if v := limits[resNVIDIACores]; v != "" {
		cores, _ = strconv.Atoi(v)
	}
	if v := limits["nvidia.com/gpumem"]; v != "" {
		mem, _ = strconv.Atoi(v)
	}
	if v := limits[resNVIDIAMemPct]; v != "" {
		pct, _ := strconv.Atoi(v)
		mem = s.hami.devmem * pct / 100
	}
	type use struct{ mem, cores, pods int }
	used := map[string]*use{}
	for _, pod := range s.livePods() {
		devs, _ := hamiAllocations(pod)
		for _, d := range devs {
			if used[d.uuid] == nil {
				used[d.uuid] = &use{}
			}
			used[d.uuid].mem += d.memMiB
			used[d.uuid].cores += d.cores
			used[d.uuid].pods++
		}
	}
	var card string
	reason := "CardUuidMismatch"
	for _, e := range s.registered() {
		if want != "" && e.ID != want {
			continue
		}
		u := used[e.ID]
		if u == nil {
			u = &use{}
		}
		switch {
		case u.pods >= e.Count:
			reason = "CardTimeSlicingExhausted"
		case u.mem+mem > e.Devmem:
			reason = "CardInsufficientMemory"
		case u.cores+cores > e.Devcore:
			reason = "CardInsufficientCore"
		default:
			card = e.ID
		}
		if card != "" {
			break
		}
	}
	if card == "" {
		unschedulable(p, "0/1 nodes are available: 1 1/1 "+reason+". no new claims to deallocate, preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.")
		return
	}
	give := hamiDevice{uuid: card, vendor: "NVIDIA", memMiB: mem, cores: cores}
	if s.hami.give != nil {
		give = *s.hami.give
	}
	if p.Metadata.Annotations == nil {
		p.Metadata.Annotations = map[string]string{}
	}
	p.Metadata.Annotations["hami.io/bind-phase"] = "success"
	p.Metadata.Annotations["hami.io/vgpu-devices-allocated"] = fmt.Sprintf("%s,%s,%d,%d:;", give.uuid, give.vendor, give.memMiB, give.cores)
	p.Metadata.Annotations["hami.io/vgpu-devices-to-allocate"] = ";;"
	p.Metadata.Annotations["hami.io/vgpu-node"] = s.node
	if p.Metadata.Labels == nil {
		p.Metadata.Labels = map[string]string{}
	}
	p.Metadata.Labels["hami.io/vgpu-node"] = s.node
	p.Spec.NodeName = s.node
	kubefake.Running(p)
	// The kubelet lists the plugin's device IDs: the UUID and a share number.
	taken := map[string]bool{}
	for _, id := range s.r.kubelet.idsOf(card) {
		taken[id] = true
	}
	n := 0
	for taken[card+"-"+strconv.Itoa(n)] {
		n++
	}
	s.r.kubelet.allocate(p.Metadata.Namespace, p.Metadata.Name, resNVIDIAGPU, []string{card + "-" + strconv.Itoa(n)})
}

// admitACK is ACK GPU sharing's scheduler: whole GiB of one card's memory,
// the lowest-numbered card that has it.
func (s *fakeSched) admitACK(p *kubeapi.Pod, limits map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Spec.SchedulerName != "" || p.Spec.NodeSelector[labelHostname] != s.node {
		p.Status.Phase = "Pending"
		return
	}
	if s.ack.failNext > 0 {
		s.ack.failNext--
		unschedulable(p, "0/1 nodes are available: 1 Insufficient aliyun.com/gpu-mem. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.")
		return
	}
	gib, _ := strconv.Atoi(limits[resACKMem])
	cards := s.r.cardsByIndex()
	usedGiB := map[int]int{}
	for _, pod := range s.livePods() {
		alloc, _ := ackAllocation(pod)
		for idx, g := range alloc {
			usedGiB[idx] += g
		}
	}
	idx := make([]int, 0, len(cards))
	for i := range cards {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	chosen := -1
	for _, i := range idx {
		if s.ack.cardGiB-usedGiB[i] >= gib {
			chosen = i
			break
		}
	}
	if chosen < 0 {
		unschedulable(p, "0/1 nodes are available: 1 Insufficient aliyun.com/gpu-mem. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.")
		return
	}
	if p.Metadata.Annotations == nil {
		p.Metadata.Annotations = map[string]string{}
	}
	p.Metadata.Annotations[ackAllocationAnnotation] = fmt.Sprintf(`{"0":{"%d":%d}}`, chosen, gib)
	p.Metadata.Annotations["gpushare.alibabacloud.com/device-resource"] = fmt.Sprintf(`{"0":%d}`, gib)
	p.Metadata.Annotations["scheduler.framework.gpushare.assigned"] = "true"
	p.Spec.NodeName = s.node
	kubefake.Running(p)
	ids := make([]string, 0, gib)
	for n := 0; n < gib; n++ {
		ids = append(ids, fmt.Sprintf("gpu%d_mem_%d", chosen, usedGiB[chosen]+n))
	}
	s.r.kubelet.allocate(p.Metadata.Namespace, p.Metadata.Name, resACKMem, ids)
}

// cardsByIndex is the node's own probe: card index to UUID.
func (r *rig) cardsByIndex() map[int]string {
	m, _ := r.d.o.Cards(context.Background())
	return m
}

// idsOf are the device IDs the kubelet handed out so far for a card of HAMi's
// resource.
func (k *fakeKubelet) idsOf(card string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for _, a := range k.allocationsLocked() {
		if a.Resource == resNVIDIAGPU && strings.HasPrefix(a.DeviceID, card+"-") {
			out = append(out, a.DeviceID)
		}
	}
	return out
}

// installHAMi makes the node one HAMi 2.11 serves: nvidia.com/gpu is
// shares-per-card times the cards, and the registration annotation lists them.
func installHAMi(t *testing.T, r *rig, s *fakeSched, scheduler string) {
	t.Helper()
	s.hami.on, s.hami.scheduler, s.hami.devmem, s.hami.shares = true, scheduler, 46068, 10
	reg, err := json.Marshal(s.registered())
	if err != nil {
		t.Fatal(err)
	}
	n := strconv.Itoa(len(r.cardsByIndex()) * s.hami.shares)
	r.srv.SetCapacity(testNode, resNVIDIAGPU, n, n)
	r.srv.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register", string(reg))
	r.srv.SetNodeLabel(testNode, "ack.node.gpu.schedule", "hami")
	r.srv.SetNodeLabel(testNode, labelHostname, testNode)
}

// installACK makes the node one ACK GPU sharing serves: GiB of card memory
// and nvidia.com/gpu withdrawn (capacity stays, allocatable 0). A HAMi
// registration from before stays on the node, as it does on a real one.
func installACK(r *rig, s *fakeSched) {
	cards := len(r.cardsByIndex())
	s.ack.on, s.ack.cardGiB = true, 44
	r.srv.SetCapacity(testNode, resNVIDIAGPU, strconv.Itoa(cards), "0")
	r.srv.SetCapacity(testNode, resACKMem, strconv.Itoa(cards*44), strconv.Itoa(cards*44))
	r.srv.SetCapacity(testNode, resACKCount, strconv.Itoa(cards), strconv.Itoa(cards))
	r.srv.SetNodeLabel(testNode, ackScheduleLabel, "cgpu")
	r.srv.SetNodeLabel(testNode, ackMemLabel, "46068MiB")
	r.srv.SetNodeLabel(testNode, labelHostname, testNode)
}

// externalPod creates a pod of another namespace, as a user's would be.
func (r *rig) externalPod(t *testing.T, ns, name, scheduler string, ann map[string]string, limits map[string]string) {
	t.Helper()
	err := r.srv.Client().Create(context.Background(), kubeapi.PodsPath(ns), kubeapi.Pod{
		Metadata: kubeapi.ObjectMeta{Name: name, Annotations: ann},
		Spec: kubeapi.PodSpec{
			SchedulerName: scheduler, NodeSelector: map[string]string{labelHostname: testNode},
			Containers: []kubeapi.Container{{Name: "main", Image: "app:1", Resources: &kubeapi.Resources{Limits: limits}}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

// holdLimits are the limits of the hold pods the platform created, by pod.
func (s *fakeSched) holdCreated() []kubeapi.Pod {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []kubeapi.Pod
	for _, p := range s.created {
		if p.Metadata.Labels[LabelRole] == RoleHold {
			out = append(out, p)
		}
	}
	return out
}
