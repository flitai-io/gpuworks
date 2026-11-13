package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// instanceOf reads an instance row by uuid.
func instanceOf(t *testing.T, st *store.Store, uuid string) *store.Instance {
	t.Helper()
	inst, err := st.InstanceByUUID(uuid)
	if err != nil {
		t.Fatalf("instance %s: %v", uuid, err)
	}
	return inst
}

// stoppingRig is an entry made by hand selecting pool a, with agents that know
// where each node's labels put it: gpu-a1 and gpu-a2 are in pool a, gpu-b1 in b.
//
//	gpu-a1  chat, embed (running)   leaves when pool b is selected
//	gpu-a2  nothing running         leaves too, but runs nothing
//	gpu-b1  chat, batch (running)   stays: "chat" also runs elsewhere
type stoppingRig struct {
	*tokenRig
	agents     *selectionAgents
	uuid, base string
	a1, a2, b1 *store.Node
}

func newStoppingRig(t *testing.T) *stoppingRig {
	t.Helper()
	agents := &selectionAgents{labels: map[string]map[string]string{
		"gpu-a1": {"gpuworks.ai/pool": "a"}, "gpu-a2": {"gpuworks.ai/pool": "a"}, "gpu-b1": {"gpuworks.ai/pool": "b"},
	}, down: map[string]bool{}}
	rig := newSelectionRig(t, agents)
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"prod","server_url":"https://gw.example","node_selector":"gpuworks.ai/pool=a"}`))
	uuid := created["uuid"].(string)
	c := entryOf(t, rig, uuid)
	r := &stoppingRig{tokenRig: rig, agents: agents, uuid: uuid, base: "/api/v1/kube/clusters/" + uuid}
	r.a1 = seedKubeNode(t, rig.st, c, "gpu-a1", "chat", "embed")
	r.a2 = seedKubeNode(t, rig.st, c, "gpu-a2")
	r.b1 = seedKubeNode(t, rig.st, c, "gpu-b1", "chat", "batch")
	return r
}

func (r *stoppingRig) state(t *testing.T, uuid string) string {
	t.Helper()
	return instanceOf(t, r.st, uuid).State
}

func (r *stoppingRig) scheduling(t *testing.T, n *store.Node) store.NodeScheduling {
	t.Helper()
	got, err := r.st.NodeByUUID(n.UUID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Scheduling
}

// A confirmed change asks exactly the instances on the nodes that leave to
// stop, and answers which; deployments that also run elsewhere keep those
// replicas, and an unconfirmed request stops nothing.
func TestConfirmedSelectionChangeStopsOnlyInstancesOnLeavingNodes(t *testing.T) {
	r := newStoppingRig(t)
	// Also on the leaving node: a finished instance and one already being
	// removed stay as they are, a draining one is asked to stop.
	chat := seedDeployment(t, r.st, "chat")
	for uuid, state := range map[string]string{"done": proto.InstStopped, "leaving": proto.InstRemoving, "draining": proto.InstDraining} {
		if _, err := r.st.CreateInstance(&store.Instance{UUID: "a1-" + uuid, DeploymentID: chat.ID, NodeID: r.a1.ID, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	const body = `{"node_selector":"gpuworks.ai/pool=b"`

	// Unconfirmed: refused, nothing is touched.
	rec := r.do(r.session, http.MethodPut, r.base, body+`}`)
	refusal(t, rec.Code, rec.Body.String())
	for _, uuid := range []string{"inst-gpu-a1-chat", "inst-gpu-a1-embed", "inst-gpu-b1-chat", "inst-gpu-b1-batch"} {
		if got := r.state(t, uuid); got != proto.InstRunning {
			t.Fatalf("%s is %s after a refused edit", uuid, got)
		}
	}
	if r.scheduling(t, r.a1) != store.NodeSchedulable {
		t.Fatal("a refused edit cordoned a node")
	}

	// Confirmed: the answer lists the instances asked to stop, in the order they were created.
	rec = r.do(r.session, http.MethodPut, r.base, body+`,"confirm_drop_instances":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed = %d %s", rec.Code, rec.Body)
	}
	want := fmt.Sprintf(`[{"instance_uuid":"inst-gpu-a1-chat","deployment":"chat","node_uuid":%q},{"instance_uuid":"inst-gpu-a1-embed","deployment":"embed","node_uuid":%q},{"instance_uuid":"a1-draining","deployment":"chat","node_uuid":%q}]`, r.a1.UUID, r.a1.UUID, r.a1.UUID)
	got := decodeKube[map[string]any](t, rec)
	if string(mustCompact(t, mustJSON(t, got["stopped_instances"]))) != string(mustCompact(t, []byte(want))) {
		t.Fatalf("stopped_instances = %s\nwant %s", mustJSON(t, got["stopped_instances"]), want)
	}
	if got["warnings"] == nil || fmt.Sprint(got["warnings"]) != "[]" || got["reapply"] != true {
		t.Fatalf("answer = %s", rec.Body)
	}

	// Only those instances were asked to stop, with the reason on them.
	for _, uuid := range []string{"inst-gpu-a1-chat", "inst-gpu-a1-embed", "a1-draining"} {
		inst := instanceOf(t, r.st, uuid)
		if inst.State != proto.InstRemoving || inst.Message != kubeSelectionStopMessage {
			t.Errorf("%s = %s %q; want removing with the reason", uuid, inst.State, inst.Message)
		}
	}
	for uuid, state := range map[string]string{"inst-gpu-b1-chat": proto.InstRunning, "inst-gpu-b1-batch": proto.InstRunning, "a1-done": proto.InstStopped, "a1-leaving": proto.InstRemoving} {
		if got := r.state(t, uuid); got != state {
			t.Errorf("%s is %s; want %s untouched", uuid, got, state)
		}
	}
	if m := instanceOf(t, r.st, "a1-leaving").Message; m == kubeSelectionStopMessage {
		t.Error("an instance already being removed was written again")
	}
	// The nodes the server knows leave are cordoned, so nothing is rebuilt there; the others are not.
	if r.scheduling(t, r.a1) != store.NodeCordoned || r.scheduling(t, r.b1) != store.NodeSchedulable {
		t.Fatalf("scheduling: a1 %q, b1 %q", r.scheduling(t, r.a1), r.scheduling(t, r.b1))
	}
	// The entry was saved, and the stop is on the timeline.
	if entryOf(t, r.tokenRig, r.uuid).NodeSelector != "gpuworks.ai/pool=b" {
		t.Fatal("the confirmed edit was not saved")
	}
	events, err := r.st.InstanceEventsByUUID("inst-gpu-a1-chat", 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("events = %v, %v", events, err)
	}
}

// Confirming a change that takes no instance with it stops nothing: the
// selection is unchanged, only widens, or the nodes that leave run nothing.
func TestConfirmingWithoutLeavingInstancesStopsNothing(t *testing.T) {
	r := newStoppingRig(t)
	for _, body := range []string{
		`{"priority":5,"confirm_drop_instances":true}`,                          // no selection change
		`{"node_selector":"gpuworks.ai/pool=a,","confirm_drop_instances":true}`, // same selector spelled differently
		`{"node_selector":"","confirm_drop_instances":true}`,                    // widened to every node
	} {
		rec := r.do(r.session, http.MethodPut, r.base, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", body, rec.Code, rec.Body)
		}
		if got := decodeKube[map[string]any](t, rec); fmt.Sprint(got["stopped_instances"]) != "[]" {
			t.Fatalf("%s stopped %v", body, got["stopped_instances"])
		}
	}
	// Narrowed to pool b, which gpu-a1 is now in: only gpu-a2, which runs nothing, leaves.
	r.agents.labels["gpu-a1"]["gpuworks.ai/pool"] = "b"
	if rec := r.do(r.session, http.MethodPut, r.base, `{"node_selector":"gpuworks.ai/pool=b","confirm_drop_instances":true}`); rec.Code != http.StatusOK {
		t.Fatalf("nothing that runs instances leaves = %d %s", rec.Code, rec.Body)
	}
	for _, uuid := range []string{"inst-gpu-a1-chat", "inst-gpu-a1-embed", "inst-gpu-b1-chat", "inst-gpu-b1-batch"} {
		if got := r.state(t, uuid); got != proto.InstRunning {
			t.Errorf("%s is %s; nothing should have stopped", uuid, got)
		}
	}
	if r.scheduling(t, r.a1) != store.NodeSchedulable || r.scheduling(t, r.a2) != store.NodeSchedulable {
		t.Fatal("a node was cordoned although none that runs instances leaves")
	}
}

// With no agent to ask, the nodes listed (every node that runs instances) are
// stopped as the confirmation said, but not cordoned: they may be selected.
// A node an operator already cordoned keeps its state either way.
func TestConfirmedUnverifiedSelectionStopsListedNodesWithoutCordoning(t *testing.T) {
	r := newStoppingRig(t)
	r.agents.down[r.a1.UUID], r.agents.down[r.a2.UUID], r.agents.down[r.b1.UUID] = true, true, true
	rec := r.do(r.session, http.MethodPut, r.base, `{"node_selector":"gpuworks.ai/pool=b","confirm_drop_instances":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed = %d %s", rec.Code, rec.Body)
	}
	stopped := decodeKube[struct {
		Stopped []struct {
			InstanceUUID string `json:"instance_uuid"`
		} `json:"stopped_instances"`
	}](t, rec).Stopped
	if len(stopped) != 4 {
		t.Fatalf("stopped = %+v; want the four instances of the two nodes that run any", stopped)
	}
	for _, n := range []*store.Node{r.a1, r.a2, r.b1} {
		if r.scheduling(t, n) != store.NodeSchedulable {
			t.Errorf("%s was cordoned although the server could not tell it leaves", n.Name)
		}
	}
}

func TestConfirmedSelectionChangeKeepsAnOperatorsCordonAndDrain(t *testing.T) {
	r := newStoppingRig(t)
	if _, err := r.st.SetNodeScheduling(r.a1.UUID, store.NodeDraining); err != nil {
		t.Fatal(err)
	}
	if rec := r.do(r.session, http.MethodPut, r.base, `{"node_selector":"gpuworks.ai/pool=b","confirm_drop_instances":true}`); rec.Code != http.StatusOK {
		t.Fatalf("confirmed = %d %s", rec.Code, rec.Body)
	}
	if r.scheduling(t, r.a1) != store.NodeDraining {
		t.Fatalf("a drained node became %q", r.scheduling(t, r.a1))
	}
	if got := r.state(t, "inst-gpu-a1-chat"); got != proto.InstRemoving {
		t.Fatalf("its instance is %s", got)
	}
}

// Taking node pools away answers the entry as before, with the instances
// asked to stop beside it.
func TestConfirmedPoolsChangeStopsInstancesOnLeavingNodes(t *testing.T) {
	rig := newKubeSourceRig(t)
	const label = "alibabacloud.com/nodepool-id"
	agents := &selectionAgents{labels: map[string]map[string]string{"n-1": {label: "np-1"}, "n-2": {label: "np-2"}}, down: map[string]bool{}}
	rig.agents = agents.call
	uuid, _ := rig.createFromPools("ack", "np-1", "np-2")
	path := "/api/v1/kube/clusters/" + uuid + "/pools"
	c := entryOf(t, rig.tokenRig, uuid)
	n1 := seedKubeNode(t, rig.st, c, "n-1", "chat")
	seedKubeNode(t, rig.st, c, "n-2", "chat", "embed")

	// Unconfirmed: nothing stops.
	rec := rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-2"]}`)
	refusal(t, rec.Code, rec.Body.String())
	if got := instanceOf(t, rig.st, "inst-n-1-chat").State; got != proto.InstRunning {
		t.Fatalf("a refused change left the instance %s", got)
	}

	rec = rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-2"],"confirm_drop_instances":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed = %d %s", rec.Code, rec.Body)
	}
	got := decodeKube[map[string]any](t, rec)
	want := fmt.Sprintf(`[{"instance_uuid":"inst-n-1-chat","deployment":"chat","node_uuid":%q}]`, n1.UUID)
	if string(mustCompact(t, mustJSON(t, got["stopped_instances"]))) != string(mustCompact(t, []byte(want))) {
		t.Fatalf("stopped_instances = %s\nwant %s", mustJSON(t, got["stopped_instances"]), want)
	}
	// The entry's own fields stay at the top level, as when the answer was the entry alone.
	if got["uuid"] != uuid || got["name"] != "ack" || got["id"] == nil {
		t.Fatalf("the entry is no longer the body of the answer: %s", rec.Body)
	}
	if st := instanceOf(t, rig.st, "inst-n-1-chat"); st.State != proto.InstRemoving {
		t.Fatalf("n-1's instance is %s", st.State)
	}
	for _, uuid := range []string{"inst-n-2-chat", "inst-n-2-embed"} {
		if got := instanceOf(t, rig.st, uuid).State; got != proto.InstRunning {
			t.Errorf("%s is %s: its node stays", uuid, got)
		}
	}

	// A change that takes nothing with it says so with an empty list, not a missing key.
	rec = rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-2","np-1"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("widen = %d %s", rec.Code, rec.Body)
	}
	if got := decodeKube[map[string]any](t, rec); fmt.Sprint(got["stopped_instances"]) != "[]" {
		t.Fatalf("stopped_instances = %v", got["stopped_instances"])
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// failInstanceWrites makes every update of the instances table fail until the
// test ends, standing in for a store error after the entry's own write.
func failInstanceWrites(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.DB().Callback().Update().Before("gorm:update").Register("test:fail-instances", func(db *gorm.DB) {
		if db.Statement.Table == "instances" {
			_ = db.AddError(errors.New("instances cannot be written"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Update().Remove("test:fail-instances") })
}

// untouched asserts a refused or failed change stopped nothing and cordoned nothing.
func (r *stoppingRig) untouched(t *testing.T) {
	t.Helper()
	for _, uuid := range []string{"inst-gpu-a1-chat", "inst-gpu-a1-embed", "inst-gpu-b1-chat", "inst-gpu-b1-batch"} {
		if got := r.state(t, uuid); got != proto.InstRunning {
			t.Errorf("%s is %s; nothing should have stopped", uuid, got)
		}
	}
	for _, n := range []*store.Node{r.a1, r.a2, r.b1} {
		if r.scheduling(t, n) != store.NodeSchedulable {
			t.Errorf("%s was cordoned", n.Name)
		}
	}
	if entryOf(t, r.tokenRig, r.uuid).NodeSelector != "gpuworks.ai/pool=a" {
		t.Error("the entry was changed")
	}
}

// A confirmed edit whose save fails stops nothing: the name is taken, a field
// is invalid, or the store fails after the entry was written.
func TestConfirmedEditThatFailsToSaveStopsNothing(t *testing.T) {
	r := newStoppingRig(t)
	if rec := r.do(r.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"taken","server_url":"https://gw.example"}`); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("second entry = %d %s", rec.Code, rec.Body)
	}
	const move = `"node_selector":"gpuworks.ai/pool=b","confirm_drop_instances":true`

	rec := r.do(r.session, http.MethodPut, r.base, `{"name":"taken",`+move+`}`)
	var e struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusConflict || e.Code != ErrorKubeClusterNameTaken {
		t.Fatalf("name taken = %d %s", rec.Code, rec.Body)
	}
	r.untouched(t)

	if rec = r.do(r.session, http.MethodPut, r.base, `{"engine_ports":"nonsense",`+move+`}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid field = %d %s", rec.Code, rec.Body)
	}
	r.untouched(t)

	failInstanceWrites(t, r.st)
	if rec = r.do(r.session, http.MethodPut, r.base, `{"name":"renamed",`+move+`}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure = %d %s", rec.Code, rec.Body)
	}
	if entryOf(t, r.tokenRig, r.uuid).Name != "prod" {
		t.Fatal("the entry was renamed although stopping its instances failed")
	}
	r.untouched(t)
}

// The pools endpoint is the same: a pool the cloud does not have, or a store
// failure after the pools were written, stops nothing and changes no pools.
func TestConfirmedPoolsChangeThatFailsToSaveStopsNothing(t *testing.T) {
	rig := newKubeSourceRig(t)
	const label = "alibabacloud.com/nodepool-id"
	agents := &selectionAgents{labels: map[string]map[string]string{"n-1": {label: "np-1"}, "n-2": {label: "np-2"}}, down: map[string]bool{}}
	rig.agents = agents.call
	uuid, _ := rig.createFromPools("ack", "np-1", "np-2")
	path := "/api/v1/kube/clusters/" + uuid + "/pools"
	c := entryOf(t, rig.tokenRig, uuid)
	n1 := seedKubeNode(t, rig.st, c, "n-1", "chat")
	seedKubeNode(t, rig.st, c, "n-2", "embed")
	untouched := func() {
		t.Helper()
		for _, id := range []string{"inst-n-1-chat", "inst-n-2-embed"} {
			if got := instanceOf(t, rig.st, id).State; got != proto.InstRunning {
				t.Errorf("%s is %s; nothing should have stopped", id, got)
			}
		}
		if got, err := rig.st.NodeByUUID(n1.UUID); err != nil || got.Scheduling != store.NodeSchedulable {
			t.Errorf("n-1 = %+v, %v; want schedulable", got, err)
		}
		if rig.poolsOf(uuid) != `["np-1","np-2"]` {
			t.Errorf("pools = %s", rig.poolsOf(uuid))
		}
	}

	// A pool the cluster does not have is refused before anything else.
	if rec := rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-2","np-gone"],"confirm_drop_instances":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown pool = %d %s", rec.Code, rec.Body)
	}
	untouched()

	failInstanceWrites(t, rig.st)
	if rec := rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-2"],"confirm_drop_instances":true}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure = %d %s", rec.Code, rec.Body)
	}
	untouched()
}
