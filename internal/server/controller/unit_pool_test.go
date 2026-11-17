package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const poolModelMeta = `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`

// poolFixture is n online nodes of gpus 80 GiB cards each, a small model
// present on all of them, and a hub that sees them online.
func poolFixture(t *testing.T, name string, n, gpus int) (*store.Store, *distributedTestHub, []*store.Node, *store.Model) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/" + name, Revision: "main", Name: name, WeightBytes: 1 << 30, Meta: poolModelMeta})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}}
	var nodes []*store.Node
	for i := 0; i < n; i++ {
		uuid := fmt.Sprintf("%s-node-%d", name, i)
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		status := proto.NodeStatus{Sys: proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"}}
		for g := 0; g < gpus; g++ {
			status.GPUs = append(status.GPUs, proto.GPUInfo{Index: g, UUID: fmt.Sprintf("%s-gpu-%d", uuid, g), Name: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia",
				ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 80 << 30, Healthy: true})
		}
		if err := st.UpdateNodeStatus(uuid, status); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/" + name}); err != nil {
			t.Fatal(err)
		}
		hub.online[uuid] = true
		nodes = append(nodes, node)
	}
	return st, hub, nodes, model
}

func poolDeployment(t *testing.T, st *store.Store, model *store.Model, nodeID uint, replicas int, strategy store.PlacementStrategy, reqs map[string][]string) *store.Deployment {
	t.Helper()
	dep, err := createTestDeploymentWithRequirements(t, st, &store.Deployment{
		Name: model.Name, ModelID: model.ID, NodeID: nodeID, ServedName: model.Name, Engine: "vllm",
		Placement: `{"tp":1}`, QuickParams: `{"max_model_len":512}`, Replicas: replicas, PlacementStrategy: strategy,
	}, reqs)
	if err != nil {
		t.Fatal(err)
	}
	return dep
}

func replicaNodes(t *testing.T, st *store.Store, depID uint) map[uint]int {
	t.Helper()
	instances, err := st.InstancesForDeployment(depID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint]int{}
	for _, inst := range instances {
		if planning.InstanceActive(inst.State) {
			out[inst.NodeID]++
		}
	}
	return out
}

// A deployment that names no node is placed from its pool on every launch:
// spread puts each replica on its own node, binpack fills one node first.
// Neither is held to the node it was created on.
func TestPoolReplicasFollowTheStrategy(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "pool_strategy", 3, 2)
	spread := poolDeployment(t, st, model, nodes[0].ID, 3, store.PlacementSpread, nil)
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), spread.ID); err != nil {
		t.Fatal(err)
	}
	if got := replicaNodes(t, st, spread.ID); len(got) != 3 {
		t.Fatalf("spread placed %v, want one replica on each of three nodes", got)
	}

	st, hub, nodes, model = poolFixture(t, "pool_strategy_binpack", 3, 2)
	c = New(st, hub, nil)
	binpack := poolDeployment(t, st, model, nodes[2].ID, 2, store.PlacementBinpack, nil)
	if err := c.reconcileDeployment(context.Background(), binpack.ID); err != nil {
		t.Fatal(err)
	}
	got := replicaNodes(t, st, binpack.ID)
	if len(got) != 1 {
		t.Fatalf("binpack placed %v, want both replicas on one node", got)
	}
	for _, count := range got {
		if count != 2 {
			t.Fatalf("binpack placed %v", got)
		}
	}
}

// A replica whose node goes away is rebuilt on another node of the pool:
// placement is decided at every launch, which is what makes it a failover.
func TestPoolReplicaIsRebuiltElsewhereWhenItsNodeIsGone(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "pool_failover", 2, 1)
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil)
	c := New(st, hub, nil)
	c.SetRetryPolicy(0, 3)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances = %+v, %v", instances, err)
	}
	first := instances[0]
	var lost string
	for _, node := range nodes {
		if node.ID == first.NodeID {
			lost = node.UUID
		}
	}
	hub.online[lost] = false
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", first.UUID).Update("state", proto.InstFailed).Error; err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	got := replicaNodes(t, st, dep.ID)
	if len(got) != 1 || got[first.NodeID] != 0 {
		t.Fatalf("after node %s went away replicas run on %v, want another node", lost, got)
	}
}

// A pinned deployment's pool is its node: when that node cannot run it, the
// reason names the requirement the node fails, not an anonymous count.
func TestPoolEmptySaysWhyThePinnedNodeIsExcluded(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "pool_pinned", 2, 1)
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, map[string][]string{
		"gpuworks.ai/node": {nodes[0].UUID}, "pool": {"inference"},
	})
	if err := New(st, hub, nil).reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	event, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found || !strings.Contains(event.Data, `"code":"pool_empty"`) ||
		!strings.Contains(event.Data, `"unmet":2`) || !strings.Contains(event.Data, "pool ∈ {inference}") {
		t.Fatalf("event = %+v found=%v err=%v", event, found, err)
	}
}

// The pool's size never decides how much a pass costs. Over a thousand nodes
// and three thousand instances with no room anywhere, one pass plans at most
// poolTrialBudget candidates and says so; the next pass plans the next ones
// instead of the same best few again; and a node with room is found in one
// trial because the bulk ranking puts it first.
func TestPoolPassIsBoundedAtAThousandNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds a thousand nodes")
	}
	const nodeCount, perNode, gpus = 1000, 3, 4
	st, err := store.Open("file:pool_thousand?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/thousand", Revision: "main", Name: "thousand", WeightBytes: 1 << 30, Meta: poolModelMeta})
	if err != nil {
		t.Fatal(err)
	}
	filler, err := st.CreateDeployment(&store.Deployment{Name: "filler", ModelID: model.ID, Engine: "vllm", Replicas: nodeCount * perNode})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}}
	db := st.DB()
	nodes := make([]store.Node, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		uuid := fmt.Sprintf("thousand-%04d", i)
		nodes = append(nodes, store.Node{UUID: uuid, Name: uuid, Hostname: uuid, State: store.NodeStateOnline, Arch: "amd64", GPUVendor: "nvidia", GPUCount: gpus})
		hub.online[uuid] = true
	}
	if err := db.CreateInBatches(&nodes, 200).Error; err != nil {
		t.Fatal(err)
	}
	var cards []store.GPU
	var instances []store.Instance
	for i := range nodes {
		for g := 0; g < gpus; g++ {
			cards = append(cards, store.GPU{NodeID: nodes[i].ID, Index: g, UUID: fmt.Sprintf("%s-%d", nodes[i].UUID, g), Name: "A100-SXM4-80GB",
				Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 80 << 30})
		}
		// Every node but the last keeps a single free card; the replica
		// needs two.
		holders := perNode
		if i == nodeCount-1 {
			holders = 0
		}
		for k := 0; k < holders; k++ {
			instances = append(instances, store.Instance{UUID: fmt.Sprintf("filler-%d-%d", i, k), DeploymentID: filler.ID, NodeID: nodes[i].ID,
				State: proto.InstRunning, GPUIndexes: fmt.Sprintf("[%d]", k)})
		}
	}
	if err := db.CreateInBatches(&cards, 500).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.CreateInBatches(&instances, 500).Error; err != nil {
		t.Fatal(err)
	}
	if len(instances) != (nodeCount-1)*perNode {
		t.Fatalf("seeded %d instances", len(instances))
	}

	c := New(st, hub, nil)
	pass := func(dep *store.Deployment, reqs map[string][]string) (*store.Node, bool, time.Duration) {
		start := time.Now()
		node, ok := c.pickPoolNode(context.Background(), &unitDeployment{dep: dep, requirements: reqs}, model)
		return node, ok, time.Since(start)
	}

	// Two GPUs per replica; only the last node has them free. Excluding it
	// leaves 999 candidates and no room anywhere.
	last := nodes[nodeCount-1]
	full, err := createTestDeploymentWithRequirements(t, st, &store.Deployment{Name: "full", ModelID: model.ID, Engine: "vllm", Replicas: 1,
		Placement: `{"tp":2}`, QuickParams: `{"max_model_len":512}`, PlacementStrategy: store.PlacementSpread}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reqs := map[string][]string{"gpuworks.ai/vendor": {"nvidia"}}
	// A requirement on a written label nobody but the 999 carry.
	for i := range nodes[:nodeCount-1] {
		if _, err := st.SetNodeLabels(nodes[i].UUID, map[string]string{"rack": "busy"}); err != nil {
			t.Fatal(err)
		}
	}
	reqs["rack"] = []string{"busy"}
	for round := 1; round <= 2; round++ {
		if node, ok, took := pass(full, reqs); ok {
			t.Fatalf("round %d placed on %s with no room anywhere", round, node.UUID)
		} else {
			t.Logf("round %d: no fit over %d candidates in %v", round, nodeCount-1, took)
		}
		event, found, err := st.LatestScheduleEvent(full.ID)
		if err != nil || !found {
			t.Fatalf("round %d recorded nothing: %v", round, err)
		}
		if round == 1 && (!strings.Contains(event.Data, `"code":"pool_no_fit"`) || !strings.Contains(event.Data, fmt.Sprintf(`"candidates":%d`, nodeCount-1)) ||
			!strings.Contains(event.Data, fmt.Sprintf(`"tried":%d`, poolTrialBudget)) ||
			// Refused for room, not for anything about the fixture.
			strings.Contains(event.Data, "placement_rejected")) {
			t.Fatalf("round %d event = %s", round, event.Data)
		}
	}
	// The two rounds planned different candidates: every one refused so far
	// is remembered, and there are twice the budget of them.
	refused := c.pool.refusals(full.ID)
	if refused != 2*poolTrialBudget {
		t.Fatalf("refused candidates remembered = %d, want %d", refused, 2*poolTrialBudget)
	}

	// The same shape without the rack requirement finds the free node first.
	open, err := createTestDeploymentWithRequirements(t, st, &store.Deployment{Name: "open", ModelID: model.ID, Engine: "vllm", Replicas: 1,
		Placement: `{"tp":2}`, QuickParams: `{"max_model_len":512}`, PlacementStrategy: store.PlacementSpread}, nil)
	if err != nil {
		t.Fatal(err)
	}
	node, ok, took := pass(open, map[string][]string{"gpuworks.ai/vendor": {"nvidia"}})
	if !ok || node.UUID != last.UUID {
		event, _, _ := st.LatestScheduleEvent(open.ID)
		t.Fatalf("picked %v ok=%v, want %s; event %s", node, ok, last.UUID, event.Data)
	}
	t.Logf("found the one free node among %d in %v", nodeCount, took)
	if _, found, _ := st.LatestScheduleEvent(open.ID); found {
		t.Fatal("a pass that placed the replica recorded a refusal")
	}
}

// Edited requirements take effect at the next launch: a replica refused on
// the node it was pinned to is placed on the node the edit names, and what
// the controller remembered about the old pool is dropped with it. Other
// changes to the deployment keep that memory.
func TestPoolFollowsEditedRequirements(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "pool_edit", 2, 1)
	occupant, err := st.CreateDeployment(&store.Deployment{Name: "occupant", ModelID: model.ID, NodeID: nodes[0].ID, Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Instance{UUID: "occupant-1", DeploymentID: occupant.ID, NodeID: nodes[0].ID, State: proto.InstRunning, GPUIndexes: "[0]"}).Error; err != nil {
		t.Fatal(err)
	}
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, map[string][]string{
		"gpuworks.ai/node": {nodes[0].UUID}, "gpuworks.ai/vendor": {"nvidia"},
	})
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if got := replicaNodes(t, st, dep.ID); len(got) != 0 {
		t.Fatalf("placed %v on a full node", got)
	}
	remembered := func() int { return c.pool.refusals(dep.ID) }
	if remembered() != 1 {
		t.Fatalf("refusals remembered = %d, want the pinned node", remembered())
	}
	c.onEvent(bus.Event{Topic: "deployment", Key: fmt.Sprint(dep.ID), Reason: "routing"})
	if remembered() != 1 {
		t.Fatal("a routing change dropped the pool's memory")
	}

	if err := st.SetDeploymentRequirements(dep.ID, map[string][]string{"gpuworks.ai/node": {nodes[1].UUID}, "gpuworks.ai/vendor": {"nvidia"}}, store.PlacementSpread); err != nil {
		t.Fatal(err)
	}
	c.onEvent(bus.Event{Topic: "deployment", Key: fmt.Sprint(dep.ID), Reason: store.DeploymentChangeRequirements})
	if remembered() != 0 {
		t.Fatalf("refusals under the old requirements still remembered: %d", remembered())
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if got := replicaNodes(t, st, dep.ID); got[nodes[1].ID] != 1 || len(got) != 1 {
		t.Fatalf("after the edit replicas run on %v, want node %d", got, nodes[1].ID)
	}
}

// A deployment whose last pass found no node sits out routine status
// batches for poolSettleBackoff, so a cluster of waiting deployments does not
// keep the controller's one worker repeating refusals; after it, or once its
// requirements change, the next batch tries it again.
func TestWaitingPoolSitsOutStatusBatchesForABackoff(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "pool_backoff", 1, 1)
	occupant, err := st.CreateDeployment(&store.Deployment{Name: "occupant", ModelID: model.ID, NodeID: nodes[0].ID, Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Instance{UUID: "occupant-1", DeploymentID: occupant.ID, NodeID: nodes[0].ID, State: proto.InstRunning, GPUIndexes: "[0]"}).Error; err != nil {
		t.Fatal(err)
	}
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil)
	now := time.Now()
	c := New(st, hub, nil)
	c.now = func() time.Time { return now }
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	queued := func() bool {
		drainQueue(c)
		c.onEvent(bus.Event{Topic: "node", Key: nodes[0].UUID, Reason: store.NodeChangeStatus})
		c.settleNodeStatus()
		for _, key := range pendingKeys(c) {
			if key == depKey(dep.ID) {
				return true
			}
		}
		return false
	}
	if queued() {
		t.Fatal("a deployment refused a moment ago was retried by a status batch")
	}
	now = now.Add(poolSettleBackoff)
	if !queued() {
		t.Fatal("after the backoff a status batch did not retry the waiting deployment")
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if queued() {
		t.Fatal("the backoff did not restart after another refusal")
	}
	c.onEvent(bus.Event{Topic: "deployment", Key: fmt.Sprint(dep.ID), Reason: store.DeploymentChangeRequirements})
	if !queued() {
		t.Fatal("edited requirements did not end the backoff")
	}
	// Room appears: the replica is placed and nothing is remembered as waiting.
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", "occupant-1").Update("state", proto.InstStopped).Error; err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if got := replicaNodes(t, st, dep.ID); got[nodes[0].ID] != 1 {
		t.Fatalf("replicas run on %v after the card was freed", got)
	}
	if c.pool.waitingRecently(dep.ID, c.now()) {
		t.Fatal("a placed deployment is still remembered as waiting")
	}
}

// A node where the model is stuck - its acquisition failed, or waits on an
// operator - would take a pool replica and hold it there with nothing to say
// why. The pool passes it over and its refusal names the model and the
// error, so the deployment's status explains the replica that cannot start.
func TestPoolPassesOverANodeWhereTheModelIsStuck(t *testing.T) {
	for _, state := range []string{proto.MFFailed, proto.MFDownloadRequired} {
		t.Run(state, func(t *testing.T) {
			st, hub, nodes, model := poolFixture(t, "pool_model_stuck_"+state, 2, 1)
			if err := st.DB().Model(&store.ModelFile{}).Where("model_id = ? AND node_id = ?", model.ID, nodes[0].ID).
				Updates(map[string]any{"state": state, "acquisition": proto.ModelAcquirePeer, "transfer_source_node_id": nodes[1].ID,
					"message": "send peer model archive: dial model sync target: "}).Error; err != nil {
				t.Fatal(err)
			}
			dep := poolDeployment(t, st, model, nodes[0].ID, 2, store.PlacementSpread, nil)
			c := New(st, hub, nil)
			if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatal(err)
			}
			if got := replicaNodes(t, st, dep.ID); len(got) != 1 || got[nodes[1].ID] != 1 {
				t.Fatalf("replicas run on %v, want one on %s and none on the stuck node", got, nodes[1].UUID)
			}
			event, found, err := st.LatestScheduleEvent(dep.ID)
			if err != nil || !found {
				t.Fatalf("no schedule event: found=%v err=%v", found, err)
			}
			for _, want := range []string{`"code":"pool_no_fit"`, `"first":"model_unavailable"`, `"node":"` + nodes[0].DisplayName() + `"`,
				`"model":"` + model.Name + `"`, `"state":"` + state + `"`, "dial model sync target"} {
				if !strings.Contains(event.Data, want) {
					t.Fatalf("event %s lacks %s", event.Data, want)
				}
			}
		})
	}
}

// A model still arriving is not stuck: the replica waits on the node it was
// given rather than being moved, and no refusal is recorded for it.
func TestPoolKeepsANodeWhoseModelIsStillArriving(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "pool_model_arriving", 1, 1)
	if err := st.DB().Model(&store.ModelFile{}).Where("model_id = ? AND node_id = ?", model.ID, nodes[0].ID).
		Updates(map[string]any{"state": proto.MFDownloading, "acquisition": proto.ModelAcquireDownload}).Error; err != nil {
		t.Fatal(err)
	}
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil)
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if got := replicaNodes(t, st, dep.ID); len(got) != 0 {
		t.Fatalf("replicas run on %v before the model arrived", got)
	}
	if event, found, _ := st.LatestScheduleEvent(dep.ID); found {
		t.Fatalf("a model still downloading recorded a refusal: %s", event.Data)
	}
}

// A pinned deployment's pool reads only its node, yet still says how the
// whole fleet stands: every other node fails the node requirement.
func TestPinnedPoolCountsTheNodesItDoesNotRead(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "pool_pinned_counts", 3, 1)
	hub.online[nodes[2].UUID] = false
	dep := poolDeployment(t, st, model, nodes[2].ID, 1, store.PlacementSpread, map[string][]string{
		"gpuworks.ai/node": {nodes[2].UUID},
	})
	if err := New(st, hub, nil).reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	event, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found {
		t.Fatalf("event found=%v err=%v", found, err)
	}
	for _, want := range []string{`"code":"pool_empty"`, `"nodes":3`, `"unmet":2`, `"offline":1`} {
		if !strings.Contains(event.Data, want) {
			t.Fatalf("event %s lacks %s", event.Data, want)
		}
	}
}
