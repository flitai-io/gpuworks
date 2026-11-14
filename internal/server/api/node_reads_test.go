package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// /healthz answers how many nodes there are: a count, not every node's full
// row and every card. Pages poll it every few seconds and on every node event.
func TestHealthzCountsNodesWithoutReadingThem(t *testing.T) {
	r := newAutoRig(t, 4)
	reads := countWholeNodeReads(t, r.st)
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var got dto.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("healthz = %d %s (%v)", rec.Code, rec.Body, err)
	}
	if got.Nodes != 4 {
		t.Fatalf("healthz nodes = %d, want 4", got.Nodes)
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("healthz read every node's full row %d times, want 0", n)
	}
}

// Choosing nodes for a replica reads the fleet once per choice
// (planning.LoadUnitSuggestFacts). The callers used to read it once more
// beforehand only to build a fallback online check they then discarded.
func TestPlacementReadsTheFleetOncePerChoice(t *testing.T) {
	r := newAutoRig(t, 4)
	multi := r.mustCreate(t, r.multiBody("scaled", ""))
	reads := countWholeNodeReads(t, r.st)
	step := func(name string, want int64, do func() int) {
		t.Helper()
		reads.Store(0)
		if status := do(); status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s = %d", name, status)
		}
		if n := reads.Load(); n != want {
			t.Errorf("%s read the whole fleet %d times, want %d", name, n, want)
		}
	}
	step("scale 1 -> 2", 1, func() int { rec, _ := r.scale(t, multi.ID, `{"replicas":2}`); return rec.Code })
	// One choice per added replica: each one's ranks claim cards the next sees.
	step("scale 2 -> 4", 2, func() int { rec, _ := r.scale(t, multi.ID, `{"replicas":4}`); return rec.Code })
	var pd dto.Deployment
	step("create auto PD", 1, func() int { pd = r.mustCreate(t, r.pdAutoBody("pd-grow", `{}`, `{}`, "")); return http.StatusCreated })
	step("grow auto PD", 1, func() int {
		status, _, _ := r.updatePD(pd.ID, `{"roles":{"prefill":{"units":[{"node_uuid":"`+pdNodes(t, pd, "prefill")[0]+`"},{}]}}}`)
		return status
	})

	h, st, dep := suggestFixture(t)
	suggest := countWholeNodeReads(t, st)
	if status, body, _ := postSuggest(t, h, dep.ID, listedReplica); status != http.StatusOK {
		t.Fatalf("suggest = %d %s", status, body)
	}
	if n := suggest.Load(); n != 1 {
		t.Errorf("suggest read the whole fleet %d times, want 1", n)
	}
}

// Reads that need only who the nodes are - their identity, state and labels -
// read those columns, not every node's full row and cards. What they answer
// is the same: the same nodes, newest first, a node being restored still
// shown connecting.
func TestIdentityReadsSkipTheWholeFleet(t *testing.T) {
	r := newAutoRig(t, 3)
	// au-1 is offline while the platform restores its agent.
	if err := r.st.SetNodeState("au-1", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetNodeRecoveryPhase("au-1", store.RecoveryPhaseProbing, 1, nil, ""); err != nil {
		t.Fatal(err)
	}
	storage, err := r.st.CreateStorage(&store.Storage{Name: "identity-reads", Type: proto.StorageLocal, Path: "/data/identity", AllNodes: true, Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	image, err := r.st.UpsertImage(&store.Image{ImageRef: "example/identity:1", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var synced, networked []string
	h := &apiHandlers{d: Deps{Store: r.st, NodeSync: nodeSyncCallbacks{
		request: func(uuid string) { mu.Lock(); synced = append(synced, uuid); mu.Unlock() },
		network: func(uuid string) { mu.Lock(); networked = append(networked, uuid); mu.Unlock() },
	}}}
	reads := countWholeNodeReads(t, r.st)
	expectNoRead := func(what string) {
		t.Helper()
		if n := reads.Load(); n != 0 {
			t.Errorf("%s read every node's full row %d times, want 0", what, n)
		}
		reads.Store(0)
	}

	rec := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/storages/%d?all_nodes=1", storage.ID), "")
	var detail dto.Storage
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("storage = %d %s", rec.Code, rec.Body)
	}
	var nodes []string
	for _, n := range detail.Nodes.All {
		nodes = append(nodes, n.NodeUUID)
	}
	if !slices.Equal(nodes, []string{"au-3", "au-2", "au-1"}) {
		t.Fatalf("storage nodes = %v, want every node newest first", nodes)
	}
	expectNoRead("storage detail")
	if rec := r.do(r.session, http.MethodGet, "/api/v1/storages", ""); rec.Code != http.StatusOK {
		t.Fatalf("storages = %d %s", rec.Code, rec.Body)
	}
	expectNoRead("storage list")

	got := h.imageDTO(image, true)
	states := map[string]string{}
	var order []string
	for _, n := range got.Nodes {
		states[n.NodeUUID] = n.NodeState
		order = append(order, n.NodeUUID)
	}
	if !slices.Equal(order, []string{"au-3", "au-2", "au-1"}) || states["au-1"] != store.NodeStateConnecting || states["au-2"] != store.NodeStateOnline {
		t.Fatalf("image nodes = %v states %v, want newest first and au-1 connecting", order, states)
	}
	expectNoRead("image detail")

	h.requestSyncAllNodes()
	h.requestOnlineNetworkSync()
	slices.Sort(synced)
	slices.Sort(networked)
	if want := []string{"au-1", "au-2", "au-3"}; !slices.Equal(synced, want) || !slices.Equal(networked, want) {
		t.Fatalf("synced %v, network-synced %v, want every node", synced, networked)
	}
	expectNoRead("node syncs")

	dep := r.mustCreate(t, r.multiBody("states", ""))
	stored := r.storedDeployment(t, dep.ID)
	for rank, node := range []string{"au-2", "au-3"} {
		n, err := r.st.NodeByUUID(node)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.st.CreateInstance(&store.Instance{UUID: fmt.Sprintf("states-%d", rank), DeploymentID: dep.ID, NodeID: n.ID, State: proto.InstRunning,
			GPUIndexes: "[0]", NNodes: 2, NodeRank: rank, Headless: rank > 0, DistGroup: "g-states", DistAttempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	insts, err := r.st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	reads.Store(0)
	if _, err := deployment.EffectiveStates(r.st, nil, []store.Deployment{*stored}, insts); err != nil {
		t.Fatal(err)
	}
	expectNoRead("deployment states")
}
