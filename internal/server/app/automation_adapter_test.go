package app

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/lifecycle"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/placewait"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// automationFixture is one node with one 24 GiB GPU and a sleepable vLLM
// deployment whose one instance runs on it, behind the real adapter.
func automationFixture(t *testing.T) (*automationAdapter, *store.Store, *store.Deployment) {
	t.Helper()
	st, err := store.Open("file:automation_adapter_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("auto-node", store.HashToken("token"), "auto-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "gpu-0", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 24 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto", Name: "auto", WeightBytes: 2 << 30, Dtype: "bfloat16"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "auto", ModelID: model.ID, NodeID: node.ID, ServedName: "auto", Engine: "vllm", Sleepable: true, State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "auto-1", DesiredState: "running", Sleepable: true})
	if _, err := st.CreateInstance(&store.Instance{UUID: "auto-1", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	adapter := &automationAdapter{store: st, now: time.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)), orders: &lifecycle.Orders{}}
	return adapter, st, dep
}

func mustInstance(t *testing.T, st *store.Store, uuid string) *store.Instance {
	t.Helper()
	in, err := st.InstanceByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func intentOf(t *testing.T, st *store.Store, uuid string) int {
	t.Helper()
	in, err := st.InstanceByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return in.SleepIntent()
}

// The adapter's sleep is the platform's unless held, a schedule's.
func TestAutomationAdapterSleeps(t *testing.T) {
	adapter, st, dep := automationFixture(t)
	if level, err := adapter.Sleep(dep.ID, 1, false); err != nil || level != 1 {
		t.Fatalf("sleep: %d %v", level, err)
	}
	if got := intentOf(t, st, "auto-1"); got != 1 {
		t.Fatalf("intent after the automation's sleep = %d, want 1", got)
	}
	if mustInstance(t, st, "auto-1").SleepHeld {
		t.Fatal("a platform sleep is held: a request would not wake it")
	}
	if _, err := adapter.Sleep(dep.ID, 2, true); err != nil {
		t.Fatalf("held sleep: %v", err)
	}
	if !mustInstance(t, st, "auto-1").SleepHeld {
		t.Fatal("a schedule's sleep is not held")
	}
}

// With host memory reported too small for the weights, an L1 sleep is
// refused with its code and nothing changes.
func TestAutomationAdapterRefusesL1WhenTheHostCannotHoldTheWeights(t *testing.T) {
	adapter, st, dep := automationFixture(t)
	metrics, err := tsdb.OpenSQLite(t.TempDir() + "/metrics.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metrics.Close() })
	node, _ := st.NodeByID(dep.NodeID)
	now := time.Now().UnixMilli()
	if err := metrics.WriteBatch([]tsdb.Sample{
		{Metric: "gpuworks_node_mem_total_bytes", Labels: map[string]string{"node": node.UUID}, Value: float64(8 << 30), TimeMS: now},
		{Metric: "gpuworks_node_mem_used_bytes", Labels: map[string]string{"node": node.UUID}, Value: float64(7 << 30), TimeMS: now},
	}); err != nil {
		t.Fatal(err)
	}
	// The reader the app wires: the metrics store through planning's host memory.
	adapter.host = (&observability{tsdb: metrics}).hostMemory()
	_, err = adapter.Sleep(dep.ID, 1, true)
	coded, ok := err.(interface{ Code() string })
	if !ok || coded.Code() != "SLEEP_HOST_MEMORY_INSUFFICIENT" {
		t.Fatalf("L1 onto a full host = %v, want SLEEP_HOST_MEMORY_INSUFFICIENT", err)
	}
	if got := intentOf(t, st, "auto-1"); got != 0 {
		t.Fatalf("a refused sleep changed the intent to %d", got)
	}
	if _, err := adapter.Sleep(dep.ID, 2, true); err != nil || intentOf(t, st, "auto-1") != 2 {
		t.Fatalf("L2 onto a full host: err=%v intent=%d", err, intentOf(t, st, "auto-1"))
	}
	// The auto level is L1 where the host holds the weights and L2 where it
	// cannot: the order is not refused.
	if err := st.SetDeploymentSleep(dep.ID, 0); err != nil {
		t.Fatal(err)
	}
	if level, err := adapter.Sleep(dep.ID, automation.LevelAuto, false); err != nil || level != proto.SleepLevelL2 || intentOf(t, st, "auto-1") != 2 {
		t.Fatalf("auto onto a full host = %d err=%v intent=%d, want L2", level, err, intentOf(t, st, "auto-1"))
	}
}

func TestAutomationAdapterAutoLevelSleepsAtL1WhenTheHostHasRoom(t *testing.T) {
	adapter, st, dep := automationFixture(t)
	if level, err := adapter.Sleep(dep.ID, automation.LevelAuto, false); err != nil || level != proto.SleepLevelL1 || intentOf(t, st, "auto-1") != 1 {
		t.Fatalf("auto = %d err=%v intent=%d, want L1", level, err, intentOf(t, st, "auto-1"))
	}
}

// A scheduled wake that does not fit leaves the sleeper waiting; once the card
// frees up, the waiting wake wakes it; schedules read back as the automation
// sees them.
func TestAutomationAdapterWakeWaitsAndWakesWhenItFits(t *testing.T) {
	adapter, st, dep := automationFixture(t)
	if err := st.SetDeploymentSleep(dep.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "auto-1", State: proto.InstRunning, SleepLevel: 2}); err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateDeployment(&store.Deployment{Name: "other", ModelID: dep.ModelID, NodeID: dep.NodeID, ServedName: "other", Engine: "vllm", State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "other-1", DesiredState: "running"})
	if _, err := st.CreateInstance(&store.Instance{UUID: "other-1", DeploymentID: other.ID, NodeID: dep.NodeID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Wake(dep.ID, true)
	if err != nil || len(result.Woken) != 0 || len(result.Waiting) != 1 || result.Waiting[0] != "auto-1" {
		t.Fatalf("wake onto a taken card = %+v err=%v", result, err)
	}
	waits, err := adapter.WaitingWakes()
	if err != nil || len(waits) != 1 || waits[0].DeploymentID != dep.ID {
		t.Fatalf("waiting wakes = %+v err=%v", waits, err)
	}
	if woken, err := adapter.WakeWaiting(dep.ID, waits[0].Instances); err != nil || len(woken) != 0 {
		t.Fatalf("woke while the card is taken: %v err=%v", woken, err)
	}
	if err := st.SetInstanceState("other-1", proto.InstStopped, "stopped"); err != nil {
		t.Fatal(err)
	}
	woken, err := adapter.WakeWaiting(dep.ID, waits[0].Instances)
	if err != nil || len(woken) != 1 || intentOf(t, st, "auto-1") != 0 {
		t.Fatalf("waiting wake after the card freed = %v err=%v intent=%d", woken, err, intentOf(t, st, "auto-1"))
	}
	if waits, _ := adapter.WaitingWakes(); len(waits) != 0 {
		t.Fatalf("still waiting after the wake: %+v", waits)
	}

	sc := &store.LifecycleSchedule{DeploymentID: dep.ID, Action: store.ScheduleActionSleep, Weekdays: automation.AllWeekdays, Minute: 60, Timezone: "Asia/Shanghai", SleepLevel: 2, Enabled: true}
	if err := st.CreateLifecycleSchedule(sc); err != nil {
		t.Fatal(err)
	}
	broken := &store.LifecycleSchedule{DeploymentID: dep.ID, Action: store.ScheduleActionWake, Weekdays: 1, Minute: 0, Timezone: "Nowhere/Never", Enabled: true}
	if err := st.CreateLifecycleSchedule(broken); err != nil {
		t.Fatal(err)
	}
	schedules, err := adapter.Schedules()
	if err != nil || len(schedules) != 1 || schedules[0].Location.String() != "Asia/Shanghai" || schedules[0].LastFiredAt.IsZero() {
		t.Fatalf("schedules = %+v err=%v (an unknown zone is skipped)", schedules, err)
	}
}

// A replica schedule goes through the API's own scaling: a single-node
// deployment records the count, and a multi-node deployment whose replicas the
// operator placed by hand is refused with the code the failure event carries,
// leaving the deployment as it was.
func TestAutomationAdapterScalesThroughTheAPIsPath(t *testing.T) {
	adapter, st, dep := automationFixture(t)
	adapter.scale = planning.Scaling{Store: st, Resolve: network.New(st, network.StoreCatalog{Store: st}).Resolve}.ScaleScheduled
	if err := adapter.ScaleReplicas(dep.ID, "", 3); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(dep.ID); got.Replicas != 3 {
		t.Fatalf("replicas = %d, want 3", got.Replicas)
	}

	multi, err := st.CreateDeployment(&store.Deployment{Name: "multi", ServedName: "multi", Engine: "vllm", State: store.DeployActive, NNodes: 2, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.ScaleReplicas(multi.ID, "", 2)
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != messages.ErrorScheduleReplicasManualPlacement {
		t.Fatalf("manual placement: err = %v, want a product error %s", err, messages.ErrorScheduleReplicasManualPlacement)
	}
	if got, _ := st.GetDeployment(multi.ID); got.Replicas != 1 {
		t.Fatalf("replicas = %d after a refused order, want 1", got.Replicas)
	}
}

// pdAutomationFixture is a PD deployment the platform places (1 prefill and
// 1 decode, each on its own one-card node) on a store with two spare nodes,
// behind the real adapter scaling through the API's path.
func pdAutomationFixture(t *testing.T) (*automationAdapter, *store.Store, *store.Deployment) {
	t.Helper()
	adapter, st, _ := automationFixture(t)
	for i, name := range []string{"pd-a", "pd-b", "pd-c"} {
		ip := "10.0.1." + string(rune('1'+i))
		node, err := st.RegisterNode(name, store.HashToken("token"), name)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(name, proto.NodeStatus{
			Sys: proto.SysInfo{Hostname: name, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"},
			Network: &proto.NetInventory{
				Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{ip + "/24"}}},
				Management: &proto.ManagementAddress{Interface: "eth0", IP: ip},
			},
			GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
			t.Fatal(err)
		}
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/pd", Name: "pd", WeightBytes: 2 << 30, Dtype: "bfloat16"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{
		Name: "pd", ModelID: model.ID, ServedName: "pd", Engine: "vllm", State: store.DeployActive, Replicas: 1, UnitsAuto: true,
		Placement: `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"pd-a"}]},"decode":{"tp":1,"units":[{"node_uuid":"pd-b"}]}}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter.scale = planning.Scaling{Store: st, Resolve: network.New(st, network.StoreCatalog{Store: st}).Resolve}.ScaleScheduled
	return adapter, st, dep
}

func pdUnitCounts(t *testing.T, st *store.Store, id uint) (prefill, decode int) {
	t.Helper()
	dep, err := st.GetDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	pd, err := planning.PDOf(dep.Placement)
	if err != nil || pd == nil {
		t.Fatalf("placement %s: %v", dep.Placement, err)
	}
	return len(pd.Units("prefill")), len(pd.Units("decode"))
}

// A PD role's schedule scales that role through the PD update, from the
// stored schedule to the stored placement, and the timeline says which role.
func TestAutomationRunsAPDRoleSchedule(t *testing.T) {
	adapter, st, dep := pdAutomationFixture(t)
	if err := adapter.ScaleReplicas(dep.ID, "prefill", 2); err != nil {
		t.Fatal(err)
	}
	if prefill, decode := pdUnitCounts(t, st, dep.ID); prefill != 2 || decode != 1 {
		t.Fatalf("units = %d prefill %d decode, want 2 and 1 (the other role is left alone)", prefill, decode)
	}
	if err := adapter.ScaleReplicas(dep.ID, "prefill", 0); err != nil {
		t.Fatalf("prefill may be empty: %v", err)
	}
	var coded interface{ Code() string }
	err := adapter.ScaleReplicas(dep.ID, "decode", 0)
	if !errors.As(err, &coded) || coded.Code() != messages.ErrorPDRoleUnitsEmpty {
		t.Fatalf("decode to 0: err = %v, want a product error %s", err, messages.ErrorPDRoleUnitsEmpty)
	}
	if prefill, decode := pdUnitCounts(t, st, dep.ID); prefill != 0 || decode != 1 {
		t.Fatalf("units = %d prefill %d decode after a refused order", prefill, decode)
	}

	// Through the evaluator, from the stored schedule: a due order scales the
	// role and records AUTO_SCALE with the role; a refused one records
	// SCHEDULE_ACTION_FAILED with the role and the reason's code.
	past := time.Now().Add(-time.Minute).In(time.Local)
	minute := past.Hour()*60 + past.Minute()
	for _, order := range []store.LifecycleSchedule{
		{DeploymentID: dep.ID, Action: store.ScheduleActionReplicas, Weekdays: automation.AllWeekdays, Minute: minute, Replicas: 1, Role: "prefill", Enabled: true},
		{DeploymentID: dep.ID, Action: store.ScheduleActionReplicas, Weekdays: automation.AllWeekdays, Minute: minute, Replicas: 0, Role: "decode", Enabled: true},
	} {
		order := order
		if err := st.CreateLifecycleSchedule(&order); err != nil {
			t.Fatal(err)
		}
		// A new order fires from its next occurrence: let the one a minute
		// ago count as not yet handled.
		if err := st.DB().Model(&store.LifecycleSchedule{}).Where("id = ?", order.ID).Update("last_fired_at", time.Now().Add(-2*time.Hour).UTC()).Error; err != nil {
			t.Fatal(err)
		}
	}
	automation.New(adapter, adapter, adapter.log).Tick()
	if prefill, decode := pdUnitCounts(t, st, dep.ID); prefill != 1 || decode != 1 {
		t.Fatalf("units = %d prefill %d decode after the schedule fired", prefill, decode)
	}
	events, err := st.DeploymentEvents(dep.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var scaled, failed map[string]any
	for _, ev := range events {
		var data map[string]any
		_ = json.Unmarshal([]byte(ev.Data), &data)
		switch ev.Code {
		case automation.EventAutoScale:
			scaled = data
		case automation.EventScheduleActionFailed:
			failed = data
		}
	}
	if scaled["role"] != "prefill" || scaled["replicas"] != float64(1) || scaled["trigger"] != "schedule" {
		t.Fatalf("AUTO_SCALE = %v", scaled)
	}
	if failed["role"] != "decode" || failed["reason_code"] != messages.ErrorPDRoleUnitsEmpty || failed["action"] != "replicas" {
		t.Fatalf("SCHEDULE_ACTION_FAILED = %v", failed)
	}
}

// A sleeper does not wake onto a card a replica waits to be placed on when
// that replica's deployment ranks at least as high: the card was given up
// for it. One ranked lower does not hold the card.
func TestAWakeLeavesTheCardsAPlacementWaitsFor(t *testing.T) {
	adapter, st, dep := automationFixture(t)
	anytime, _ := yieldpolicy.Parse(yieldpolicy.Anytime, yieldpolicy.Overrides{})
	if err := st.SetDeploymentYieldPolicy(dep.ID, anytime); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentSleep(dep.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "auto-1", State: proto.InstRunning, SleepLevel: 1}); err != nil {
		t.Fatal(err)
	}
	newcomer, err := st.CreateDeployment(&store.Deployment{Name: "chat", ModelID: dep.ModelID, NodeID: dep.NodeID, ServedName: "chat", Engine: "vllm", Sleepable: true, State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	idle, _ := yieldpolicy.Parse(yieldpolicy.Idle, yieldpolicy.Overrides{})
	if err := st.SetDeploymentYieldPolicy(newcomer.ID, idle); err != nil {
		t.Fatal(err)
	}
	waits := placewait.New()
	adapter.waits = waits
	card := placewait.Cards{NodeID: dep.NodeID, GPUIndexes: []int{0}}
	// auto-1's sleep is ordered already: the controller's next plan of the
	// newcomer waits for no holder, only for that sleep to land. The card is
	// still the newcomer's.
	waits.Wait(newcomer.ID, card, nil, time.Now())
	if woken, err := adapter.WakeWaiting(dep.ID, []string{"auto-1"}); err != nil || len(woken) != 0 {
		t.Fatalf("waiting wake onto a card whose holder's sleep was ordered for a placement = %v err=%v", woken, err)
	}
	// A wait for other cards leaves the sleeper be.
	waits.Wait(newcomer.ID, placewait.Cards{NodeID: dep.NodeID, GPUIndexes: []int{1}}, nil, time.Now())
	if free, held := adapter.notForPlacement(dep, []store.Instance{*mustInstance(t, st, "auto-1")}); len(free) != 1 || len(held) != 0 {
		t.Fatalf("a wait for card 1 kept card 0: free %d held %d", len(free), len(held))
	}
	waits.Wait(newcomer.ID, card, []string{"auto-1"}, time.Now())

	// The newcomer is idle and ranks higher: the card is its.
	result, err := adapter.Wake(dep.ID, true)
	if err != nil || len(result.Woken) != 0 || len(result.Waiting) != 1 || intentOf(t, st, "auto-1") != 1 {
		t.Fatalf("wake onto a placement's card = %+v err=%v intent=%d, want it left waiting", result, err, intentOf(t, st, "auto-1"))
	}
	if woken, err := adapter.WakeWaiting(dep.ID, []string{"auto-1"}); err != nil || len(woken) != 0 {
		t.Fatalf("waiting wake onto a placement's card = %v err=%v", woken, err)
	}
	// A newcomer that ranks lower does not hold the card.
	if err := st.SetDeploymentYieldPolicy(newcomer.ID, anytime); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentYieldPolicy(dep.ID, idle); err != nil {
		t.Fatal(err)
	}
	if woken, err := adapter.WakeWaiting(dep.ID, []string{"auto-1"}); err != nil || len(woken) != 1 {
		t.Fatalf("waiting wake past a lower placement = %v err=%v", woken, err)
	}
}
