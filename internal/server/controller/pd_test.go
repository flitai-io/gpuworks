package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type pdFixture struct {
	st    *store.Store
	hub   *distributedTestHub
	ctrl  *Controller
	dep   *store.Deployment
	nodes map[string]*store.Node
}

func newPDFixture(t *testing.T, name string, placement func(n1, n2 string) string) *pdFixture {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &pdFixture{st: st, nodes: map[string]*store.Node{}}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/pd", Name: "pd", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	online := map[string]bool{}
	for i, gpus := range []int{2, 1} {
		key := fmt.Sprintf("n%d", i+1)
		node, err := st.RegisterNode(name+"-"+key, store.HashToken("token-"+key), name+"-"+key)
		if err != nil {
			t.Fatal(err)
		}
		for g := 0; g < gpus; g++ {
			if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: g, UUID: fmt.Sprintf("%s-%s-gpu%d", name, key, g), Name: "NVIDIA GeForce RTX 4090", Vendor: "nvidia", ArchID: "sm_89", ComputeCapability: "8.9", MemoryTotal: 24 << 30}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/pd"}); err != nil {
			t.Fatal(err)
		}
		inventory := fmt.Sprintf(`{"interfaces":[{"name":"eth0","addrs":["10.0.0.%d/24"]}],"rdma_ports":[],"management":{"interface":"eth0","ip":"10.0.0.%d"}}`, i+1, i+1)
		if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("network_inventory", inventory).Error; err != nil {
			t.Fatal(err)
		}
		f.nodes[key] = node
		online[node.UUID] = true
	}
	if _, err := st.UpsertImage(&store.Image{ImageRef: "vllm/vllm-openai:v0.25.1", Source: "custom", Engine: "vllm", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	f.dep, err = createTestDeployment(t, st, &store.Deployment{
		Name: "pd", ModelID: model.ID, NodeID: f.nodes["n2"].ID, NodeIDs: fmt.Sprintf("[%d,%d]", f.nodes["n2"].ID, f.nodes["n1"].ID),
		ServedName: "pd", Engine: "vllm", Image: "vllm/vllm-openai:v0.25.1", State: "active",
		QuickParams: `{"kv_offloading_size":2}`,
		Placement:   placement(f.nodes["n1"].UUID, f.nodes["n2"].UUID),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.hub = &distributedTestHub{online: online, updates: map[string][]proto.StateUpdate{}}
	f.ctrl = New(st, f.hub, nil)
	return f
}

func twoPrefillOneDecode(n1, n2 string) string {
	return fmt.Sprintf(`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":%q,"gpus":[0]},{"node_uuid":%q}]},"decode":{"tp":1,"units":[{"node_uuid":%q}]}}}}`, n1, n1, n2)
}

func (f *pdFixture) reconcile(t *testing.T) []store.Instance {
	t.Helper()
	if err := f.ctrl.reconcileDeployment(context.Background(), f.dep.ID); err != nil {
		t.Fatal(err)
	}
	insts, err := f.st.InstancesForDeployment(f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	return insts
}

func desiredOf(t *testing.T, inst store.Instance) proto.InstanceDesiredState {
	t.Helper()
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func argAfter(argv []string, flag string) string {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func TestPDReconcileCreatesEveryRoleInstance(t *testing.T) {
	f := newPDFixture(t, "pd_reconcile_create", twoPrefillOneDecode)
	insts := f.reconcile(t)
	if len(insts) != 3 {
		t.Fatalf("instances = %d, want 3", len(insts))
	}
	roles := map[string]int{}
	ports := map[string]bool{}
	for _, inst := range insts {
		roles[inst.Role]++
		spec := desiredOf(t, inst)
		held := parseInts(inst.Ports)
		if len(held) != 2 || held[0] != inst.APIPort || !spec.HostNetwork {
			t.Fatalf("%s ports = %v api=%d host=%v", inst.Role, held, inst.APIPort, spec.HostNetwork)
		}
		for _, p := range held {
			key := fmt.Sprintf("%d/%d", inst.NodeID, p)
			if ports[key] {
				t.Fatalf("port %s allocated twice", key)
			}
			ports[key] = true
		}
		config := argAfter(spec.Argv, "--kv-transfer-config")
		wantRole := map[string]string{"prefill": "kv_producer", "decode": "kv_consumer"}[inst.Role]
		if !strings.Contains(config, `"kv_role":"`+wantRole+`"`) {
			t.Fatalf("%s config = %s", inst.Role, config)
		}
		// KV offload runs on prefill only, composed into one connector.
		if inst.Role == pdmode.RolePrefill && !strings.Contains(config, "MultiConnector") {
			t.Fatalf("prefill did not compose the CPU tier: %s", config)
		}
		if inst.Role == pdmode.RoleDecode && strings.Contains(config, "Offloading") {
			t.Fatalf("decode carries a KV tier: %s", config)
		}
		if strings.Contains(strings.Join(spec.Argv, " "), "--kv-offloading-size") {
			t.Fatalf("CPU tier flag beside the connector: %v", spec.Argv)
		}
		if spec.Env["VLLM_NIXL_SIDE_CHANNEL_PORT"] != fmt.Sprint(held[1]) || spec.Env["VLLM_NIXL_SIDE_CHANNEL_HOST"] == "" {
			t.Fatalf("%s side channel env = %v", inst.Role, spec.Env)
		}
	}
	if roles["prefill"] != 2 || roles["decode"] != 1 {
		t.Fatalf("roles = %v", roles)
	}
	// The pinned prefill kept GPU 0, so the unpinned one got GPU 1.
	var gpus []string
	for _, inst := range insts {
		if inst.Role == pdmode.RolePrefill {
			gpus = append(gpus, inst.GPUIndexes)
		}
	}
	if !(strings.Contains(strings.Join(gpus, ","), "[0]") && strings.Contains(strings.Join(gpus, ","), "[1]")) {
		t.Fatalf("prefill GPUs = %v", gpus)
	}
	// A second pass with every unit filled creates nothing.
	if again := f.reconcile(t); len(again) != 3 {
		t.Fatalf("second pass instances = %d", len(again))
	}
}

func TestPDReconcileRebuildsOnlyTheFailedInstance(t *testing.T) {
	f := newPDFixture(t, "pd_reconcile_rebuild", twoPrefillOneDecode)
	f.ctrl.SetRetryPolicy(0, 3)
	insts := f.reconcile(t)
	var prefill, decode store.Instance
	for _, inst := range insts {
		if inst.Role == pdmode.RoleDecode {
			decode = inst
		} else if strings.Contains(inst.GPUIndexes, "0") {
			prefill = inst
		}
	}
	if err := f.st.SetInstanceState(decode.UUID, proto.InstRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetInstanceState(prefill.UUID, proto.InstFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	insts = f.reconcile(t)
	if len(insts) != 4 {
		t.Fatalf("instances = %d, want the failed row plus a replacement", len(insts))
	}
	for _, inst := range insts {
		if inst.UUID == decode.UUID && inst.State != proto.InstRunning {
			t.Fatalf("decode was touched: %s", inst.State)
		}
	}
	// The retry budget is per unit: exhaust it and the unit stays empty while
	// the rest keep running.
	for i := 0; i < 3; i++ {
		for _, inst := range insts {
			if inst.Role == pdmode.RolePrefill && inst.State == proto.InstPending && strings.Contains(inst.GPUIndexes, "0") {
				_ = f.st.SetInstanceState(inst.UUID, proto.InstFailed, "boom")
			}
		}
		insts = f.reconcile(t)
	}
	failed := 0
	for _, inst := range insts {
		if inst.State == proto.InstFailed {
			failed++
		}
	}
	if failed != 3 {
		t.Fatalf("failed attempts = %d, want the budget of 3", failed)
	}
}

type fakeDrainProbe map[string]int

func (f fakeDrainProbe) InFlight(uuid string) int { return f[uuid] }

func TestPDReconcileDrainsInstancesWithoutUnit(t *testing.T) {
	f := newPDFixture(t, "pd_reconcile_scale_down", twoPrefillOneDecode)
	insts := f.reconcile(t)
	if len(insts) != 3 {
		t.Fatalf("instances = %d", len(insts))
	}
	var extra string
	for _, inst := range insts {
		if inst.Role == pdmode.RolePrefill && !strings.Contains(inst.GPUIndexes, "[0]") {
			extra = inst.UUID
		}
		_ = f.st.SetInstanceState(inst.UUID, proto.InstRunning, "")
	}
	probe := fakeDrainProbe{extra: 1}
	f.ctrl.SetDrainProbe(probe)
	oneEach := fmt.Sprintf(`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":%q,"gpus":[0]}]},"decode":{"units":[{"node_uuid":%q}]}}}}`, f.nodes["n1"].UUID, f.nodes["n2"].UUID)
	if err := f.st.DB().Model(&store.Deployment{}).Where("id = ?", f.dep.ID).Update("placement", oneEach).Error; err != nil {
		t.Fatal(err)
	}
	removing := func() []string {
		var out []string
		for _, inst := range f.reconcile(t) {
			if inst.State == proto.InstRemoving {
				out = append(out, inst.UUID)
			}
		}
		return out
	}
	// First sight marks it draining, which takes it out of the gateway, and
	// starts the drain clock; nothing is removed yet.
	if got := removing(); len(got) != 0 {
		t.Fatalf("removed before draining: %v", got)
	}
	if row, _ := f.st.InstanceByUUID(extra); row.State != proto.InstDraining {
		t.Fatalf("the unclaimed instance is %s, want draining", row.State)
	}
	// Past the minimum but still holding work: keep waiting.
	f.ctrl.drainClocks.startedAt(f.dep.ID, extra, time.Now().Add(-drainMin-time.Second))
	if got := removing(); len(got) != 0 {
		t.Fatalf("removed while busy: %v", got)
	}
	// Idle: removed, and only the instance no unit claims.
	probe[extra] = 0
	if got := removing(); len(got) != 1 || got[0] != extra {
		t.Fatalf("removing = %v, want %s", got, extra)
	}
}

func TestPDDrainGivesUpAfterTheBound(t *testing.T) {
	c := New(nil, nil, nil)
	c.SetDrainProbe(fakeDrainProbe{"busy": 5})
	excess := []store.Instance{{UUID: "busy"}}
	if got := c.drainedUnits(1, excess); len(got) != 0 {
		t.Fatal("removed on first sight")
	}
	c.drainClocks.startedAt(1, "busy", time.Now().Add(-drainMax))
	if got := c.drainedUnits(1, excess); len(got) != 1 {
		t.Fatal("a stuck request held the drain past its bound")
	}
	if clocks := c.drainClocks.of(1); clocks != nil {
		t.Fatalf("a finished drain kept its clock: %v", clocks)
	}
}

// The drain clocks of a deployment keep only the instances still draining,
// and go with the deployment.
func TestDrainClocksFollowTheDrainingInstances(t *testing.T) {
	c := New(nil, nil, nil)
	c.SetDrainProbe(fakeDrainProbe{"a": 1, "b": 1})
	c.drainedUnits(1, []store.Instance{{UUID: "a"}, {UUID: "b"}})
	c.drainedUnits(2, []store.Instance{{UUID: "c"}})
	// b left draining some other way (the deployment restarted it at once).
	c.drainedUnits(1, []store.Instance{{UUID: "a"}})
	if clocks := c.drainClocks.of(1); len(clocks) != 1 || clocks["a"].IsZero() {
		t.Fatalf("deployment 1 clocks = %v, want only a", clocks)
	}
	c.forgetDeployment(2, false)
	if clocks := c.drainClocks.of(2); clocks != nil {
		t.Fatalf("a forgotten deployment kept its clocks: %v", clocks)
	}
}

// A unit is rebuilt on the network the group resolves to now, even when a
// running member started on another one (the operator may be moving the
// deployment between networks); whether the pair still hands off is the
// handoff check's to say.
func TestPDRebuildFollowsTheCurrentNetwork(t *testing.T) {
	f := newPDFixture(t, "pd_reconcile_drift", twoPrefillOneDecode)
	f.ctrl.SetRetryPolicy(0, 3)
	insts := f.reconcile(t)
	var failed string
	for _, inst := range insts {
		if inst.Role == pdmode.RoleDecode {
			if err := f.st.DB().Model(&store.Instance{}).Where("uuid = ?", inst.UUID).Update("schedule_expl", `{"network":{"compute":{"name":"retired-net"}}}`).Error; err != nil {
				t.Fatal(err)
			}
			continue
		}
		if strings.Contains(inst.GPUIndexes, "0") {
			failed = inst.UUID
			_ = f.st.SetInstanceState(inst.UUID, proto.InstFailed, "boom")
		}
	}
	insts = f.reconcile(t)
	if len(insts) != 4 {
		t.Fatalf("instances after the rebuild = %d, want the failed row plus a replacement", len(insts))
	}
	for _, inst := range insts {
		if inst.Role == pdmode.RolePrefill && inst.UUID != failed && inst.State != proto.InstFailed {
			if network, ok := planning.ReadScheduleRecord(inst.ScheduleExpl).ComputeNetwork(); !ok || network.Name == "retired-net" {
				t.Fatalf("replacement network = %+v %v", network, ok)
			}
		}
	}
}

func TestMatchUnitsForPDRoles(t *testing.T) {
	pd := &planning.PDPlacement{Mode: "vllm-nixl", Roles: map[string]planning.PDRole{
		"prefill": {Units: []planning.PDUnit{{NodeUUID: "a", GPUIndexes: []int{1}}, {NodeUUID: "a"}}},
		"decode":  {Units: []planning.PDUnit{{NodeUUID: "b"}}},
	}}
	mode, _ := pdmode.Lookup("vllm-nixl")
	nodes := map[uint]string{1: "a", 2: "b"}
	insts := []store.Instance{
		{UUID: "p0", Role: "prefill", NodeID: 1, GPUIndexes: "[0]", State: proto.InstRunning},
		{UUID: "p1", Role: "prefill", NodeID: 1, GPUIndexes: "[1]", State: proto.InstRunning},
		{UUID: "p2", Role: "prefill", NodeID: 1, GPUIndexes: "[2]", State: proto.InstRunning},
		{UUID: "d-failed", Role: "decode", NodeID: 2, GPUIndexes: "[0]", State: proto.InstFailed},
		{UUID: "stray", Role: "", NodeID: 2, GPUIndexes: "[0]", State: proto.InstRunning},
	}
	match := planning.MatchUnits(pd.UnitsOf(mode), insts, nodes, "")
	slots, excess, failed := match.Slots, match.Excess, match.Failed
	if len(slots) != 3 || slots[0].Unit.Role != "decode" || slots[0].Inst != nil {
		t.Fatalf("decode slot = %+v", slots[0])
	}
	if slots[1].Inst == nil || slots[1].Inst.UUID != "p1" || slots[2].Inst == nil || slots[2].Inst.UUID != "p0" {
		t.Fatalf("prefill slots = %+v %+v", slots[1].Inst, slots[2].Inst)
	}
	if len(excess) != 2 || excess[0].UUID != "p2" || excess[1].UUID != "stray" {
		t.Fatalf("excess = %+v", excess)
	}
	if len(failed[slots[0].Key]) != 1 {
		t.Fatalf("failed = %+v", failed)
	}
}

func TestPickPortsAllocatesDisjointBlocks(t *testing.T) {
	st, err := store.Open("file:pd_pick_ports?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("ports", store.HashToken("t"), "ports")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "held", NodeID: node.ID, State: proto.InstRunning, Ports: "[40001,40003]"}); err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil)
	c.SetEnginePorts(40000, 40010)
	got, rejected := c.pickPorts(node, 1, 2, 3)
	// 40000 is free; the 2-block cannot use 40001 or 40003, so it lands on
	// 40004-40005; the 3-block follows at 40006.
	if rejected != nil || fmt.Sprint(got) != "[40000 40004 40006]" {
		t.Fatalf("blocks = %v %v", got, rejected)
	}
	if port, rejected := c.pickPort(node); rejected != nil || port != 40000 {
		t.Fatalf("single port = %d %v", port, rejected)
	}
	// A block that no longer fits is a rejection, not a clashing port.
	if got, rejected := c.pickPorts(node, 1, 20); rejected == nil || got != nil {
		t.Fatalf("oversized block = %v %v", got, rejected)
	}
}

// A PD placement the controller cannot read holds the deployment instead of
// launching its nodes as an ordinary replica, which would run without a KV
// connector.
func TestPDUnreadablePlacementHoldsTheDeployment(t *testing.T) {
	f := newPDFixture(t, "pd_unreadable", func(n1, n2 string) string {
		return `{"pd":{"mode":"vllm-nixl","roles":{"decode":{"units":[{"node_uuid":"` + n2 + `"}]}},"bogus":true}}`
	})
	if insts := f.reconcile(t); len(insts) != 0 {
		t.Fatalf("unreadable PD placement launched %d instances", len(insts))
	}
	latest, found, err := f.st.LatestScheduleEvent(f.dep.ID)
	if err != nil || !found || latest.Code != "DEPLOYMENT_UNSCHEDULABLE" || !strings.Contains(latest.Data, `"code":"`+scheduler.ReasonPlacementRejected+`"`) || !strings.Contains(latest.Data, "bogus") {
		t.Fatalf("latest schedule event = %+v %v %v, want the placement rejected naming the bad field", latest, found, err)
	}
}

// The retry backoff is measured on the controller's clock, so a rebuild waits
// out the backoff and starts once the clock has passed it.
func TestPDRetryBackoffFollowsTheControllerClock(t *testing.T) {
	f := newPDFixture(t, "pd_retry_clock", twoPrefillOneDecode)
	f.ctrl.SetRetryPolicy(time.Hour, 3)
	insts := f.reconcile(t)
	var decode store.Instance
	for _, inst := range insts {
		if inst.Role == pdmode.RoleDecode {
			decode = inst
		}
	}
	if err := f.st.SetInstanceState(decode.UUID, proto.InstFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	if insts = f.reconcile(t); len(insts) != 3 {
		t.Fatalf("instances within the backoff = %d, want no rebuild yet", len(insts))
	}
	f.ctrl.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	insts = f.reconcile(t)
	rebuilt := 0
	for _, inst := range insts {
		if inst.Role == pdmode.RoleDecode && inst.UUID != decode.UUID {
			rebuilt++
		}
	}
	if rebuilt != 1 {
		t.Fatalf("decode rebuilds after the backoff = %d, want 1", rebuilt)
	}
	events, err := f.st.DeploymentEvents(f.dep.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range events {
		if ev.Code == store.EventReplicaRebuilt {
			count++
			if !strings.Contains(ev.Data, `"role":"decode"`) || !strings.Contains(ev.Data, `"attempt":2`) || !strings.Contains(ev.Data, `"reason":"boom"`) {
				t.Fatalf("rebuild event = %s, want the decode unit's second attempt after boom", ev.Data)
			}
		}
	}
	if count != 1 {
		t.Fatalf("REPLICA_REBUILT events = %d, want 1", count)
	}
}

// A unit that spent its budget says so once on its newest attempt, however
// many passes find it exhausted.
func TestUnitRetryExhaustionIsRecordedOnce(t *testing.T) {
	f := newPDFixture(t, "unit_exhausted", twoPrefillOneDecode)
	f.ctrl.SetRetryPolicy(0, 1)
	var decode store.Instance
	for _, inst := range f.reconcile(t) {
		if inst.Role == pdmode.RoleDecode {
			decode = inst
		}
	}
	if err := f.st.SetInstanceState(decode.UUID, proto.InstFailed, "image pull timed out"); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 3; pass++ {
		f.reconcile(t)
	}
	events, err := f.st.InstanceEventsByUUID(decode.UUID, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range events {
		if ev.Code == store.EventUnitRetryExhausted {
			count++
			if !strings.Contains(ev.Data, "image pull timed out") || !strings.Contains(ev.Data, `"attempts":1`) {
				t.Fatalf("exhaustion event = %s", ev.Data)
			}
		}
	}
	if count != 1 {
		t.Fatalf("exhaustion events = %d, want 1", count)
	}
}

// A failed attempt of a unit that left the placement is retired, so the unit
// starts with its whole budget when it is added again.
func TestAttemptsOfARemovedUnitAreRetired(t *testing.T) {
	f := newPDFixture(t, "unit_removed", twoPrefillOneDecode)
	f.ctrl.SetRetryPolicy(0, 1)
	var pinned store.Instance
	for _, inst := range f.reconcile(t) {
		if inst.Role == pdmode.RolePrefill && inst.GPUIndexes == "[0]" {
			pinned = inst
		}
	}
	if pinned.UUID == "" {
		t.Fatal("no prefill instance on the pinned unit")
	}
	if err := f.st.SetInstanceState(pinned.UUID, proto.InstFailed, "image pull timed out"); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	placement := fmt.Sprintf(`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":%q}]},"decode":{"tp":1,"units":[{"node_uuid":%q}]}}}}`, f.nodes["n1"].UUID, f.nodes["n2"].UUID)
	if err := f.st.DB().Model(&store.Deployment{}).Where("id = ?", f.dep.ID).Update("placement", placement).Error; err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if row, _ := f.st.InstanceByUUID(pinned.UUID); row.State != proto.InstRemoving || !strings.Contains(row.Message, "left the placement") {
		t.Fatalf("attempt of the removed unit = %s %q", row.State, row.Message)
	}
}

// Instances of one deployment may run different images (a rolling upgrade,
// or one image identified differently by two nodes' stores): a rebuild is
// not held back for it; whether the pair still hands off is the handoff
// check's to say.
func TestPDRebuildIsNotHeldByADifferentImage(t *testing.T) {
	f := newPDFixture(t, "pd_image_differs", twoPrefillOneDecode)
	f.ctrl.SetRetryPolicy(0, 3)
	insts := f.reconcile(t)
	var decode store.Instance
	for _, inst := range insts {
		if inst.Role == pdmode.RoleDecode {
			decode = inst
		}
		if err := f.st.DB().Model(&store.Instance{}).Where("uuid = ?", inst.UUID).Updates(map[string]any{"state": proto.InstRunning, "image_digest_id": "sha256:gpu3-local-id"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	image, err := f.st.ResolveImageRef("vllm/vllm-openai:v0.25.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, ImageID: image.ID, NodeID: f.nodes["n2"].ID, State: store.ImageStatePresent, ImageDigestID: "sha256:loaded-elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetInstanceState(decode.UUID, proto.InstFailed, "crashed"); err != nil {
		t.Fatal(err)
	}
	rebuilt := false
	for _, inst := range f.reconcile(t) {
		if inst.Role == pdmode.RoleDecode && inst.UUID != decode.UUID {
			rebuilt = true
		}
	}
	if !rebuilt {
		t.Fatal("the decode unit was not rebuilt on a node whose image id differs")
	}
}

// A PD mode whose KV rides UCX cannot start on an accelerator RDMA network:
// the deployment is held with the reason instead of launching instances that
// would hand a card's name to UCX.
func TestPDIsHeldOnANetworkItsTransportCannotUse(t *testing.T) {
	f := newPDFixture(t, "pd_network_transport", twoPrefillOneDecode)
	network, err := f.st.CreateNetwork(&store.Network{Name: "npu", Type: netplan.TypeAcceleratorRDMA, Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range f.nodes {
		if _, err := f.st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: network.ID, NodeID: node.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if insts := f.reconcile(t); len(insts) != 0 {
		t.Fatalf("instances = %d, want none on a network the mode cannot use", len(insts))
	}
	latest, found, err := f.st.LatestScheduleEvent(f.dep.ID)
	if err != nil || !found || latest.Code != "DEPLOYMENT_UNSCHEDULABLE" || !strings.Contains(latest.Data, `"code":"`+scheduler.ReasonPlacementRejected+`"`) ||
		!strings.Contains(latest.Data, "needs a host network") || !strings.Contains(latest.Data, "npu") {
		t.Fatalf("schedule event = %+v found=%v err=%v", latest, found, err)
	}
	// Off that network the same deployment starts.
	if err := f.st.DeleteNetwork(network.ID); err != nil {
		t.Fatal(err)
	}
	f.ctrl.now = func() time.Time { return time.Now().Add(time.Hour) }
	if insts := f.reconcile(t); len(insts) != 3 {
		t.Fatalf("instances after leaving the network = %d, want 3", len(insts))
	}
}
