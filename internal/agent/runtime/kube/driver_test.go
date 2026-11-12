package kube

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi/kubefake"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/podresources"
)

const (
	testNS   = "gpuworks-system"
	testNode = "gpu-node-1"
)

// fakeKubelet stands in for the kubelet's device manager: a pod requesting
// the GPU resource is given free cards in order, or refused admission when
// none is left. Allocations live as long as the pod does.
type fakeKubelet struct {
	mu       sync.Mutex
	srv      *kubefake.Server
	resource string // "" is nvidia.com/gpu
	devices  []string
	given    map[string]string // pod key -> device
	extra    []podresources.Allocation
	// dyn are the allocations of a scheduler's pods (cards_modes_fake_test.go),
	// by pod key; like given they last as long as the pod does.
	dyn     map[string][]podresources.Allocation
	listErr error
}

// allocate records the devices the kubelet gave a pod of a resource a
// scheduler placed.
func (k *fakeKubelet) allocate(ns, pod, resource string, ids []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.dyn == nil {
		k.dyn = map[string][]podresources.Allocation{}
	}
	for _, id := range ids {
		k.dyn[ns+"/"+pod] = append(k.dyn[ns+"/"+pod], podresources.Allocation{Namespace: ns, Pod: pod, Container: "hold", Resource: resource, DeviceID: id})
	}
}

func (k *fakeKubelet) admit(p *kubeapi.Pod) {
	k.mu.Lock()
	defer k.mu.Unlock()
	res := p.Spec.Containers[0].Resources
	if res == nil || res.Limits[k.res()] == "" {
		kubefake.Running(p)
		return
	}
	used := map[string]bool{}
	for _, a := range k.allocationsLocked() {
		used[a.DeviceID] = true
	}
	for _, dev := range k.devices {
		if !used[dev] {
			k.given[p.Metadata.Namespace+"/"+p.Metadata.Name] = dev
			kubefake.Running(p)
			return
		}
	}
	p.Status.Phase, p.Status.Reason, p.Status.Message = "Failed", "UnexpectedAdmissionError", "Allocate failed due to requested number of devices unavailable"
}

func (k *fakeKubelet) allocationsLocked() []podresources.Allocation {
	out := append([]podresources.Allocation(nil), k.extra...)
	live := map[string]bool{}
	if k.srv != nil {
		for _, p := range k.srv.Pods() {
			live[p.Metadata.Namespace+"/"+p.Metadata.Name] = p.Status.Phase != "Failed"
		}
	}
	for key, dev := range k.given {
		if !live[key] {
			continue
		}
		ns, name, _ := strings.Cut(key, "/")
		out = append(out, podresources.Allocation{Namespace: ns, Pod: name, Container: "hold", Resource: k.res(), DeviceID: dev})
	}
	for key, allocs := range k.dyn {
		if live[key] {
			out = append(out, allocs...)
		}
	}
	return out
}

func (k *fakeKubelet) res() string {
	if k.resource == "" {
		return "nvidia.com/gpu"
	}
	return k.resource
}

func (k *fakeKubelet) List(context.Context) ([]podresources.Allocation, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.listErr != nil {
		return nil, k.listErr
	}
	return k.allocationsLocked(), nil
}

type fakeImages struct {
	ImageStore
	execs [][]string
}

func (f *fakeImages) Exec(_ context.Context, id string, cmd []string) ([]byte, error) {
	f.execs = append(f.execs, append([]string{id}, cmd...))
	return []byte("ok"), nil
}

func (f *fakeImages) ExecPTY(_ context.Context, id string, cmd []string) ([]string, error) {
	return append([]string{"nerdctl", "-n", "k8s.io", "exec", "-i", "-t", id}, cmd...), nil
}

type rig struct {
	srv     *kubefake.Server
	kubelet *fakeKubelet
	images  *fakeImages
	d       *Driver
	now     time.Time
}

func newRig(t *testing.T, gpus int) *rig {
	t.Helper()
	cards := map[int]string{}
	var devices []string
	for i := 0; i < gpus; i++ {
		dev := "GPU-" + string(rune('a'+i))
		devices = append(devices, dev)
		cards[i] = dev
	}
	return newRigFor(t, "nvidia.com/gpu", cards, devices)
}

// newRigFor builds a node whose device plugin advertises devices as
// resource, over cards (the node's own probe, index to UUID).
func newRigFor(t *testing.T, resource string, cards map[int]string, devices []string) *rig {
	t.Helper()
	srv := kubefake.New(t, testNS, testNode)
	r := &rig{srv: srv, images: &fakeImages{}, now: time.Unix(1_800_000_000, 0)}
	r.kubelet = &fakeKubelet{srv: srv, given: map[string]string{}, resource: resource, devices: devices}
	if len(devices) > 0 {
		srv.SetAllocatable(testNode, resource, strconv.Itoa(len(devices)))
	}
	srv.Kubelet = r.kubelet.admit
	if err := srv.Client().Create(context.Background(), kubeapi.PodsPath(testNS), kubeapi.Pod{
		Metadata: kubeapi.ObjectMeta{Name: "gpuworks-agent-x1"},
		Spec:     kubeapi.PodSpec{NodeName: testNode, Containers: []kubeapi.Container{{Name: "agent", Image: "gpuworks/gpuworks-agent:v1"}}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	r.d = New(Options{
		API: srv.Client(), Kubelet: r.kubelet, Images: r.images, Namespace: testNS, NodeName: testNode, PodName: "gpuworks-agent-x1",
		RuntimeClass: "nvidia", Cards: func(context.Context) (map[int]string, error) { return cards, nil },
		IdleReturn: 10 * time.Minute, Log: slog.New(slog.DiscardHandler), Poll: time.Millisecond, StartTimeout: 200 * time.Millisecond,
	})
	r.d.cards.now = func() time.Time { return r.now }
	r.d.cards.borrowRetry, r.d.cards.releaseWait = 50*time.Millisecond, 2*time.Second
	if ok, err := r.d.Detect(context.Background()); !ok || err != nil {
		t.Fatalf("Detect = %v, %v", ok, err)
	}
	return r
}

func (r *rig) holdPods() []string {
	var out []string
	for _, p := range r.srv.Pods() {
		if p.Metadata.Labels[LabelRole] == RoleHold {
			out = append(out, p.Metadata.Name)
		}
	}
	return out
}

func spec(name string, gpus ...int) runtime.WorkloadSpec {
	return runtime.WorkloadSpec{Name: name, Image: "img:1", Env: map[string]string{}, GPUIndexes: gpus, HostNetwork: true}
}

func TestRunStatusLogsRemoveWithoutADevicePlugin(t *testing.T) {
	r := newRig(t, 0)
	ctx := context.Background()
	id, err := r.d.Run(ctx, spec("gpuworks-aaaaaaaaaaaa", 0))
	if err != nil || id != "gpuworks-aaaaaaaaaaaa" {
		t.Fatalf("Run = %q, %v", id, err)
	}
	pod := r.srv.Pod(testNS, id)
	if pod == nil || pod.Spec.NodeName != testNode || pod.Metadata.OwnerReferences[0].UID != "ds-uid" || len(r.holdPods()) != 0 {
		t.Fatalf("pod = %+v, hold pods %v", pod, r.holdPods())
	}
	st, err := r.d.Status(ctx, id)
	if err != nil || !st.Exists || !st.Running {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	if names, _ := r.d.RunningContainers(ctx); !slices.Equal(names, []string{id}) {
		t.Fatalf("RunningContainers = %v", names)
	}
	r.srv.Logs[id] = "one\ntwo\nthree\n"
	rc, err := r.d.Logs(ctx, id, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "two\nthree\n" {
		t.Fatalf("Logs = %q", b)
	}
	if out, err := r.d.Exec(ctx, id, []string{"true"}); err != nil || string(out) != "ok" || r.images.execs[0][0] != strings.TrimPrefix(pod.Status.ContainerStatuses[0].ContainerID, "containerd://") {
		t.Fatalf("Exec = %q %v %v", out, err, r.images.execs)
	}
	if err := r.d.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.d.Status(ctx, id); st.Exists {
		t.Fatal("pod exists after Remove")
	}
	if names, _ := r.d.ListContainers(ctx); len(names) != 0 {
		t.Fatalf("ListContainers after Remove = %v", names)
	}
	if err := r.d.Remove(ctx, id); err != nil {
		t.Fatalf("Remove of a gone pod: %v", err)
	}
}

func TestRunReportsWhyAPodDidNotStart(t *testing.T) {
	r := newRig(t, 0)
	r.srv.Kubelet = func(p *kubeapi.Pod) {
		p.Status.ContainerStatuses = []kubeapi.ContainerStatus{{Name: ContainerName, State: kubeapi.ContainerState{Waiting: &kubeapi.StateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}}}}
	}
	_, err := r.d.Run(context.Background(), spec("gpuworks-bbbbbbbbbbbb"))
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("Run = %v", err)
	}
	if r.srv.Pod(testNS, "gpuworks-bbbbbbbbbbbb") != nil {
		t.Fatal("a pod that cannot start was left behind")
	}
	r.srv.Kubelet = func(p *kubeapi.Pod) {} // stays Pending
	if _, err := r.d.Run(context.Background(), spec("gpuworks-cccccccccccc")); err == nil || !strings.Contains(err.Error(), "did not start within") {
		t.Fatalf("Run on a pod that never starts = %v", err)
	}
	r.srv.FailNext["POST /api/v1/namespaces/"+testNS+"/pods"] = 500
	if _, err := r.d.Run(context.Background(), spec("gpuworks-dddddddddddd")); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Run with the API failing = %v", err)
	}
	r.srv.Kubelet = nil
	if _, err := r.d.Run(context.Background(), spec("Bad_Name")); err == nil || !strings.Contains(err.Error(), "Invalid") {
		t.Fatalf("Run with an invalid name = %v", err)
	}
}

func TestStatusOfExitedAndEvictedPods(t *testing.T) {
	r := newRig(t, 0)
	ctx := context.Background()
	if _, err := r.d.Run(ctx, spec("gpuworks-eeeeeeeeeeee")); err != nil {
		t.Fatal(err)
	}
	r.srv.UpdatePod(testNS, "gpuworks-eeeeeeeeeeee", func(p *kubeapi.Pod) {
		p.Status.Phase = "Failed"
		p.Status.ContainerStatuses[0].State = kubeapi.ContainerState{Terminated: &kubeapi.StateTerminated{ExitCode: 137, Reason: "OOMKilled"}}
	})
	if st, _ := r.d.Status(ctx, "gpuworks-eeeeeeeeeeee"); !st.Exists || st.Running || st.ExitCode != 137 || !st.OOMKilled {
		t.Fatalf("OOMKilled status = %+v", st)
	}
	r.srv.UpdatePod(testNS, "gpuworks-eeeeeeeeeeee", func(p *kubeapi.Pod) {
		p.Status = kubeapi.PodStatus{Phase: "Failed", Reason: "Evicted"}
	})
	if st, _ := r.d.Status(ctx, "gpuworks-eeeeeeeeeeee"); !st.Exists || st.Running || st.ExitCode != 1 {
		t.Fatalf("evicted status = %+v", st)
	}
	if names, _ := r.d.RunningContainers(ctx); len(names) != 0 {
		t.Fatalf("RunningContainers with an evicted pod = %v", names)
	}
}

func TestBorrowTakesAllFreeCardsAndKeepsOnlyTheNeededOnes(t *testing.T) {
	r := newRig(t, 4)
	ctx := context.Background()
	// Somebody else's training pod already holds card c.
	r.kubelet.extra = []podresources.Allocation{{Namespace: "ml", Pod: "train-0", Container: "t", Resource: "nvidia.com/gpu", DeviceID: "GPU-c"}}
	if _, err := r.d.Run(ctx, spec("gpuworks-111111111111", 0)); err != nil {
		t.Fatal(err)
	}
	if holds := r.holdPods(); len(holds) != 1 {
		t.Fatalf("hold pods after borrowing card a = %v; want only the one holding it", holds)
	}
	if got := r.srv.Pod(testNS, "gpuworks-111111111111").Metadata.Annotations[AnnotationCards]; got != "GPU-a" {
		t.Fatalf("cards annotation = %q", got)
	}
	holders, err := r.d.ExternalCardHolders(ctx)
	if err != nil || len(holders) != 1 || holders["GPU-c"][0].Name != "Pod ml/train-0" {
		t.Fatalf("external holders = %v, %v", holders, err)
	}
	if _, err := r.d.Run(ctx, spec("gpuworks-222222222222", 2)); err == nil || !strings.Contains(err.Error(), "ml/train-0") {
		t.Fatalf("Run on a card another pod holds = %v", err)
	}
	// A second instance on card a reuses the hold; one on card b borrows it.
	if _, err := r.d.Run(ctx, spec("gpuworks-333333333333", 0, 1)); err != nil {
		t.Fatal(err)
	}
	if holds := r.holdPods(); len(holds) != 2 {
		t.Fatalf("hold pods = %v; want cards a and b", holds)
	}
	// Stopping the instances starts the grace period; the sweep returns the
	// cards only after it.
	for _, id := range []string{"gpuworks-111111111111", "gpuworks-333333333333"} {
		if err := r.d.Remove(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	r.d.cards.sweep(ctx)
	if holds := r.holdPods(); len(holds) != 2 {
		t.Fatalf("hold pods right after the instances stopped = %v", holds)
	}
	r.now = r.now.Add(11 * time.Minute)
	r.d.cards.sweep(ctx)
	if holds := r.holdPods(); len(holds) != 0 {
		t.Fatalf("hold pods after the grace period = %v", holds)
	}
}

func TestBorrowFailsWhenAnotherPodWinsTheRace(t *testing.T) {
	r := newRig(t, 2)
	// The kubelet hands card a to somebody else between our count and our
	// hold pods' admission: only one hold pod is admitted, and it gets b.
	admit := r.kubelet.admit
	first := true
	r.srv.Kubelet = func(p *kubeapi.Pod) {
		if first && strings.HasPrefix(p.Metadata.Name, holdPrefix) {
			first = false
			r.kubelet.mu.Lock()
			r.kubelet.extra = []podresources.Allocation{{Namespace: "ml", Pod: "sneaky", Resource: "nvidia.com/gpu", DeviceID: "GPU-a"}}
			r.kubelet.mu.Unlock()
		}
		admit(p)
	}
	// The first attempt is short of the card; the retry sees who has it.
	_, err := r.d.Run(context.Background(), spec("gpuworks-444444444444", 0))
	if err == nil || !strings.Contains(err.Error(), "card GPU-a is allocated to Pod ml/sneaky by Kubernetes") {
		t.Fatalf("Run = %v", err)
	}
	for _, p := range r.srv.Pods() {
		if p.Metadata.Labels[LabelRole] == RoleHold {
			t.Fatalf("hold pod %s left behind (phase %s)", p.Metadata.Name, p.Status.Phase)
		}
	}
}

func TestKeepAllAndRecoveryAfterRestart(t *testing.T) {
	r := newRig(t, 3)
	ctx := context.Background()
	r.d.SetIdleReturn(-1)
	if err := r.d.cards.keepAll(ctx); err != nil {
		t.Fatal(err)
	}
	if holds := r.holdPods(); len(holds) != 3 {
		t.Fatalf("hold pods when keeping = %v", holds)
	}
	if _, err := r.d.Run(ctx, spec("gpuworks-555555555555", 1)); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(24 * time.Hour)
	r.d.cards.sweep(ctx)
	if holds := r.holdPods(); len(holds) != 3 {
		t.Fatalf("kept cards returned: %v", holds)
	}
	// A new agent (restart) finds the holds and the instance's cards.
	d2 := New(r.d.o)
	d2.cards.now = func() time.Time { return r.now }
	if ok, err := d2.Detect(ctx); !ok || err != nil {
		t.Fatal(err)
	}
	if err := d2.cards.load(ctx); err != nil {
		t.Fatal(err)
	}
	if len(d2.cards.held) != 3 || !d2.cards.users["GPU-b"]["gpuworks-555555555555"] || len(d2.cards.idleSince) != 2 {
		t.Fatalf("recovered pool: held=%v users=%v idle=%v", d2.cards.held, d2.cards.users, d2.cards.idleSince)
	}
}

func TestPodResourcesFailureIsReported(t *testing.T) {
	r := newRig(t, 2)
	r.kubelet.listErr = io.ErrUnexpectedEOF
	if _, err := r.d.Run(context.Background(), spec("gpuworks-666666666666", 0)); err == nil {
		t.Fatal("borrowing without the kubelet's answer succeeded")
	}
	if r.srv.Pod(testNS, "gpuworks-666666666666") != nil {
		t.Fatal("pod created without its cards")
	}
	if _, err := r.d.ExternalCardHolders(context.Background()); err == nil {
		t.Fatal("ExternalCardHolders hid the kubelet error")
	}
}

func TestUnschedulableReadsTheNode(t *testing.T) {
	r := newRig(t, 0)
	if v, err := r.d.Unschedulable(context.Background()); err != nil || v {
		t.Fatalf("Unschedulable = %v, %v", v, err)
	}
	r.srv.SetUnschedulable(testNode, true)
	if v, err := r.d.Unschedulable(context.Background()); err != nil || !v {
		t.Fatalf("Unschedulable after cordon = %v, %v", v, err)
	}
	r.srv.FailNext["GET /api/v1/nodes/"] = 503
	if _, err := r.d.Unschedulable(context.Background()); err == nil {
		t.Fatal("an API failure read as a node state")
	}
}

func TestAgentOnFindsTheEntrysAgentPodOnANode(t *testing.T) {
	r := newRig(t, 0)
	ctx := context.Background()
	api := r.srv.Client()
	for _, p := range []kubeapi.Pod{
		{Metadata: kubeapi.ObjectMeta{Name: "agent-a", Namespace: testNS, Labels: map[string]string{"app.kubernetes.io/name": DaemonSetName}}, Spec: kubeapi.PodSpec{NodeName: "node-a", Containers: []kubeapi.Container{{Name: "agent", Image: "agent"}}}},
		{Metadata: kubeapi.ObjectMeta{Name: "other", Namespace: testNS, Labels: map[string]string{"app.kubernetes.io/name": "else"}}, Spec: kubeapi.PodSpec{NodeName: "node-b", Containers: []kubeapi.Container{{Name: "x", Image: "x"}}}},
		{Metadata: kubeapi.ObjectMeta{Name: "agent-c", Namespace: "another-entry", Labels: map[string]string{"app.kubernetes.io/name": DaemonSetName}}, Spec: kubeapi.PodSpec{NodeName: "node-c", Containers: []kubeapi.Container{{Name: "agent", Image: "agent"}}}},
	} {
		if err := api.Create(ctx, kubeapi.PodsPath(p.Metadata.Namespace), p, nil); err != nil {
			t.Fatal(err)
		}
	}
	for node, want := range map[string]bool{"node-a": true, "node-b": false, "node-c": false} {
		if got, err := r.d.AgentOn(ctx, node); err != nil || got != want {
			t.Fatalf("AgentOn(%s) = %v, %v; want %v", node, got, err, want)
		}
	}
	r.srv.FailNext["GET /api/v1/namespaces/"] = 500
	if _, err := r.d.AgentOn(ctx, "node-a"); err == nil {
		t.Fatal("an API failure read as an answer")
	}
}

func TestNodeExists(t *testing.T) {
	r := newRig(t, 0)
	if ok, err := r.d.NodeExists(context.Background(), testNode); !ok || err != nil {
		t.Fatalf("own node = %v, %v", ok, err)
	}
	if ok, err := r.d.NodeExists(context.Background(), "gone-node"); ok || err != nil {
		t.Fatalf("missing node = %v, %v", ok, err)
	}
	r.srv.FailNext["GET /api/v1/nodes/"] = 500
	if _, err := r.d.NodeExists(context.Background(), "x"); err == nil {
		t.Fatal("an API failure read as an answer")
	}
}
