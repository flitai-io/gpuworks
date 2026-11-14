package api

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// sleepStateFixture seeds the states a sleep order produces: an awake
// sleepable deployment, one under a sleep order its engines have not carried
// out yet (the drain window), one fully asleep, and a two-replica deployment
// with one replica asleep.
type sleepStateFixture struct {
	listFixture
	node *store.Node
}

func newSleepStateFixture(t *testing.T) sleepStateFixture {
	t.Helper()
	f := listFixture{st: openSleepStateStore(t), handler: nil}
	f.handler = &apiHandlers{d: Deps{Store: f.st}}
	node, err := f.st.RegisterNode("sleep-state-node", store.HashToken("sleep-state-node"), "sleep-state-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	model, err := f.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/sleep", Revision: "main", Name: "org/sleep",
		Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/m"}); err != nil {
		t.Fatal(err)
	}
	fix := sleepStateFixture{listFixture: f, node: node}

	makeDeployment := func(name string, replicas int) *store.Deployment {
		dep, err := f.st.CreateDeployment(&store.Deployment{Name: name, ServedName: name, Engine: "vllm", ModelID: model.ID,
			NodeID: node.ID, Replicas: replicas, Sleepable: true})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < replicas; i++ {
			uuid := fmt.Sprintf("%s-%d", name, i)
			if _, err := f.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
				GPUIndexes: fmt.Sprintf("[%d]", i), APIPort: 8000,
				DesiredStateJSON: `{"uuid":"` + uuid + `","desired_state":"running","sleepable":true}`}); err != nil {
				t.Fatal(err)
			}
		}
		return dep
	}
	// awake keeps serving; draining has the order and no reported level yet,
	// which is what the gateway refuses on; asleep reports the level it was
	// ordered to; partial sleeps one of its two replicas.
	makeDeployment("awake", 1)
	draining := makeDeployment("draining", 1)
	asleep := makeDeployment("asleep", 1)
	partial := makeDeployment("partial", 2)
	for _, dep := range []*store.Deployment{draining, asleep} {
		if err := f.st.OrderDeploymentSleep(dep.ID, 1, "wait"); err != nil {
			t.Fatal(err)
		}
	}
	reportAsleep(t, f.st, "asleep-0", 1)
	if err := f.st.OrderInstancesSleep(partial.ID, []string{"partial-0"}, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	reportAsleep(t, f.st, "partial-0", 1)
	return fix
}

func openSleepStateStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// sleepRow is one list row read back as the wire carries it, with the sleep
// keys kept raw so a test can tell an absent key from a zero.
type sleepRow struct {
	dto.DeploymentListItem
	raw map[string]json.RawMessage
}

func (f sleepStateFixture) row(t *testing.T, name string) sleepRow {
	t.Helper()
	body := f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&page_size=100&q="+name)
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("page %s: %v", body, err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("q=%s matched %d rows: %s", name, len(page.Items), body)
	}
	row := sleepRow{raw: map[string]json.RawMessage{}}
	if err := json.Unmarshal(page.Items[0], &row.DeploymentListItem); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(page.Items[0], &row.raw); err != nil {
		t.Fatal(err)
	}
	if row.Name != name {
		t.Fatalf("q=%s matched %q", name, row.Name)
	}
	return row
}

func (f sleepStateFixture) detail(t *testing.T, name string) dto.Deployment {
	t.Helper()
	deps, err := f.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	for i := range deps {
		if deps[i].Name == name {
			return f.handler.deploymentDTO(&deps[i], false)
		}
	}
	t.Fatalf("no deployment %q", name)
	return dto.Deployment{}
}

// A sleep order reaches both the list row and the single-deployment read as
// the same state, and ready_replicas and asleep_replicas describe the same
// replicas: a replica the gateway refuses for sleep is never also counted
// ready, and one it serves is never counted asleep.
func TestDeploymentWireCarriesSleepState(t *testing.T) {
	f := newSleepStateFixture(t)
	for _, tc := range []struct {
		name        string
		wantState   string
		wantRuntime string
		ready       int
		asleep      int  // asleep_replicas, 0 when the key is absent
		hasAsleep   bool // whether asleep_replicas is on the wire
		level       int
		hasLevel    bool
	}{
		{name: "awake", wantState: "running", wantRuntime: "running", ready: 1},
		// The order is accepted and the gateway already refuses, while the
		// engine still reports the level it had: the drain window. The replica
		// is asleep for the gateway, so it is not ready too.
		{name: "draining", wantState: "sleeping", wantRuntime: "running", asleep: 1, hasAsleep: true},
		{name: "asleep", wantState: "asleep", wantRuntime: "running", asleep: 1, hasAsleep: true, level: 1, hasLevel: true},
		// One replica of two sleeps: the deployment still serves, so it keeps
		// its running state and the counts say what is asleep.
		{name: "partial", wantState: "running", wantRuntime: "running", ready: 1, asleep: 1, hasAsleep: true, level: 1, hasLevel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := f.row(t, tc.name)
			if row.State != tc.wantState || row.RuntimeState != tc.wantRuntime {
				t.Fatalf("list state/runtime = %q/%q, want %q/%q", row.State, row.RuntimeState, tc.wantState, tc.wantRuntime)
			}
			if row.ReadyReplicas == nil || *row.ReadyReplicas != tc.ready {
				t.Fatalf("list ready_replicas = %v, want %d", row.ReadyReplicas, tc.ready)
			}
			if row.AsleepReplicas != tc.asleep {
				t.Fatalf("list asleep_replicas = %d, want %d", row.AsleepReplicas, tc.asleep)
			}
			if _, ok := row.raw["asleep_replicas"]; ok != tc.hasAsleep {
				t.Fatalf("list asleep_replicas present = %v, want %v (row %v)", ok, tc.hasAsleep, row.raw)
			}
			if _, ok := row.raw["sleep_level"]; ok != tc.hasLevel {
				t.Fatalf("list sleep_level present = %v, want %v (row %v)", ok, tc.hasLevel, row.raw)
			}
			if row.SleepLevel != tc.level {
				t.Fatalf("list sleep_level = %d, want %d", row.SleepLevel, tc.level)
			}
			// The detail must not disagree with the row it was listed from.
			detail := f.detail(t, tc.name)
			if detail.State != tc.wantState || detail.RuntimeState != tc.wantRuntime {
				t.Fatalf("detail state/runtime = %q/%q, want %q/%q", detail.State, detail.RuntimeState, tc.wantState, tc.wantRuntime)
			}
			if detail.ReadyReplicas == nil || *detail.ReadyReplicas != tc.ready {
				t.Fatalf("detail ready_replicas = %v, want %d", detail.ReadyReplicas, tc.ready)
			}
			if detail.AsleepReplicas != tc.asleep || detail.SleepLevel != tc.level {
				t.Fatalf("detail asleep_replicas/sleep_level = %d/%d, want %d/%d", detail.AsleepReplicas, detail.SleepLevel, tc.asleep, tc.level)
			}
		})
	}
}

// The list filter accepts the states a sleep order shows, and each one matches
// exactly the deployments showing it.
func TestDeploymentStateFilterMatchesSleepStates(t *testing.T) {
	f := newSleepStateFixture(t)
	for _, tc := range []struct {
		state string
		want  []string
	}{
		{state: "asleep", want: []string{"asleep"}},
		{state: "sleeping", want: []string{"draining"}},
		// A partly slept deployment serves, so the running filter keeps it.
		{state: "running", want: []string{"awake", "partial"}},
	} {
		body := f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&page_size=100&state="+tc.state)
		page := decodeDeploymentPage(t, body)
		var got []string
		for _, item := range page.Items {
			got = append(got, item.Name)
			if item.State != tc.state && tc.state != "running" {
				t.Fatalf("state=%s returned a row showing %q", tc.state, item.State)
			}
		}
		sort.Strings(got)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("state=%s = %v, want %v (%s)", tc.state, got, tc.want, body)
		}
	}
}

// The state order sorts by the state each row shows.
func TestDeploymentStateSortUsesTheShownState(t *testing.T) {
	f := newSleepStateFixture(t)
	body := f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&page_size=100&sort=state")
	page := decodeDeploymentPage(t, body)
	if len(page.Items) != 4 {
		t.Fatalf("rows = %d, want 4", len(page.Items))
	}
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i-1].State > page.Items[i].State {
			t.Fatalf("state order %q before %q is not ascending", page.Items[i-1].State, page.Items[i].State)
		}
	}
}

// A deployment whose instances cannot serve is never reported asleep, however
// its instances were ordered to sleep: there is no replica to wake.
func TestDeploymentWithoutServingReplicaIsNotAsleep(t *testing.T) {
	f := newSleepStateFixture(t)
	for _, tc := range []struct {
		name  string
		state string
	}{
		{name: "stopped-instance", state: proto.InstStopped},
		{name: "failed-instance", state: proto.InstFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep, err := f.st.CreateDeployment(&store.Deployment{Name: tc.name, ServedName: tc.name, Engine: "vllm",
				NodeID: f.node.ID, Replicas: 1, Sleepable: true})
			if err != nil {
				t.Fatal(err)
			}
			inst, err := f.st.CreateInstance(&store.Instance{UUID: tc.name + "-0", DeploymentID: dep.ID, NodeID: f.node.ID,
				State: proto.InstRunning, GPUIndexes: "[0]", APIPort: 8000,
				DesiredStateJSON: `{"uuid":"` + tc.name + `-0","desired_state":"running","sleepable":true}`})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.st.OrderDeploymentSleep(dep.ID, 1, "wait"); err != nil {
				t.Fatal(err)
			}
			reportAsleep(t, f.st, inst.UUID, 1)
			if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: tc.state, Message: "gone"}); err != nil {
				t.Fatal(err)
			}
			row := f.row(t, tc.name)
			if row.State == deployment.StateAsleep || row.State == deployment.StateSleeping {
				t.Fatalf("state = %q with no serving replica", row.State)
			}
			if row.AsleepReplicas != 0 {
				t.Fatalf("asleep_replicas = %d with no serving replica", row.AsleepReplicas)
			}
		})
	}
}

// A stopped deployment that still carries a sleep order stays stopped.
func TestStoppedDeploymentWithSleepOrderStaysStopped(t *testing.T) {
	f := newSleepStateFixture(t)
	deps, err := f.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	for i := range deps {
		if deps[i].Name == "asleep" {
			if err := f.st.SetDeploymentState(deps[i].ID, store.DeployStopped); err != nil {
				t.Fatal(err)
			}
		}
	}
	row := f.row(t, "asleep")
	if row.State != "stopped" {
		t.Fatalf("state = %q, want stopped", row.State)
	}
	detail := f.detail(t, "asleep")
	if detail.State != "stopped" {
		t.Fatalf("detail state = %q, want stopped", detail.State)
	}
}

// Deepening a sleep level is a transition the row has to show. A change
// between two levels goes through awake, because the engine only sleeps from
// awake (engine.applySleepLevel wakes first), so a level-2 order on a level-1
// engine reports level 1 for the whole wake-then-offload sequence. Comparing
// the reported level against the level ordered is what separates "the order
// did not start" from "the order finished": without it the row reads asleep
// from the click to completion, byte-identical to the state before it, and a
// rollback that rewrites the order to the reported level leaves no field
// saying the deep sleep never happened.
func TestDeploymentDeepeningSleepReadsSleepingUntilTheEngineArrives(t *testing.T) {
	f := newSleepStateFixture(t)
	deps, err := f.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	var id uint
	for i := range deps {
		if deps[i].Name == "asleep" {
			id = deps[i].ID
		}
	}
	if id == 0 {
		t.Fatal("no deployment asleep")
	}
	// Settled at level 1 is asleep, the state the operator starts from.
	if got := f.row(t, "asleep"); got.State != "asleep" || got.SleepLevel != 1 {
		t.Fatalf("settled = state %q sleep_level %d, want asleep at 1", got.State, got.SleepLevel)
	}
	// Order level 2. The engine has not carried it out, so it still reports 1:
	// the order is in flight and the row must say so rather than reusing the
	// settled state it showed a moment earlier.
	if err := f.st.OrderDeploymentSleep(id, 2, "wait"); err != nil {
		t.Fatal(err)
	}
	if got := f.row(t, "asleep"); got.State != "sleeping" || got.SleepLevel != 1 || got.AsleepReplicas != 1 {
		t.Fatalf("ordered again = state %q sleep_level %d asleep %d, want sleeping at the reported 1", got.State, got.SleepLevel, got.AsleepReplicas)
	}
	if got := f.detail(t, "asleep"); got.State != "sleeping" {
		t.Fatalf("detail state = %q, want sleeping while the level-2 order is in flight", got.State)
	}
	// The engine arrives at 2: settled again, and the deeper level shows.
	reportAsleep(t, f.st, "asleep-0", 2)
	if got := f.row(t, "asleep"); got.State != "asleep" || got.SleepLevel != 2 {
		t.Fatalf("arrived = state %q sleep_level %d, want asleep at 2", got.State, got.SleepLevel)
	}
}

// A failed deepening rolls the order back to the level the engine reports. The
// row must then read asleep at that level again rather than staying in the
// transition forever.
func TestDeploymentFailedDeepeningSettlesBackToTheReportedLevel(t *testing.T) {
	f := newSleepStateFixture(t)
	deps, err := f.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	var id uint
	for i := range deps {
		if deps[i].Name == "asleep" {
			id = deps[i].ID
		}
	}
	if id == 0 {
		t.Fatal("no deployment asleep")
	}
	if err := f.st.OrderDeploymentSleep(id, 2, "wait"); err != nil {
		t.Fatal(err)
	}
	if got := f.row(t, "asleep"); got.State != "sleeping" {
		t.Fatalf("ordered = state %q, want sleeping", got.State)
	}
	// The engine's level-2 attempt failed and the store rewrote the order back
	// to the level it is still at, which is what rollbackSleepIntent does.
	if err := f.st.OrderDeploymentSleep(id, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	if got := f.row(t, "asleep"); got.State != "asleep" || got.SleepLevel != 1 {
		t.Fatalf("rolled back = state %q sleep_level %d, want asleep at 1", got.State, got.SleepLevel)
	}
}
