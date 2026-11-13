package kube

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/podresources"
)

// newHAMiRig is a node of gpus cards that HAMi 2.11 serves: the plain device
// plugin is gone, nvidia.com/gpu counts ten shares a card, and hami-scheduler
// assigns cards.
func newHAMiRig(t *testing.T, gpus int) (*rig, *fakeSched) {
	t.Helper()
	r := newRig(t, gpus)
	r.kubelet.devices = nil
	s := installSched(r)
	installHAMi(t, r, s, DefaultHAMiScheduler)
	r.d.cards.podsTTL, r.d.cards.scheduleWait = 0, 300*time.Millisecond
	return r, s
}

// The hold pod goes through HAMi's scheduler, names the card and asks for all
// of it (the shape that worked on a real cluster); it is not bound with
// nodeName, which would bypass the scheduler HAMi's device plugin depends on.
// Only the wanted card is borrowed.
func TestHAMiHoldPodNamesTheCardAndGoesThroughHAMisScheduler(t *testing.T) {
	r, s := newHAMiRig(t, 2)
	if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 1)); err != nil {
		t.Fatal(err)
	}
	holds := s.holdCreated()
	if len(holds) != 1 {
		t.Fatalf("hold pods created = %d, want only the wanted card's", len(holds))
	}
	h := holds[0]
	if h.Spec.NodeName != "" || h.Spec.SchedulerName != "hami-scheduler" || !maps.Equal(h.Spec.NodeSelector, map[string]string{"kubernetes.io/hostname": testNode}) {
		t.Fatalf("hold pod placement: nodeName %q scheduler %q selector %v", h.Spec.NodeName, h.Spec.SchedulerName, h.Spec.NodeSelector)
	}
	res := h.Spec.Containers[0].Resources
	if !maps.Equal(res.Limits, map[string]string{"nvidia.com/gpu": "1", "nvidia.com/gpucores": "100", "nvidia.com/gpumem-percentage": "100"}) {
		t.Fatalf("limits = %v", res.Limits)
	}
	if _, ok := res.Requests["nvidia.com/gpu"]; ok || res.Requests["cpu"] == "" {
		t.Fatalf("requests = %v: only limits ask for the card, as on the cluster the shape was proven on", res.Requests)
	}
	if !maps.Equal(h.Metadata.Annotations, map[string]string{"nvidia.com/use-gpuuuid": "GPU-b"}) {
		t.Fatalf("annotations = %v", h.Metadata.Annotations)
	}
	// HAMi gave it the card, whole, and the platform owns it by what HAMi
	// wrote, though the kubelet's device ID ends in a share number.
	pod := r.srv.Pod(testNS, h.Metadata.Name)
	if got := pod.Metadata.Annotations["hami.io/vgpu-devices-allocated"]; got != "GPU-b,NVIDIA,46068,100:;" {
		t.Fatalf("allocation = %q", got)
	}
	if allocs, _ := r.kubelet.List(context.Background()); len(allocs) != 1 || allocs[0].DeviceID != "GPU-b-0" || allocs[0].Resource != "nvidia.com/gpu" {
		t.Fatalf("kubelet allocations = %+v", allocs)
	}
	if !reflect.DeepEqual(r.d.cards.held, map[string]string{"GPU-b": h.Metadata.Name}) {
		t.Fatalf("held = %v", r.d.cards.held)
	}
	// An agent that restarted finds the same ownership.
	d2 := New(r.d.o)
	if ok, err := d2.Detect(context.Background()); !ok || err != nil {
		t.Fatal(err)
	}
	if err := d2.cards.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d2.cards.held, map[string]string{"GPU-b": h.Metadata.Name}) {
		t.Fatalf("recovered held = %v", d2.cards.held)
	}
	// The instance asks for no HAMi resource: its pod gets the card the way
	// docker containers do, and HAMi's limits are not injected.
	inst := r.srv.Pod(testNS, "gpuworks-aaaaaaaaaaaa")
	for name := range inst.Spec.Containers[0].Resources.Limits {
		if strings.HasPrefix(name, "nvidia.com/") {
			t.Fatalf("instance pod asks for %s", name)
		}
	}
}

// A device ID of HAMi's plugin is the card's UUID and a share number; ACK's
// names the card number.
func TestCardOfReadsEveryDevicePluginsIDs(t *testing.T) {
	cards := map[int]string{0: "GPU-5b6f2aeb-610b-e00f-59bc-8bc6f15434fb", 1: "GPU-8a0f"}
	for id, want := range map[string]string{
		"GPU-5b6f2aeb-610b-e00f-59bc-8bc6f15434fb":   "GPU-5b6f2aeb-610b-e00f-59bc-8bc6f15434fb",
		"GPU-5b6f2aeb-610b-e00f-59bc-8bc6f15434fb-9": "GPU-5b6f2aeb-610b-e00f-59bc-8bc6f15434fb",
		"GPU-8a0f-0":    "GPU-8a0f",
		"gpu1_mem_43":   "GPU-8a0f",
		"gpu0_mem_0":    "GPU-5b6f2aeb-610b-e00f-59bc-8bc6f15434fb",
		"gpu7_mem_0":    "",
		"Ascend910-1":   "GPU-8a0f", // an index at the end
		"GPU-unknown-3": "",
		"nothing":       "",
		"GPU-5b6f2aeb-610b-e00f-59bc-8bc6f15434fb-x-3": "",
	} {
		if got := cardOf(id, cards); got != want {
			t.Errorf("cardOf(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestParseHAMiAllocated(t *testing.T) {
	devs, err := parseHAMiAllocated("GPU-5b6f,NVIDIA,46068,100:;")
	if err != nil || len(devs) != 1 || devs[0] != (hamiDevice{uuid: "GPU-5b6f", vendor: "NVIDIA", memMiB: 46068, cores: 100}) {
		t.Fatalf("one card = %+v, %v", devs, err)
	}
	devs, err = parseHAMiAllocated("GPU-a,NVIDIA,8000,30:GPU-b,NVIDIA,100,0:;GPU-c,NVIDIA,1,1:;")
	if err != nil || len(devs) != 3 || devs[1].uuid != "GPU-b" || devs[2].uuid != "GPU-c" {
		t.Fatalf("two containers = %+v, %v", devs, err)
	}
	if devs, err := parseHAMiAllocated(";;"); err != nil || len(devs) != 0 {
		t.Fatalf("nothing allocated = %+v, %v", devs, err)
	}
	for _, bad := range []string{"GPU-a,NVIDIA,8000:;", "GPU-a,NVIDIA,x,30:;", ",NVIDIA,1,1:;"} {
		if _, err := parseHAMiAllocated(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// HAMi giving something other than the whole card asked for is not the card
// owned: the hold pod goes back with the reason, and no instance is placed.
func TestHAMiBorrowChecksTheCardIsGivenWhole(t *testing.T) {
	for name, tc := range map[string]struct {
		give hamiDevice
		want string
	}{
		"another card":   {hamiDevice{uuid: "GPU-b", vendor: "NVIDIA", memMiB: 46068, cores: 100}, "HAMi gave the hold pod GPU-b instead of card GPU-a"},
		"part of memory": {hamiDevice{uuid: "GPU-a", vendor: "NVIDIA", memMiB: 8000, cores: 100}, "HAMi gave only 8000 of card GPU-a's 46068 MiB of memory"},
		"part of cores":  {hamiDevice{uuid: "GPU-a", vendor: "NVIDIA", memMiB: 46068, cores: 30}, "HAMi gave only 30% of card GPU-a's compute"},
	} {
		t.Run(name, func(t *testing.T) {
			r, s := newHAMiRig(t, 2)
			s.hami.give = &tc.give
			r.d.cards.borrowRetry = time.Minute // a final answer is not retried
			started := time.Now()
			_, err := r.d.Run(context.Background(), spec("gpuworks-bbbbbbbbbbbb", 0))
			if err == nil || !strings.Contains(err.Error(), tc.want) || time.Since(started) > 5*time.Second {
				t.Fatalf("Run = %v after %s", err, time.Since(started))
			}
			if holds := r.holdPods(); len(holds) != 0 {
				t.Fatalf("hold pods left behind: %v", holds)
			}
			if len(r.d.cards.users) != 0 || len(r.d.cards.held) != 0 {
				t.Fatalf("pool after the failed borrow: users %v held %v", r.d.cards.users, r.d.cards.held)
			}
			if r.srv.Pod(testNS, "gpuworks-bbbbbbbbbbbb") != nil {
				t.Fatal("an instance pod was created on a card that is not the platform's")
			}
		})
	}
}

// A pod HAMi gave part of a card (the annotation shape of the cluster) is an
// external holder with what it holds, and the kubelet's own entry for the
// same pod is not a second holder.
func TestHAMiExternalHoldersShowWhatEachPodHolds(t *testing.T) {
	r, _ := newHAMiRig(t, 2)
	r.externalPod(t, "ml", "partial", DefaultHAMiScheduler, map[string]string{"nvidia.com/use-gpuuuid": "GPU-a"},
		map[string]string{"nvidia.com/gpu": "1", "nvidia.com/gpucores": "30", "nvidia.com/gpumem": "8000"})
	if got := r.srv.Pod("ml", "partial").Metadata.Annotations["hami.io/vgpu-devices-allocated"]; got != "GPU-a,NVIDIA,8000,30:;" {
		t.Fatalf("the fake's allocation = %q, want the shape seen on the cluster", got)
	}
	// A pod from before HAMi, known to the kubelet only.
	r.kubelet.mu.Lock()
	r.kubelet.extra = append(r.kubelet.extra, podAlloc("ml", "legacy", "nvidia.com/gpu", "GPU-b-3"))
	r.kubelet.mu.Unlock()

	held, err := r.d.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := held["GPU-a"]
	if len(a) != 1 || a[0].Name != "Pod ml/partial (8000 MiB, 30% of the compute)" || a[0].MemoryBytes != 8000<<20 {
		t.Fatalf("GPU-a holders = %+v", a)
	}
	if b := held["GPU-b"]; len(b) != 1 || b[0].Name != "Pod ml/legacy" {
		t.Fatalf("GPU-b holders = %+v", b)
	}
	// The platform's own hold pod is not an outsider.
	// A pod only the kubelet knows is not in HAMi's books, so the scheduler
	// cannot keep the card from the platform: it is refused at once.
	if _, err := r.d.Run(context.Background(), spec("gpuworks-cccccccccccc", 1)); err == nil || !strings.Contains(err.Error(), "Pod ml/legacy") || !strings.Contains(err.Error(), "outside HAMi's books") {
		t.Fatalf("Run on a card a pod outside HAMi's books holds = %v", err)
	}
}

// A service account without the cluster-wide pod list (a manifest of an
// earlier version) still gets the kubelet's view, and no error.
func TestHAMiExternalHoldersFallBackToTheKubeletWithoutPodListPermission(t *testing.T) {
	r, _ := newHAMiRig(t, 2)
	r.externalPod(t, "ml", "partial", DefaultHAMiScheduler, nil, map[string]string{"nvidia.com/gpu": "1", "nvidia.com/gpucores": "30", "nvidia.com/gpumem": "8000"})
	r.srv.DenyClusterPods = true
	held, err := r.d.ExternalCardHolders(context.Background())
	if err != nil {
		t.Fatalf("ExternalCardHolders = %v", err)
	}
	if a := held["GPU-a"]; len(a) != 1 || a[0].Name != "Pod ml/partial" || a[0].MemoryBytes != 0 {
		t.Fatalf("GPU-a holders = %+v, want the pod by name only", a)
	}
}

// A card another pod holds part of cannot be given whole: the scheduler leaves
// the hold pod Pending, and the error carries its reason and names the pods
// that hold the card. Another card is still borrowable.
func TestHAMiBorrowFailsWithTheSchedulersReasonAndTheHoldingPods(t *testing.T) {
	r, _ := newHAMiRig(t, 2)
	r.d.cards.borrowRetry = time.Minute
	r.externalPod(t, "ml", "partial", DefaultHAMiScheduler, map[string]string{"nvidia.com/use-gpuuuid": "GPU-a"},
		map[string]string{"nvidia.com/gpu": "1", "nvidia.com/gpucores": "30", "nvidia.com/gpumem": "8000"})
	started := time.Now()
	_, err := r.d.Run(context.Background(), spec("gpuworks-dddddddddddd", 0))
	if err == nil || time.Since(started) > 5*time.Second {
		t.Fatalf("Run = %v after %s", err, time.Since(started))
	}
	for _, want := range []string{"GPU-a", "CardInsufficientMemory", "Pod ml/partial (8000 MiB, 30% of the compute)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not say %q", err, want)
		}
	}
	if len(r.holdPods()) != 0 || len(r.d.cards.users) != 0 {
		t.Fatalf("a Pending hold pod or a user was left: %v %v", r.holdPods(), r.d.cards.users)
	}
	if _, err := r.d.Run(context.Background(), spec("gpuworks-eeeeeeeeeeee", 1)); err != nil {
		t.Fatalf("the card nobody uses is not borrowable: %v", err)
	}
}

// The scheduler may still count a hold pod that was just deleted (an idle
// sweep does not wait for it): that is not another pod holding the card, and
// the borrow is tried again until the pod is gone.
func TestHAMiBorrowRetriesWhileTheSchedulerStillCountsAReturnedPod(t *testing.T) {
	r, _ := newHAMiRig(t, 1)
	r.srv.TerminationDelay = 150 * time.Millisecond
	r.d.cards.borrowRetry = 5 * time.Second
	ctx := context.Background()
	r.d.SetIdleReturn(time.Minute)
	if _, err := r.d.Run(ctx, spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Remove(ctx, "gpuworks-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(2 * time.Minute)
	r.d.cards.sweep(ctx)
	if _, err := r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 0)); err != nil {
		t.Fatalf("Run right after the idle card was returned = %v", err)
	}
}

// An idle card is given back by deleting its hold pod, and borrowed again by
// a new one: the scheduler's books are the pods', so nothing else is kept.
func TestHAMiIdleCardIsReturnedAndBorrowedAgain(t *testing.T) {
	r, _ := newHAMiRig(t, 2)
	ctx := context.Background()
	r.d.SetIdleReturn(time.Minute)
	if _, err := r.d.Run(ctx, spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatal(err)
	}
	if err := r.d.Remove(ctx, "gpuworks-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(2 * time.Minute)
	r.d.cards.sweep(ctx)
	if holds := r.holdPods(); len(holds) != 0 {
		t.Fatalf("hold pods after the idle time = %v", holds)
	}
	if rel, why := r.d.HoldCardForCheck(ctx, "GPU-a"); rel != nil || !strings.Contains(why, "Kubernetes allocates card GPU-a") {
		t.Fatalf("a returned card is held for a check: %v %q", rel != nil, why)
	}
	if _, err := r.d.Run(ctx, spec("gpuworks-bbbbbbbbbbbb", 0)); err != nil {
		t.Fatalf("Run on the returned card = %v", err)
	}
}

// The scheduler's name is the cluster's: a HAMi installed under another name
// is reached through GPUWORKS_KUBE_HAMI_SCHEDULER, and a hold pod that asks
// for a name nobody serves says so.
func TestHAMiSchedulerNameIsConfigurable(t *testing.T) {
	if got := New(Options{}).cards.hamiScheduler; got != "hami-scheduler" {
		t.Fatalf("default scheduler = %q", got)
	}
	if got := New(Options{HAMiScheduler: "my-hami"}).cards.hamiScheduler; got != "my-hami" {
		t.Fatalf("configured scheduler = %q", got)
	}
	r, s := newHAMiRig(t, 1)
	s.hami.scheduler = "my-hami" // the cluster named it so
	r.d.cards.scheduleWait, r.d.cards.borrowRetry = 100*time.Millisecond, 10*time.Millisecond
	_, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 0))
	if err == nil || !strings.Contains(err.Error(), "neither scheduled nor refused") || !strings.Contains(err.Error(), `"hami-scheduler"`) || !strings.Contains(err.Error(), "GPUWORKS_KUBE_HAMI_SCHEDULER") {
		t.Fatalf("Run with the wrong scheduler name = %v", err)
	}
	if len(r.holdPods()) != 0 {
		t.Fatalf("Pending hold pods left: %v", r.holdPods())
	}
	r.d.cards.hamiScheduler = "my-hami"
	if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatalf("Run with the configured name = %v", err)
	}
	if got := s.holdCreated()[len(s.holdCreated())-1].Spec.SchedulerName; got != "my-hami" {
		t.Fatalf("hold pod scheduler = %q", got)
	}
}

// A cluster that keeps cards takes each one through HAMi, one by name.
func TestHAMiKeepAllBorrowsEveryCardByName(t *testing.T) {
	r, s := newHAMiRig(t, 3)
	r.d.SetIdleReturn(-1)
	if err := r.d.cards.keepAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.holdPods()) != 3 {
		t.Fatalf("hold pods = %v", r.holdPods())
	}
	named := map[string]bool{}
	for _, p := range s.holdCreated() {
		named[p.Metadata.Annotations["nvidia.com/use-gpuuuid"]] = true
	}
	if len(named) != 3 || !named["GPU-a"] || !named["GPU-b"] || !named["GPU-c"] {
		t.Fatalf("cards named = %v", named)
	}
	// Once held, a second pass takes nothing.
	if err := r.d.cards.keepAll(context.Background()); err != nil || len(r.holdPods()) != 3 {
		t.Fatalf("second keepAll = %v, hold pods %v", err, r.holdPods())
	}
}

// A card HAMi did not register (or marked unhealthy) is refused at once with
// that reason, not left to a Pending pod.
func TestHAMiBorrowRefusesACardHAMiDoesNotList(t *testing.T) {
	r, _ := newHAMiRig(t, 2)
	r.srv.SetNodeAnnotation(testNode, "hami.io/node-nvidia-register",
		`[{"id":"GPU-a","count":10,"devmem":46068,"devcore":100,"type":"NVIDIA L20","health":true},{"id":"GPU-b","count":10,"devmem":46068,"devcore":100,"type":"NVIDIA L20","health":false}]`)
	r.srv.SetCapacity(testNode, "nvidia.com/gpu", "10", "10")
	_, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 1))
	if err == nil || !strings.Contains(err.Error(), "HAMi does not list card GPU-b as a healthy card") {
		t.Fatalf("Run on an unhealthy card = %v", err)
	}
	if len(r.holdPods()) != 0 {
		t.Fatalf("hold pods: %v", r.holdPods())
	}
	if _, err := r.d.Run(context.Background(), spec("gpuworks-aaaaaaaaaaaa", 0)); err != nil {
		t.Fatalf("the healthy card = %v", err)
	}
}

func podAlloc(ns, pod, resource, id string) podresources.Allocation {
	return podresources.Allocation{Namespace: ns, Pod: pod, Container: "main", Resource: resource, DeviceID: id}
}

// HAMi on Ascend: the shape follows HAMi's documented registration and
// annotations (huawei.com/<model>: 1 with no memory is a whole card, the
// card named by hami.io/use-<model>-uuid) and is checked against fixtures
// only; it has not been run on hardware.
func TestHAMiAscendHoldPodAndWholeCardCheck(t *testing.T) {
	cards := map[int]string{0: "Ascend910B4-0", 1: "Ascend910B4-1"}
	r := newRigFor(t, "huawei.com/Ascend910B4", cards, nil)
	r.srv.SetCapacity(testNode, "huawei.com/Ascend910B4", "32", "32")
	r.srv.SetNodeAnnotation(testNode, "hami.io/node-ascend-register",
		`[{"id":"Ascend910B4-0","count":16,"devmem":32768,"devcore":100,"type":"Ascend910B4","health":true},`+
			`{"id":"Ascend910B4-1","count":16,"devmem":32768,"devcore":100,"type":"Ascend910B4","health":true}]`)
	v, err := r.d.cards.refresh(context.Background())
	if err != nil || v.mode != modeHAMi || v.resource != "huawei.com/Ascend910B4" || v.hami.vendor != accelerator.VendorAscend {
		t.Fatalf("view = %+v, %v", v, err)
	}
	pod := r.d.cards.hamiHoldPod(v, "gpuworks-hold-x", "Ascend910B4-1")
	res := pod.Spec.Containers[0].Resources
	if !maps.Equal(res.Limits, map[string]string{"huawei.com/Ascend910B4": "1"}) ||
		!maps.Equal(pod.Metadata.Annotations, map[string]string{"hami.io/use-Ascend910B4-uuid": "Ascend910B4-1"}) ||
		pod.Spec.SchedulerName != "hami-scheduler" || pod.Spec.NodeName != "" {
		t.Fatalf("hold pod = limits %v annotations %v scheduler %q nodeName %q", res.Limits, pod.Metadata.Annotations, pod.Spec.SchedulerName, pod.Spec.NodeName)
	}
	allocated := func(mem string) kubeapi.Pod {
		return kubeapi.Pod{Metadata: kubeapi.ObjectMeta{Annotations: map[string]string{"hami.io/Ascend910B4-devices-allocated": "Ascend910B4-1,Ascend910B4," + mem + ",0:;"}}}
	}
	if err := v.hami.verifyWhole(allocated("32768"), "Ascend910B4-1"); err != nil {
		t.Fatalf("whole card = %v", err)
	}
	if err := v.hami.verifyWhole(allocated("16384"), "Ascend910B4-1"); err == nil || !strings.Contains(err.Error(), "only 16384 of card Ascend910B4-1's 32768 MiB") {
		t.Fatalf("half the memory = %v", err)
	}
}
