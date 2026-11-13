package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// selectionAgents stands in for the agents of one cluster. It answers
// RPCKubeNodesSelected the way the real agent does: from the labels of the
// nodes it was given, a node it does not know being gone. It refuses any
// other RPC and any peer it was told to fail.
type selectionAgents struct {
	mu     sync.Mutex
	labels map[string]map[string]string // kube node name -> labels
	down   map[string]bool              // agent (node uuid) -> unreachable
	asked  []string                     // agent uuids, in the order the server called them
	last   proto.KubeNodesSelectedParams
}

func (s *selectionAgents) call(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if method != proto.RPCKubeNodesSelected {
		return nil, fmt.Errorf("unexpected RPC %s", method)
	}
	req, ok := params.(proto.KubeNodesSelectedParams)
	if !ok {
		return nil, fmt.Errorf("params are %T", params)
	}
	s.asked = append(s.asked, uuid)
	if s.down[uuid] {
		return nil, errors.New("node is offline")
	}
	s.last = req
	out := proto.KubeNodesSelectedResult{Selected: map[string]bool{}}
	for _, name := range req.NodeNames {
		labels, exists := s.labels[name]
		selected := exists
		for k, v := range req.Selector {
			if got, has := labels[k]; !has || got != v {
				selected = false
			}
		}
		if req.PoolLabel != "" && !slices.Contains(req.PoolIDs, labels[req.PoolLabel]) {
			selected = false
		}
		out.Selected[name] = selected
	}
	return json.Marshal(out)
}

func (s *selectionAgents) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.asked)
}

// seedKubeNode enrolls a node of an entry, online, with one running instance
// per named deployment.
func seedKubeNode(t *testing.T, st *store.Store, c *store.KubeCluster, name string, deployments ...string) *store.Node {
	t.Helper()
	n, _, err := st.EnrollKubeNode(c.Token, "uid-"+c.Name, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(n.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	for _, dep := range deployments {
		d := seedDeployment(t, st, dep)
		if _, err := st.CreateInstance(&store.Instance{UUID: "inst-" + name + "-" + dep, DeploymentID: d.ID, NodeID: n.ID, State: proto.InstRunning}); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

func seedDeployment(t *testing.T, st *store.Store, name string) *store.Deployment {
	t.Helper()
	all, err := st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i]
		}
	}
	d, err := st.CreateDeployment(&store.Deployment{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// refusal reads a 409 KUBE_SELECTION_DROPS_INSTANCES: its params, as the
// client gets them.
func refusal(t *testing.T, code int, body string) json.RawMessage {
	t.Helper()
	var e struct {
		Error  string          `json:"error"`
		Code   string          `json:"code"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if code != http.StatusConflict || e.Code != ErrorKubeSelectionDropsInstances {
		t.Fatalf("answer = %d %s; want 409 %s", code, body, ErrorKubeSelectionDropsInstances)
	}
	for _, want := range []string{"agents are removed when the manifest is applied again", "keep running", "no longer managed", "Stop or move those deployments first, or confirm", "Nothing changes in the cluster until the manifest is applied again"} {
		if !strings.Contains(e.Error, want) {
			t.Errorf("the English fallback lacks %q: %s", want, e.Error)
		}
	}
	return e.Params
}

// newSelectionRig is a router over a real store, an admin session and the
// given agents.
func newSelectionRig(t *testing.T, agents *selectionAgents) *tokenRig {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	admin, _ := st.VerifyUser("admin", "gpuworks-test-password-2026")
	signer, err := auth.NewTokenSigner("kube-selection-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: admin.ID, Username: admin.Username, Role: admin.Role})
	if err != nil {
		t.Fatal(err)
	}
	return &tokenRig{t: t, h: Router(Deps{AuthSigner: signer, Store: st, AI: &fakeAI{available: true}, CallNode: agents.call}), st: st, session: session}
}

func entryOf(t *testing.T, rig *tokenRig, uuid string) *store.KubeCluster {
	t.Helper()
	c, err := rig.st.KubeClusterByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// An entry made by hand: narrowing its node selector so that a node that
// runs instances leaves it is refused until confirmed.
func TestNarrowingTheNodeSelectorOfAnEntryAsksBeforeDroppingInstances(t *testing.T) {
	agents := &selectionAgents{labels: map[string]map[string]string{
		"gpu-a1": {"gpuworks.ai/pool": "a"}, "gpu-b1": {"gpuworks.ai/pool": "b"}, "gpu-b2": {"gpuworks.ai/pool": "b"},
	}, down: map[string]bool{}}
	rig := newSelectionRig(t, agents)
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"prod","server_url":"https://gw.example","node_selector":"gpuworks.ai/pool=a"}`))
	uuid := created["uuid"].(string)
	base := "/api/v1/kube/clusters/" + uuid
	c := entryOf(t, rig, uuid)
	a1 := seedKubeNode(t, rig.st, c, "gpu-a1", "chat", "embed")
	seedKubeNode(t, rig.st, c, "gpu-b1", "chat")
	seedKubeNode(t, rig.st, c, "gpu-b2") // runs nothing

	// Pool b is selected instead of a: gpu-a1 leaves with two deployments.
	rec := rig.do(rig.session, http.MethodPut, base, `{"node_selector":"gpuworks.ai/pool=b"}`)
	params := refusal(t, rec.Code, rec.Body.String())
	want := fmt.Sprintf(`{"verified":true,"nodes":[{"node_uuid":%q,"node_name":"gpu-a1","kube_node":"gpu-a1","deployments":["chat","embed"]}]}`, a1.UUID)
	if string(mustCompact(t, params)) != string(mustCompact(t, []byte(want))) {
		t.Fatalf("params = %s\nwant %s", params, want)
	}
	// The agent was asked about the nodes that run instances only, with the new selection.
	if got := agents.last; !slices.Equal(got.NodeNames, []string{"gpu-a1", "gpu-b1"}) || got.Selector["gpuworks.ai/pool"] != "b" || got.PoolLabel != "" {
		t.Fatalf("question = %+v", got)
	}
	if entryOf(t, rig, uuid).NodeSelector != "gpuworks.ai/pool=a" {
		t.Fatal("a refused edit changed the entry")
	}

	// Saying no to the prompt (an explicit false) is the same as not answering.
	rec = rig.do(rig.session, http.MethodPut, base, `{"node_selector":"gpuworks.ai/pool=b","confirm_drop_instances":false}`)
	refusal(t, rec.Code, rec.Body.String())

	// Confirmed, it goes through as before; the cluster changes when the manifest is applied.
	rec = rig.do(rig.session, http.MethodPut, base, `{"node_selector":"gpuworks.ai/pool=b","confirm_drop_instances":true}`)
	got := decodeKube[map[string]any](t, rec)
	if rec.Code != http.StatusOK || got["reapply"] != true || fmt.Sprint(got["changed"]) != "[node_selector]" ||
		got["cluster"].(map[string]any)["node_selector"] != "gpuworks.ai/pool=b" {
		t.Fatalf("confirmed edit = %d %s", rec.Code, rec.Body)
	}
}

// Widening the selection, or narrowing it where only nodes without
// instances leave, asks nothing; neither does an edit of another setting.
func TestOnlyAChangedSelectionWithInstancesOnLeavingNodesIsAsked(t *testing.T) {
	agents := &selectionAgents{labels: map[string]map[string]string{
		"gpu-a1": {"gpuworks.ai/pool": "a"}, "gpu-b1": {"gpuworks.ai/pool": "b"},
	}, down: map[string]bool{}}
	rig := newSelectionRig(t, agents)
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"prod","server_url":"https://gw.example","node_selector":"gpuworks.ai/pool=a"}`))
	uuid := created["uuid"].(string)
	base := "/api/v1/kube/clusters/" + uuid
	c := entryOf(t, rig, uuid)

	// No node runs anything: no question, no prompt.
	seedKubeNode(t, rig.st, c, "gpu-a1")
	seedKubeNode(t, rig.st, c, "gpu-b1")
	if rec := rig.do(rig.session, http.MethodPut, base, `{"node_selector":"gpuworks.ai/pool=b"}`); rec.Code != http.StatusOK {
		t.Fatalf("no instances = %d %s", rec.Code, rec.Body)
	}
	if agents.calls() != 0 {
		t.Fatalf("an agent was asked with no instance at stake: %v", agents.asked)
	}

	// The selection stays: another setting changes, or the same selector is spelled differently.
	seedKubeNode(t, rig.st, c, "gpu-b2", "chat")
	agents.labels["gpu-b2"] = map[string]string{"gpuworks.ai/pool": "b"}
	for _, body := range []string{`{"name":"prod-2"}`, `{"priority":5}`, `{"node_selector":" gpuworks.ai/pool=b "}`, `{"node_selector":"gpuworks.ai/pool=b,"}`} {
		if rec := rig.do(rig.session, http.MethodPut, base, body); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", body, rec.Code, rec.Body)
		}
	}
	if agents.calls() != 0 {
		t.Fatalf("an agent was asked although the selection did not change: %v", agents.asked)
	}

	// Widening: the agent is asked and every node that runs instances stays.
	if rec := rig.do(rig.session, http.MethodPut, base, `{"node_selector":""}`); rec.Code != http.StatusOK {
		t.Fatalf("widen = %d %s", rec.Code, rec.Body)
	}
	if agents.calls() != 1 || !slices.Equal(agents.last.NodeNames, []string{"gpu-b2"}) {
		t.Fatalf("widening asked %v about %v", agents.asked, agents.last.NodeNames)
	}

	// A node that runs instances but stays is no reason to ask the user.
	if rec := rig.do(rig.session, http.MethodPut, base, `{"node_selector":"gpuworks.ai/pool=b"}`); rec.Code != http.StatusOK {
		t.Fatalf("narrow to where the instances are = %d %s", rec.Code, rec.Body)
	}
}

// With no agent to ask, every node that runs instances may be affected, and
// the refusal says it was not verified.
func TestNarrowingASelectionWithNoAgentToAskIsUnverified(t *testing.T) {
	agents := &selectionAgents{labels: map[string]map[string]string{
		"gpu-a1": {"gpuworks.ai/pool": "a"}, "gpu-b1": {"gpuworks.ai/pool": "b"},
	}, down: map[string]bool{}}
	rig := newSelectionRig(t, agents)
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"prod","server_url":"https://gw.example","node_selector":"gpuworks.ai/pool=a"}`))
	uuid := created["uuid"].(string)
	base := "/api/v1/kube/clusters/" + uuid
	c := entryOf(t, rig, uuid)
	a1 := seedKubeNode(t, rig.st, c, "gpu-a1", "chat")
	b1 := seedKubeNode(t, rig.st, c, "gpu-b1", "embed")
	body := `{"node_selector":"gpuworks.ai/pool=b"}`

	// Every agent of the entry is unreachable.
	agents.down[a1.UUID], agents.down[b1.UUID] = true, true
	rec := rig.do(rig.session, http.MethodPut, base, body)
	params := refusal(t, rec.Code, rec.Body.String())
	want := fmt.Sprintf(`{"verified":false,"nodes":[{"node_uuid":%q,"node_name":"gpu-a1","kube_node":"gpu-a1","deployments":["chat"]},{"node_uuid":%q,"node_name":"gpu-b1","kube_node":"gpu-b1","deployments":["embed"]}]}`, a1.UUID, b1.UUID)
	if string(mustCompact(t, params)) != string(mustCompact(t, []byte(want))) {
		t.Fatalf("params = %s\nwant %s", params, want)
	}
	if !strings.Contains(rec.Body.String(), "could be asked which nodes") {
		t.Fatalf("the fallback does not say nothing was verified: %s", rec.Body)
	}
	if agents.calls() != 2 {
		t.Fatalf("the agents were tried %v; every online one is, in turn", agents.asked)
	}

	// One agent down, the next answers.
	agents.down[a1.UUID], agents.down[b1.UUID] = true, false
	rec = rig.do(rig.session, http.MethodPut, base, body)
	params = refusal(t, rec.Code, rec.Body.String())
	if !strings.Contains(string(params), `"verified":true`) || strings.Contains(string(params), "gpu-b1") {
		t.Fatalf("params = %s", params)
	}

	// An entry no agent is online for cannot be asked either.
	if err := rig.st.SetNodeState(a1.UUID, store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	if err := rig.st.SetNodeState(b1.UUID, store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	before := agents.calls()
	rec = rig.do(rig.session, http.MethodPut, base, body)
	if params = refusal(t, rec.Code, rec.Body.String()); !strings.Contains(string(params), `"verified":false`) || agents.calls() != before {
		t.Fatalf("offline entry: params = %s, calls %d -> %d", params, before, agents.calls())
	}

	// Confirming does not need an agent.
	if rec := rig.do(rig.session, http.MethodPut, base, `{"node_selector":"gpuworks.ai/pool=b","confirm_drop_instances":true}`); rec.Code != http.StatusOK {
		t.Fatalf("confirmed = %d %s", rec.Code, rec.Body)
	}
	if agents.calls() != before {
		t.Fatal("an agent was asked although the request confirmed")
	}
}

// The strict decoding of an edit accepts the confirmation and still refuses
// what it does not know; an entry from a cloud cluster still cannot be given
// a node selector.
func TestEditStillRefusesUnknownFieldsBesideTheConfirmation(t *testing.T) {
	rig := newSelectionRig(t, &selectionAgents{})
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody))
	base := "/api/v1/kube/clusters/" + created["uuid"].(string)
	if rec := rig.do(rig.session, http.MethodPut, base, `{"confirm_drop_instances":true}`); rec.Code != http.StatusOK {
		t.Fatalf("confirmation alone = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPut, base, `{"confirm_drop_instances":"yes"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a non-boolean confirmation = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPut, base, `{"confirm_drop":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown field = %d", rec.Code)
	}
}

// Taking node pools out of an entry made from a cloud cluster.
func TestShrinkingTheNodePoolsOfAnEntryAsksBeforeDroppingInstances(t *testing.T) {
	rig := newKubeSourceRig(t)
	const label = "alibabacloud.com/nodepool-id"
	agents := &selectionAgents{labels: map[string]map[string]string{
		"n-1": {label: "np-1"}, "n-2": {label: "np-2"}, "n-ppu": {label: "np-ppu"},
	}, down: map[string]bool{}}
	rig.agents = agents.call
	uuid, _ := rig.createFromPools("ack", "np-1", "np-2")
	path := "/api/v1/kube/clusters/" + uuid + "/pools"
	c := entryOf(t, rig.tokenRig, uuid)
	n1 := seedKubeNode(t, rig.st, c, "n-1", "chat")
	seedKubeNode(t, rig.st, c, "n-2", "embed")

	// Widening: the agent is asked, nobody leaves.
	if rec := rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-1","np-2","np-ppu"]}`); rec.Code != http.StatusOK {
		t.Fatalf("widen = %d %s", rec.Code, rec.Body)
	}
	if agents.calls() != 1 {
		t.Fatalf("widening asked %v", agents.asked)
	}
	// The same pools: no question.
	if rec := rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-ppu","np-2","np-1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("same pools = %d %s", rec.Code, rec.Body)
	}
	if agents.calls() != 1 {
		t.Fatalf("unchanged pools asked %v", agents.asked)
	}

	// Taking np-1 away leaves n-1, which runs chat.
	rec := rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-2","np-ppu"]}`)
	params := refusal(t, rec.Code, rec.Body.String())
	want := fmt.Sprintf(`{"verified":true,"nodes":[{"node_uuid":%q,"node_name":"n-1","kube_node":"n-1","deployments":["chat"]}]}`, n1.UUID)
	if string(mustCompact(t, params)) != string(mustCompact(t, []byte(want))) {
		t.Fatalf("params = %s\nwant %s", params, want)
	}
	if got := agents.last; got.PoolLabel != label || !slices.Equal(got.PoolIDs, []string{"np-2", "np-ppu"}) || len(got.Selector) != 0 {
		t.Fatalf("question = %+v", got)
	}
	if rig.poolsOf(uuid) != `["np-1","np-2","np-ppu"]` {
		t.Fatalf("a refused edit changed the pools: %s", rig.poolsOf(uuid))
	}

	// Confirmed.
	rec = rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-2","np-ppu"],"confirm_drop_instances":true}`)
	if rec.Code != http.StatusOK || rig.poolsOf(uuid) != `["np-2","np-ppu"]` {
		t.Fatalf("confirmed = %d %s; pools %s", rec.Code, rec.Body, rig.poolsOf(uuid))
	}

	// No agent to ask: unverified, every node that runs instances is listed.
	rig.agents = nil
	rec = rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-ppu"]}`)
	params = refusal(t, rec.Code, rec.Body.String())
	if !strings.Contains(string(params), `"verified":false`) || !strings.Contains(string(params), `"kube_node":"n-1"`) || !strings.Contains(string(params), `"kube_node":"n-2"`) {
		t.Fatalf("params = %s", params)
	}
}

// Instances that are finished do not stand in the way.
func TestFinishedInstancesDoNotBlockNarrowingTheSelection(t *testing.T) {
	agents := &selectionAgents{labels: map[string]map[string]string{"gpu-a1": {"gpuworks.ai/pool": "a"}}, down: map[string]bool{}}
	rig := newSelectionRig(t, agents)
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"prod","server_url":"https://gw.example","node_selector":"gpuworks.ai/pool=a"}`))
	uuid := created["uuid"].(string)
	c := entryOf(t, rig, uuid)
	n := seedKubeNode(t, rig.st, c, "gpu-a1")
	d := seedDeployment(t, rig.st, "old")
	for i, state := range proto.InstanceFinishedStates() {
		if _, err := rig.st.CreateInstance(&store.Instance{UUID: fmt.Sprintf("done-%d", i), DeploymentID: d.ID, NodeID: n.ID, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/kube/clusters/"+uuid, `{"node_selector":"gpuworks.ai/pool=b"}`); rec.Code != http.StatusOK {
		t.Fatalf("= %d %s", rec.Code, rec.Body)
	}
	if agents.calls() != 0 {
		t.Fatalf("asked %v", agents.asked)
	}
}

func mustCompact(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	out, err := json.Marshal(v) // object keys sorted: comparable whatever their order
	if err != nil {
		t.Fatal(err)
	}
	return out
}
