package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The rows here are read the way the gateway reads a deployment, and each case
// is one way the two used to disagree: a PD role that sleeps on its own, an
// ordinary scale-down that leaves an awake replica the placement no longer
// claims, and a multi-node generation whose leader sleeps while a rank is down.
// A row showing "running" while the gateway answers 503 is the reported defect;
// a row showing "asleep" while the gateway serves is the same defect inverted.

// pdStateFixture is a PD deployment of the fixture's two prefill units and one
// decode unit, all running and awake, on the two PD fixture nodes.
type pdStateFixture struct {
	handlers *apiHandlers
	store    *store.Store
	id       uint
}

func newPDStateFixture(t *testing.T) pdStateFixture {
	t.Helper()
	h, st, created := createPDForOps(t)
	for _, rank := range []struct{ uuid, role, node, gpus string }{
		{"prefill-0", "prefill", "pd-a", "[0]"},
		{"prefill-1", "prefill", "pd-a", "[1]"},
		{"decode-0", "decode", "pd-b", "[0,1]"},
	} {
		node, err := st.NodeByUUID(rank.node)
		if err != nil {
			t.Fatal(err)
		}
		raw := fmt.Sprintf(`{"uuid":%q,"desired_state":"running","sleepable":true,"role":%q}`, rank.uuid, rank.role)
		if _, err := st.CreateInstance(&store.Instance{UUID: rank.uuid, DeploymentID: created.ID, NodeID: node.ID,
			State: proto.InstRunning, Role: rank.role, GPUIndexes: rank.gpus, APIPort: 8000, DesiredStateJSON: raw}); err != nil {
			t.Fatal(err)
		}
	}
	return pdStateFixture{handlers: h, store: st, id: created.ID}
}

// sleepRole records a sleep order for one role the way the handler does: the
// intent on those instances only, leaving the deployment-wide level alone.
func (f pdStateFixture) sleepRole(t *testing.T, role string, level int) {
	t.Helper()
	var uuids []string
	inst, err := f.store.InstancesForDeployment(f.id)
	if err != nil {
		t.Fatal(err)
	}
	for i := range inst {
		if inst[i].Role == role {
			uuids = append(uuids, inst[i].UUID)
		}
	}
	if len(uuids) == 0 {
		t.Fatalf("no instance of role %q", role)
	}
	if err := f.store.OrderInstancesSleep(f.id, uuids, level, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
}

// reported records the level an instance's engine says it is at.
func (f pdStateFixture) reported(t *testing.T, uuid string, level int) {
	t.Helper()
	if err := f.store.UpdateInstanceStatus(proto.InstanceStatus{UUID: uuid, State: proto.InstRunning, SleepLevel: level, Phase: proto.PhaseSleep}); err != nil {
		t.Fatal(err)
	}
}

// dto reads the deployment as GET /deployments/{id} shows it.
func (f pdStateFixture) dto(t *testing.T) dtoDeploymentView {
	t.Helper()
	got := f.handlers.deploymentDTO(mustDeployment(t, f.store, f.id), true)
	view := dtoDeploymentView{State: got.State, RuntimeState: got.RuntimeState, AsleepReplicas: got.AsleepReplicas, SleepLevel: got.SleepLevel}
	if got.PD != nil {
		view.PDState = got.PD.State
		for _, role := range got.PD.Roles {
			view.Roles = append(view.Roles, fmt.Sprintf("%s:%d/%d/%d", role.Role, role.Ready, role.Asleep, role.Desired))
		}
	}
	return view
}

// dtoDeploymentView is the sleep and readiness fields of one deployment read.
type dtoDeploymentView struct {
	State, RuntimeState string
	AsleepReplicas      int
	SleepLevel          int
	PDState             string
	Roles               []string
}

func (v dtoDeploymentView) String() string {
	return fmt.Sprintf("state=%s runtime=%s asleep_replicas=%d sleep_level=%d pd_state=%s roles=%v",
		v.State, v.RuntimeState, v.AsleepReplicas, v.SleepLevel, v.PDState, v.Roles)
}

// A PD deployment whose prefill role sleeps on its own still serves: the
// gateway falls back to its decoders (wantsPrefill is false when the mode may
// run without prefill), so the row must keep its running state and say the one
// prefill unit async.
func TestDeploymentPDAsleepPrefillStillServes(t *testing.T) {
	f := newPDStateFixture(t)
	if got := f.dto(t); got.State != "running" || got.PDState != "running" || got.AsleepReplicas != 0 {
		t.Fatalf("awake = %s, want every role ready", got)
	}
	f.sleepRole(t, "prefill", 1)
	// Ordered but not carried out yet: the gateway already refuses the prefill
	// instance, and its decoders answer alone.
	if got := f.dto(t); got.State != "running" || got.AsleepReplicas != 2 || got.SleepLevel != 0 {
		t.Fatalf("draining prefill = %s, want running with both prefill replicas asleep", got)
	}
	f.reported(t, "prefill-0", 1)
	got := f.dto(t)
	if got.State != "running" || got.RuntimeState != "running" {
		t.Fatalf("asleep prefill = %s, want the deployment still running", got)
	}
	if got.AsleepReplicas != 2 || got.SleepLevel != 1 {
		t.Fatalf("asleep prefill = %s, want asleep_replicas 2 and sleep_level 1", got)
	}
	// The role's own counts say which role sleeps, so the panel explains why
	// the deployment is short of its desired instances without reading broken.
	if fmt.Sprint(got.Roles) != fmt.Sprint([]string{"decode:1/0/1", "prefill:0/2/2"}) {
		t.Fatalf("roles = %v, want decode 1 ready and both prefill units asleep", got.Roles)
	}
	if got.PDState != "running" {
		t.Fatalf("pd state = %q, want running: the mode serves without prefill", got.PDState)
	}
	// Waking it: with the order withdrawn only the engine's own level keeps
	// the replica asleep, which is what the gateway reads until it wakes.
	f.sleepRole(t, "prefill", 0)
	waking := f.dto(t)
	if waking.State != "running" || waking.AsleepReplicas != 1 || waking.SleepLevel != 1 {
		t.Fatalf("waking = %s, want one replica still asleep at its reported level", waking)
	}
	f.reported(t, "prefill-0", 0)
	woken := f.dto(t)
	if woken.State != "running" || woken.AsleepReplicas != 0 || woken.SleepLevel != 0 {
		t.Fatalf("woken = %s, want no asleep replica", woken)
	}
	if fmt.Sprint(woken.Roles) != fmt.Sprint([]string{"decode:1/0/1", "prefill:2/0/2"}) {
		t.Fatalf("roles after wake = %v, want every prefill unit ready", woken.Roles)
	}
}

// A PD deployment whose decode role sleeps cannot serve at all: the gateway
// refuses every request, so the row reads asleep (or sleeping) and the roles
// still say which one is asleep. Prefill sleeping too is what makes the whole
// deployment asleep, not the prefill alone.
func TestDeploymentPDAsleepDecodeRefuses(t *testing.T) {
	f := newPDStateFixture(t)
	f.sleepRole(t, "decode", 1)
	// Only the decoders sleep and the mode serves without prefill: nothing can
	// answer, so the deployment is asleep.
	if got := f.dto(t); got.State != "sleeping" || got.AsleepReplicas != 1 {
		t.Fatalf("draining decode = %s, want sleeping with one asleep replica", got)
	}
	f.reported(t, "decode-0", 1)
	got := f.dto(t)
	if got.State != "asleep" || got.AsleepReplicas != 1 || got.SleepLevel != 1 {
		t.Fatalf("asleep decode = %s, want asleep", got)
	}
	if got.PDState != "partial" {
		t.Fatalf("pd state = %q, want partial: only decode is awake, and it is not", got.PDState)
	}
	// Decode is awake again but prefill still sleeps: the decoders alone answer.
	f.sleepRole(t, "decode", 0)
	f.reported(t, "decode-0", 0)
	f.sleepRole(t, "prefill", 1)
	f.reported(t, "prefill-0", 1)
	if got := f.dto(t); got.State != "running" || got.AsleepReplicas != 2 {
		t.Fatalf("woken decode, asleep prefill = %s, want running with both prefill replicas asleep", got)
	}
	// Both roles asleep, and every engine reports its level: nothing answers.
	f.sleepRole(t, "decode", 1)
	f.reported(t, "decode-0", 1)
	f.reported(t, "prefill-1", 1)
	if got := f.dto(t); got.State != "asleep" || got.AsleepReplicas != 3 {
		t.Fatalf("both asleep = %s, want asleep with three asleep replicas", got)
	}
}

// A prefill role the mode cannot run without is not optional: when it sleeps
// the gateway has no prefill instance and refuses, so the row must not read
// running on the strength of its decoders. sglang-nixl never falls back and
// cannot be scaled to zero.
func TestDeploymentPDRequiredPrefillAsleepRefuses(t *testing.T) {
	h, st, created := createPDForOps(t)
	// The fixture's placement is vllm-nixl; write the strict mode's placement
	// straight in, then deploy the instances it names.
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", created.ID).
		Update("placement", `{"pd":{"mode":"sglang-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"pd-a","gpus":[0]}]},"decode":{"tp":2,"units":[{"node_uuid":"pd-b"}]}}}}`).Error; err != nil {
		t.Fatal(err)
	}
	for _, rank := range []struct{ uuid, role, node string }{{"prefill-0", "prefill", "pd-a"}, {"decode-0", "decode", "pd-b"}} {
		node, err := st.NodeByUUID(rank.node)
		if err != nil {
			t.Fatal(err)
		}
		raw := fmt.Sprintf(`{"uuid":%q,"desired_state":"running","sleepable":true,"role":%q}`, rank.uuid, rank.role)
		if _, err := st.CreateInstance(&store.Instance{UUID: rank.uuid, DeploymentID: created.ID, NodeID: node.ID,
			State: proto.InstRunning, Role: rank.role, GPUIndexes: "[0]", APIPort: 8000, DesiredStateJSON: raw}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.OrderInstancesSleep(created.ID, []string{"prefill-0"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "prefill-0", State: proto.InstRunning, SleepLevel: 1, Phase: proto.PhaseSleep}); err != nil {
		t.Fatal(err)
	}
	got := h.deploymentDTO(mustDeployment(t, st, created.ID), true)
	if got.State != "asleep" || got.AsleepReplicas != 1 {
		t.Fatalf("state = %q asleep_replicas = %d, want asleep: the mode cannot run without prefill", got.State, got.AsleepReplicas)
	}
	if got.PD == nil || got.PD.State != "partial" {
		t.Fatalf("pd = %+v, want partial", got.PD)
	}
}

// A scale-down leaves the newer replica awake and the placement no longer
// claims it: the gateway still routes to it (the ordinary path does not filter
// the excess replica, which the controller drains on a later pass), so the row
// must count it ready rather than read asleep on the older replica's order.
func TestDeploymentScaleDownKeepsAwakeExcessReplicaReady(t *testing.T) {
	f := newSleepStateFixture(t)
	deps, err := f.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	var dep *store.Deployment
	for i := range deps {
		if deps[i].Name == "partial" {
			dep = &deps[i]
		}
	}
	if dep == nil {
		t.Fatal("no partial deployment")
	}
	// Scale the two-replica deployment to one; partial-0 (older) is asleep and
	// partial-1 is awake, so the awake one becomes the excess replica.
	if err := f.st.DB().Model(&store.Deployment{}).Where("id = ?", dep.ID).Update("replicas", 1).Error; err != nil {
		t.Fatal(err)
	}
	row := f.row(t, "partial")
	if row.State == deployment.StateAsleep || row.State == deployment.StateSleeping {
		t.Fatalf("state = %q, want the running state: the awake excess replica still serves", row.State)
	}
	if row.ReadyReplicas == nil || *row.ReadyReplicas != 1 {
		t.Fatalf("ready_replicas = %v, want 1: the awake replica still serves", row.ReadyReplicas)
	}
	if row.AsleepReplicas != 1 {
		t.Fatalf("asleep_replicas = %d, want 1", row.AsleepReplicas)
	}
	// With the awake replica gone too, nothing serves and the remaining asleep
	// replica is what the gateway refuses: the row reads asleep.
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "partial-1", State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	row = f.row(t, "partial")
	if row.State != deployment.StateAsleep {
		t.Fatalf("state = %q, want asleep", row.State)
	}
	if row.ReadyReplicas == nil || *row.ReadyReplicas != 0 {
		t.Fatalf("ready_replicas = %v, want 0", row.ReadyReplicas)
	}
}

// A multi-node replica whose leader sleeps while a follower is down: the
// gateway records the generation asleep before it checks whether every rank is
// up, so it answers "model is asleep; wake it first" and never reaches the
// incomplete-rank message. The row must read asleep too, or it says running
// while the gateway refuses.
func TestDeploymentMultiNodeIncompleteAsleepReadsAsleep(t *testing.T) {
	f := newListFixture(t, 0)
	nodes, _ := f.st.ListNodes()
	for _, n := range nodes {
		_ = f.st.SetNodeState(n.UUID, store.NodeStateOnline)
	}
	a, _ := f.st.NodeByUUID("list-a")
	b, _ := f.st.NodeByUUID("list-b")
	dep, err := f.st.CreateDeployment(&store.Deployment{Name: "incomplete", State: store.DeployActive, Sleepable: true,
		NodeID: a.ID, NodeIDs: fmt.Sprintf("[%d,%d]", a.ID, b.ID), NNodes: 2, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range []store.Instance{
		{UUID: "inc-leader", NodeID: a.ID, State: proto.InstRunning, NNodes: 2, DistGroup: "inc", DistAttempt: 1,
			DesiredStateJSON: `{"uuid":"inc-leader","desired_state":"running","sleepable":true}`},
		{UUID: "inc-follower", NodeID: b.ID, State: proto.InstRunning, Headless: true, NNodes: 2, NodeRank: 1, DistGroup: "inc", DistAttempt: 1,
			DesiredStateJSON: `{"uuid":"inc-follower","desired_state":"running","sleepable":true,"headless":true}`},
	} {
		inst.DeploymentID = dep.ID
		if _, err := f.st.CreateInstance(&inst); err != nil {
			t.Fatal(err)
		}
	}
	check := func(state string, ready, asleep int) {
		t.Helper()
		got := f.handler.deploymentDTO(mustDeployment(t, f.st, dep.ID), true)
		if got.State != state || got.ReadyReplicas == nil || *got.ReadyReplicas != ready || got.AsleepReplicas != asleep {
			t.Fatalf("got state=%q ready=%v asleep=%d, want %q/%d/%d", got.State, got.ReadyReplicas, got.AsleepReplicas, state, ready, asleep)
		}
	}
	check("running", 1, 0)
	// The follower stops: the generation cannot serve, and it is not asleep.
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "inc-follower", State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	check("running", 0, 0)
	// The leader is ordered to sleep and reports it: the gateway now refuses
	// with the asleep message before it ever notices the missing rank.
	if err := f.st.OrderDeploymentSleep(dep.ID, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	reportAsleep(t, f.st, "inc-leader", 1)
	check("asleep", 0, 1)
}

// A sleep order whose engine failed is never carried out, and the agent
// reports no target with its failure, so the intent stays: the replica is
// still refused by the gateway, and the row must say so rather than read
// running because no engine reports a level.
func TestDeploymentUncertainSleepStillReadsAsleep(t *testing.T) {
	f := newSleepStateFixture(t)
	deps, err := f.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	var dep *store.Deployment
	for i := range deps {
		if deps[i].Name == "awake" {
			dep = &deps[i]
		}
	}
	if dep == nil {
		t.Fatal("no awake deployment")
	}
	if err := f.st.OrderDeploymentSleep(dep.ID, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	// The agent's uncertain-sleep report: failed, no level, no target.
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "awake-0", State: proto.InstFailed, SleepLevel: 0,
		Phase: proto.PhaseSleep, Message: "sleep level uncertain"}); err != nil {
		t.Fatal(err)
	}
	row := f.row(t, "awake")
	if inst, err := f.st.InstanceByUUID("awake-0"); err != nil || inst.SleepIntent() != 1 {
		t.Fatalf("intent = %+v %v, want the failed order's intent kept", inst, err)
	}
	// Nothing can serve: the one instance failed. The row must not present the
	// deployment as asleep either, because nothing is left to wake; both the
	// gateway and the row say the deployment is down.
	if row.State == deployment.StateAsleep || row.State == deployment.StateSleeping {
		t.Fatalf("state = %q with a failed instance, want the failure's own state", row.State)
	}
	if row.AsleepReplicas != 0 {
		t.Fatalf("asleep_replicas = %d with a failed instance", row.AsleepReplicas)
	}
}

// A PD deployment's detail response must describe its replicas the way its
// list row does. The stored Replicas field only configures an ordinary
// deployment (PD creation forces it to 1), so reporting it on the detail made
// the drawer pair a unit-counted numerator with a denominator of 1: the
// fixture below put asleep_replicas at 2 over replicas at 1, and the drawer
// printed "Partly asleep · 2/1 replicas". One number has to mean one thing on
// both surfaces, and the denominator must never be smaller than its numerator.
func TestDeploymentPDDetailReplicasMatchTheListRow(t *testing.T) {
	f := newPDStateFixture(t)
	f.sleepRole(t, "prefill", 1)
	f.reported(t, "prefill-0", 1)
	f.reported(t, "prefill-1", 1)

	detail := f.handlers.deploymentDTO(mustDeployment(t, f.store, f.id), true)
	if detail.AsleepReplicas != 2 {
		t.Fatalf("asleep_replicas = %d, want both prefill units", detail.AsleepReplicas)
	}
	// The detail's own replica counts add up to the units the mode has, and
	// ready_replicas is filled rather than omitted: the drawer prints the pair.
	if detail.ReadyReplicas == nil {
		t.Fatal("ready_replicas absent on a PD detail, want the count the drawer pairs with asleep_replicas")
	}
	units := 0
	if detail.PD != nil {
		for _, role := range detail.PD.Roles {
			units += role.Desired
		}
	}
	if units == 0 {
		t.Fatal("no PD roles to count")
	}
	if *detail.ReadyReplicas+detail.AsleepReplicas != units {
		t.Fatalf("ready+asleep = %d, want %d units", *detail.ReadyReplicas+detail.AsleepReplicas, units)
	}
	// The denominator the drawer prints is this field, so it must be the unit
	// total rather than the stored 1 that PD creation writes.
	if detail.Replicas != units {
		t.Fatalf("detail replicas = %d, want the %d units the mode has", detail.Replicas, units)
	}
	if detail.Replicas < detail.AsleepReplicas {
		t.Fatalf("detail replicas = %d < asleep_replicas = %d: the drawer would print a numerator over a smaller denominator",
			detail.Replicas, detail.AsleepReplicas)
	}
	// The row the operator sees in the table describes the same deployment, so
	// its replicas must equal the unit total rather than the stored 1.
	row := f.listRow(t)
	if row.AsleepReplicas != detail.AsleepReplicas {
		t.Fatalf("list asleep_replicas = %d, detail = %d, want the same walk", row.AsleepReplicas, detail.AsleepReplicas)
	}
	if row.Replicas != units {
		t.Fatalf("list replicas = %d, want the %d units the mode has", row.Replicas, units)
	}
}

// The same invariant when every unit sleeps: the denominator is still the unit
// total, so the drawer reads "3/3" rather than a numerator over the stored 1.
func TestDeploymentPDDetailReplicasFullyAsleep(t *testing.T) {
	f := newPDStateFixture(t)
	f.sleepRole(t, "prefill", 1)
	f.sleepRole(t, "decode", 1)
	for _, uuid := range []string{"prefill-0", "prefill-1", "decode-0"} {
		f.reported(t, uuid, 1)
	}

	detail := f.handlers.deploymentDTO(mustDeployment(t, f.store, f.id), true)
	units := 0
	for _, role := range detail.PD.Roles {
		units += role.Desired
	}
	if units != 3 {
		t.Fatalf("unit total = %d, want 3", units)
	}
	if detail.AsleepReplicas != units {
		t.Fatalf("asleep_replicas = %d, want all %d units", detail.AsleepReplicas, units)
	}
	if detail.ReadyReplicas == nil || *detail.ReadyReplicas != 0 {
		t.Fatalf("ready_replicas = %v, want a counted 0", detail.ReadyReplicas)
	}
	if detail.Replicas != units {
		t.Fatalf("detail replicas = %d, want the %d units the mode has", detail.Replicas, units)
	}
	// The whole deployment is asleep, and still the denominator covers the
	// numerator: the pair a client prints is 0 ready over 3, asleep 3 of 3.
	if row := f.listRow(t); row.Replicas != detail.Replicas || row.AsleepReplicas != detail.AsleepReplicas {
		t.Fatalf("list = %d/%d, detail = %d/%d, want the same pair",
			row.AsleepReplicas, row.Replicas, detail.AsleepReplicas, detail.Replicas)
	}
}

// An ordinary deployment's detail is untouched by the PD rewrite: its replicas
// stay the stored count and its ready_replicas keeps counting the same walk.
func TestDeploymentOrdinaryDetailReplicasUnchanged(t *testing.T) {
	f := newSleepStateFixture(t)
	// "partial" is a two-replica deployment with one replica asleep.
	detail := f.detail(t, "partial")
	if detail.PD != nil {
		t.Fatalf("PD summary on an ordinary deployment: %+v", detail.PD)
	}
	if detail.Replicas != 2 {
		t.Fatalf("replicas = %d, want the stored 2", detail.Replicas)
	}
	if detail.ReadyReplicas == nil || *detail.ReadyReplicas != 1 {
		t.Fatalf("ready_replicas = %v, want 1", detail.ReadyReplicas)
	}
	if detail.AsleepReplicas != 1 {
		t.Fatalf("asleep_replicas = %d, want 1", detail.AsleepReplicas)
	}
	if detail.AsleepReplicas > detail.Replicas {
		t.Fatalf("asleep_replicas = %d > replicas = %d", detail.AsleepReplicas, detail.Replicas)
	}
	// And it matches the row, which never had the bug.
	row := f.row(t, "partial")
	if row.Replicas != detail.Replicas || row.AsleepReplicas != detail.AsleepReplicas {
		t.Fatalf("list = %d/%d, detail = %d/%d", row.AsleepReplicas, row.Replicas, detail.AsleepReplicas, detail.Replicas)
	}
}

// listRow reads this PD deployment from the list the way the table does.
func (f pdStateFixture) listRow(t *testing.T) dto.DeploymentListItem {
	t.Helper()
	rec := httptest.NewRecorder()
	f.handlers.listDeployments(rec, httptest.NewRequest(http.MethodGet, "/api/v1/deployments?page=1&page_size=100", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []dto.DeploymentListItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("page: %v", err)
	}
	for _, row := range page.Items {
		if row.ID == f.id {
			return row
		}
	}
	t.Fatalf("deployment %d absent from the list: %s", f.id, rec.Body.String())
	return dto.DeploymentListItem{}
}
