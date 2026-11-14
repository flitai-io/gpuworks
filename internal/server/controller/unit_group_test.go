package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type distributedTestHub struct {
	online  map[string]bool
	updates map[string][]proto.StateUpdate
	pushErr error
	// pushFails fails pushes to these nodes only.
	pushFails map[string]error
	// ucxHang is what each node's agent reports about its kernel hanging UCX.
	ucxHang map[string]*proto.UCXNetlinkHang
}

// groupFixture is a two-node multi-node deployment and one generation of it
// in the given rank states, launched for its unit as the controller would.
func groupFixture(t *testing.T, name string, dep store.Deployment, states [2]string) (*store.Store, *store.Deployment, []*store.Node, []store.Instance) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/" + name, Name: name, WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*store.Node
	for _, suffix := range []string{"a", "b"} {
		node, err := st.RegisterNode(name+"-"+suffix, store.HashToken(name+suffix), name+"-"+suffix)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	nodeIDs, _ := json.Marshal([]uint{nodes[0].ID, nodes[1].ID})
	dep.ModelID, dep.NodeID, dep.NodeIDs, dep.State, dep.NNodes, dep.DistGroup = model.ID, nodes[0].ID, string(nodeIDs), store.DeployActive, 2, "g-"+name
	created, err := createTestDeployment(t, st, &dep)
	if err != nil {
		t.Fatal(err)
	}
	var rows []store.Instance
	for rank, state := range states {
		uuid := fmt.Sprintf("%s-rank-%d", name, rank)
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", VRAMLimitBytes: dep.VRAMLimitBytes})
		row := &store.Instance{UUID: uuid, DeploymentID: created.ID, NodeID: nodes[rank].ID, State: state, DesiredStateJSON: string(spec),
			NNodes: 2, NodeRank: rank, Headless: rank > 0, DistGroup: created.DistGroup, DistAttempt: 1}
		if rank == 0 {
			row.ScheduleExpl = planning.NewScheduleRecord(planning.LaunchPlan{}, false, nil, &planning.UnitRecord{Followers: nodes[1].UUID + "[]"}).Encode()
		}
		if _, err := st.CreateInstance(row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, *row)
	}
	return st, created, nodes, rows
}

// A rank that stopped cannot rejoin a collective that is still running, so
// its generation fails: the leader becomes the spent attempt and its
// container is removed.
func TestMultiNodeStoppedRankFailsItsGeneration(t *testing.T) {
	st, dep, nodes, rows := groupFixture(t, "stopped_rank", store.Deployment{VRAMLimitBytes: 4 << 30}, [2]string{proto.InstRunning, proto.InstStopped})
	hub := &distributedTestHub{online: map[string]bool{nodes[0].UUID: true, nodes[1].UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.SetRetryPolicy(0, 3)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	leader, _ := st.InstanceByUUID(rows[0].UUID)
	if leader.State != proto.InstFailed || !strings.Contains(leader.Message, "rank 1") {
		t.Fatalf("leader = %s %q", leader.State, leader.Message)
	}
	if !removedByHub(hub, nodes[0].UUID, rows[0].UUID) {
		t.Fatal("the running leader's container was not removed")
	}
	if _, err := st.InstanceByUUID(rows[1].UUID); err == nil {
		t.Fatal("the stopped rank's row was kept")
	}
}

// A generation is rebuilt only once every rank of the failed one has
// confirmed its teardown: under dGPU a rank still being removed holds its
// GPU share.
func TestMultiNodeRebuildWaitsForEveryTeardownAck(t *testing.T) {
	st, dep, nodes, rows := groupFixture(t, "teardown_ack", store.Deployment{VRAMLimitBytes: 4 << 30}, [2]string{proto.InstRunning, proto.InstFailed})
	hub := &distributedTestHub{online: map[string]bool{nodes[0].UUID: true, nodes[1].UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.SetRetryPolicy(0, 3)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	follower, _ := st.InstanceByUUID(rows[1].UUID)
	if follower.State != proto.InstRemoving {
		t.Fatalf("failed follower = %s", follower.State)
	}
	for pass := 0; pass < 2; pass++ {
		if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.InstancesForDeployment(dep.ID); len(got) != 2 {
			t.Fatalf("pass %d changed the rows before the teardown ack: %+v", pass, got)
		}
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: rows[1].UUID, State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := st.InstancesForDeployment(dep.ID)
	if len(got) != 1 || got[0].UUID != rows[0].UUID || got[0].State != proto.InstFailed {
		t.Fatalf("after the ack = %+v, want only the failed leader (the nodes have no GPUs to relaunch on)", got)
	}
}

func TestDistributedLabelsRejectAllRanksBeforeFirstInstance(t *testing.T) {
	st, err := store.Open("file:distributed_labels_preflight?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/labels", Name: "labels", WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*store.Node
	for rank, uuid := range []string{"label-rank-0", "label-rank-1"} {
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		node.NetworkInventory = managementInventoryJSON(t, fmt.Sprintf("10.2.0.%d", rank+1))
		if err := st.DB().Save(node).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetNodeLabels(uuid, map[string]string{"pool": "inference"}); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: uuid + "-gpu", Name: "GPU", Vendor: "nvidia", MemoryTotal: 24 << 30}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/labels"}); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	ids, _ := json.Marshal([]uint{nodes[0].ID, nodes[1].ID})
	dep, err := createTestDeploymentWithRequirements(t, st, &store.Deployment{ModelID: model.ID, NodeID: nodes[0].ID, NodeIDs: string(ids), ServedName: "labels", Engine: "vllm", State: "active", NNodes: 2, MasterAddr: managementIP(*nodes[0]), MasterPort: 29500, Placement: `{"tp":2}`}, map[string][]string{"pool": {"inference"}})
	if err != nil {
		t.Fatal(err)
	}
	// The follower changes after submission but before the first reconcile.
	if _, err := st.SetNodeLabels(nodes[1].UUID, map[string]string{"pool": "batch"}); err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{nodes[0].UUID: true, nodes[1].UUID: true}, updates: map[string][]proto.StateUpdate{}}
	if err := New(st, hub, nil).reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if event, found, _ := st.LatestScheduleEvent(dep.ID); !found || !strings.Contains(event.Data, "pool ∈ {inference}") {
		t.Fatalf("the rejected follower label was not recorded: %+v", event)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 0 {
		t.Fatalf("a partial collective was scheduled: instances=%+v err=%v", instances, err)
	}
}

func TestMultiNodeOfflineRankGetsGraceThenFailsItsGeneration(t *testing.T) {
	st, err := store.Open("file:controller_distributed_offline_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model", WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*store.Node
	for rank, uuid := range []string{"offline-grace-a", "offline-grace-b"} {
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		node.NetworkInventory = managementInventoryJSON(t, fmt.Sprintf("10.0.1.%d", rank+1))
		if err := st.DB().Save(node).Error; err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: uuid + "-gpu", Name: "GPU", Vendor: "nvidia", MemoryTotal: 24 << 30}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/model"}); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	nodeIDs, _ := json.Marshal([]uint{nodes[0].ID, nodes[1].ID})
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "offline-grace", ModelID: model.ID, NodeID: nodes[0].ID, NodeIDs: string(nodeIDs),
		ServedName: "offline-grace", Engine: "vllm", State: "active", NNodes: 2,
		DistGroup: "offline-grace-group", MasterAddr: managementIP(*nodes[0]), MasterPort: 29500, Placement: `{"tp":2}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{nodes[0].UUID: true, nodes[1].UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.SetRetryPolicy(0, 3)
	c.SetOfflineGrace(time.Minute)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	initial, _ := st.InstancesForDeployment(dep.ID)
	if len(initial) != 2 {
		t.Fatalf("initial distributed group=%+v", initial)
	}

	// A fresh disconnect remains inside the reconnect grace and must not tear
	// down the healthy sibling.
	hub.online[nodes[1].UUID] = false
	if err := st.SetNodeState(nodes[1].UUID, store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	insideGrace, _ := st.InstancesForDeployment(dep.ID)
	if len(insideGrace) != 2 {
		t.Fatalf("fresh disconnect reset group: %+v", insideGrace)
	}

	// Once the last-seen timestamp is outside the grace, the generation
	// fails. The leader's container goes first; while that push fails the
	// generation stays as it is and the next pass tries again.
	old := time.Now().Add(-2 * time.Minute)
	if err := st.DB().Model(&store.Node{}).Where("id = ?", nodes[1].ID).Update("last_seen_at", old).Error; err != nil {
		t.Fatal(err)
	}
	hub.pushErr = errors.New("temporary online sibling push failure")
	if err := c.reconcileDeployment(context.Background(), dep.ID); err == nil {
		t.Fatal("generation failure unexpectedly succeeded while the leader push failed")
	}
	for _, inst := range initial {
		if row, _ := st.InstanceByUUID(inst.UUID); row.State == proto.InstFailed || row.State == proto.InstRemoving {
			t.Fatalf("a failed push changed %s to %s", inst.UUID, row.State)
		}
	}

	// With the push through, the leader is the spent attempt and its
	// container is removed; the offline rank gets no push. Its row waits,
	// removing, for its node without holding anything up: an agent that
	// restarted meanwhile no longer knows the container, so the removal is
	// pushed when the node is back.
	hub.pushErr = nil
	for pass := 0; pass < 2; pass++ {
		if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := st.InstancesForDeployment(dep.ID)
	var leader, offline *store.Instance
	for i := range rows {
		switch rows[i].NodeID {
		case nodes[0].ID:
			leader = &rows[i]
		case nodes[1].ID:
			offline = &rows[i]
		}
	}
	if len(rows) != 2 || leader == nil || leader.State != proto.InstFailed || !strings.Contains(leader.Message, "offline") ||
		offline == nil || offline.State != proto.InstRemoving {
		t.Fatalf("after grace = %+v", rows)
	}
	for _, inst := range initial {
		if inst.NodeID == nodes[0].ID && !removedByHub(hub, nodes[0].UUID, inst.UUID) {
			t.Fatalf("online leader %s was not removed", inst.UUID)
		}
		if inst.NodeID == nodes[1].ID && removedByHub(hub, nodes[1].UUID, inst.UUID) {
			t.Fatalf("offline rank %s unexpectedly received a push", inst.UUID)
		}
	}

	// The node comes back: the removal is pushed to it, and its confirmation
	// ends the row.
	hub.online[nodes[1].UUID] = true
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if !removedByHub(hub, nodes[1].UUID, offline.UUID) {
		t.Fatalf("the returning node was not asked to remove %s", offline.UUID)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: offline.UUID, State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InstanceByUUID(offline.UUID); err == nil {
		t.Fatalf("the confirmed removal %s kept its row", offline.UUID)
	}
}

func (h *distributedTestHub) Online(uuid string) bool { return h.online[uuid] }

// UCXNetlinkHang is the reported fact of a connected node; the real hub keeps
// it on the session, so an offline node has none.
func (h *distributedTestHub) UCXNetlinkHang(uuid string) *proto.UCXNetlinkHang {
	if !h.online[uuid] {
		return nil
	}
	return h.ucxHang[uuid]
}

// RequestImageRefs records the refs as the push the hub would send.
func (h *distributedTestHub) RequestImageRefs(uuid string, refs []string) {
	_ = h.PushToNode(uuid, proto.StateUpdate{ImageRefs: refs})
}

func (h *distributedTestHub) PushToNode(uuid string, update proto.StateUpdate) error {
	if h.pushErr != nil {
		return h.pushErr
	}
	if err := h.pushFails[uuid]; err != nil {
		return err
	}
	h.updates[uuid] = append(h.updates[uuid], update)
	return nil
}
func (h *distributedTestHub) SyncNode(string) error { return nil }
func (h *distributedTestHub) CopyModelPath(context.Context, proto.ModelSyncCopyRequest) (proto.ModelSyncCopyResult, error) {
	return proto.ModelSyncCopyResult{}, nil
}
func (h *distributedTestHub) StopModelSync(context.Context, string, string, string) error { return nil }

func TestDistributedReconcileSchedulesResetsAndRemovesEveryRank(t *testing.T) {
	st, err := store.Open("file:controller_distributed_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "Qwen/Qwen3-8B", Revision: "main",
		Name: "Qwen3", Arch: "qwen3", WeightBytes: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := make([]*store.Node, 0, 2)
	for rank, uuid := range []string{"node-a", "node-b"} {
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		node.NetworkInventory = managementInventoryJSON(t, fmt.Sprintf("10.0.0.%d", rank+10))
		if err := st.DB().Save(node).Error; err != nil {
			t.Fatal(err)
		}
		for gpuIndex := 0; gpuIndex < 2; gpuIndex++ {
			if err := st.DB().Create(&store.GPU{
				NodeID: node.ID, Index: gpuIndex, UUID: fmt.Sprintf("%s-gpu-%d", uuid, gpuIndex), Name: "NVIDIA H100",
				Vendor: "nvidia", ArchID: "sm_90",
				ComputeCapability: "9.0", MemoryTotal: 80 << 30,
			}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{
			ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/qwen3",
		}); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	nodeIDs, _ := json.Marshal([]uint{nodes[0].ID, nodes[1].ID})
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "qwen-dist", ModelID: model.ID, NodeID: nodes[0].ID, NodeIDs: string(nodeIDs),
		ServedName: "qwen-dist", Engine: "vllm", State: "active", NNodes: 2,
		DistGroup: "group-a", MasterAddr: "10.0.0.10", MasterPort: 29500,
		Placement: `{"tp":2,"pp":2}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{
		online:  map[string]bool{"node-a": true, "node-b": true},
		updates: map[string][]proto.StateUpdate{},
	}
	c := New(st, hub, nil)
	c.SetRetryPolicy(0, 3)

	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	insts, _ := st.InstancesForDeployment(dep.ID)
	if len(insts) != 2 {
		t.Fatalf("instances = %d, want two ranks", len(insts))
	}
	byRank := map[int]store.Instance{}
	specByRank := map[int]proto.InstanceDesiredState{}
	for _, inst := range insts {
		byRank[inst.NodeRank] = inst
		var spec proto.InstanceDesiredState
		if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &spec); err != nil {
			t.Fatal(err)
		}
		// Each generation is its own group, named after the deployment's.
		if spec.NodeRank != inst.NodeRank || spec.DistAttempt != 1 || !strings.HasPrefix(spec.DistGroup, "group-a-") || spec.DistGroup != insts[0].DistGroup {
			t.Fatalf("rank metadata mismatch: instance=%+v spec=%+v", inst, spec)
		}
		specByRank[inst.NodeRank] = spec
	}
	leader, follower := byRank[0], byRank[1]
	if leader.Headless || leader.APIPort <= 0 || leader.DistRole != "leader" {
		t.Fatalf("leader = %+v", leader)
	}
	if !follower.Headless || follower.APIPort != 0 || follower.Ports != "[]" || follower.DistRole != "follower" {
		t.Fatalf("follower = %+v", follower)
	}
	leaderSpec, followerSpec := specByRank[0], specByRank[1]
	leaderCmd, followerCmd := strings.Join(leaderSpec.Argv, " "), strings.Join(followerSpec.Argv, " ")
	for rank, cmd := range map[int]string{0: leaderCmd, 1: followerCmd} {
		if !strings.Contains(cmd, "--tensor-parallel-size 2") || !strings.Contains(cmd, "--pipeline-parallel-size 2") || !strings.Contains(cmd, "--master-addr 10.0.0.10") {
			t.Fatalf("vllm rank %d command = %q", rank, cmd)
		}
	}
	if strings.Contains(leaderCmd, "--headless") || !strings.Contains(followerCmd, "--headless") {
		t.Fatalf("vllm headless commands = %q / %q", leaderCmd, followerCmd)
	}
	if leaderSpec.Env["VLLM_HOST_IP"] != "10.0.0.10" || followerSpec.Env["VLLM_HOST_IP"] != "10.0.0.11" {
		t.Fatalf("vllm rank addresses = %+v / %+v", leaderSpec.Env, followerSpec.Env)
	}
	// Each rank records the network it started on, here the management
	// network since the group shares no dedicated one.
	for rank, inst := range byRank {
		var expl struct {
			Network struct {
				Compute map[string]any `json:"compute"`
			} `json:"network"`
		}
		if err := json.Unmarshal([]byte(inst.ScheduleExpl), &expl); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"name": "management", "management": true, "type": "host_ethernet", "interface": "eth0", "ip": fmt.Sprintf("10.0.0.%d", rank+10)}
		if !reflect.DeepEqual(expl.Network.Compute, want) {
			t.Fatalf("rank %d compute network = %v, want %v", rank, expl.Network.Compute, want)
		}
	}

	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: leader.UUID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: follower.UUID, State: proto.InstFailed, Message: "collective failed"}); err != nil {
		t.Fatal(err)
	}
	// A failed rank fails its generation: the leader becomes the unit's
	// spent attempt (kept as evidence), and both containers are removed.
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	failedLeader, _ := st.InstanceByUUID(leader.UUID)
	if failedLeader.State != proto.InstFailed || !strings.Contains(failedLeader.Message, "collective failed") {
		t.Fatalf("leader after rank failure = %s %q", failedLeader.State, failedLeader.Message)
	}
	retired, _ := st.InstanceByUUID(follower.UUID)
	if retired.State != proto.InstRemoving {
		t.Fatalf("follower after rank failure = %s", retired.State)
	}
	if !removedByHub(hub, "node-a", leader.UUID) || !removedByHub(hub, "node-b", follower.UUID) {
		t.Fatalf("generation failure did not target both nodes: %+v", hub.updates)
	}

	// The next generation waits for the follower's teardown, then starts
	// whole, as attempt 2.
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if insts, _ = st.InstancesForDeployment(dep.ID); len(insts) != 2 {
		t.Fatalf("a generation started before the previous one was torn down: %+v", insts)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: follower.UUID, State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	insts, _ = st.InstancesForDeployment(dep.ID)
	var replacement []store.Instance
	for _, inst := range insts {
		if inst.State != proto.InstFailed {
			replacement = append(replacement, inst)
		}
	}
	if len(insts) != 3 || len(replacement) != 2 || replacement[0].DistAttempt != 2 || replacement[1].DistAttempt != 2 {
		t.Fatalf("replacement generation = %+v", insts)
	}

	if err := st.SetDeploymentState(dep.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	remaining, _ := st.InstancesForDeployment(dep.ID)
	if len(remaining) != 3 {
		t.Fatalf("stop released ranks before teardown acknowledgement: %+v", remaining)
	}
	for _, inst := range remaining {
		if inst.State != proto.InstRemoving {
			t.Fatalf("stopping rank %s state = %s, want removing", inst.UUID, inst.State)
		}
		if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstStopped}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	remaining, _ = st.InstancesForDeployment(dep.ID)
	if len(remaining) != 0 {
		t.Fatalf("acknowledged ranks remained after stop: %+v", remaining)
	}
	for _, inst := range insts {
		nodeUUID := "node-a"
		if inst.NodeID == nodes[1].ID {
			nodeUUID = "node-b"
		}
		if !removedByHub(hub, nodeUUID, inst.UUID) {
			t.Fatalf("stop did not remove %s from %s", inst.UUID, nodeUUID)
		}
	}
	deleteInst, err := st.CreateInstance(&store.Instance{
		UUID: "delete-running-instance", DeploymentID: dep.ID, NodeID: nodes[0].ID, State: proto.InstRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentState(dep.ID, "deleting"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	// An accepted push is not proof the container is gone: teardown is
	// asynchronous on the agent, and on udocker the only code that can delete
	// the rootfs is Remove()'s fallback. Dropping the deployment row first
	// discards the last handle on a container that may still be running with
	// its GPU allocated, so the deployment must survive until the agent
	// reports the teardown.
	if !removedByHub(hub, "node-a", deleteInst.UUID) {
		t.Fatalf("delete did not remove %s from node-a", deleteInst.UUID)
	}
	if _, err := st.GetDeployment(dep.ID); err != nil {
		t.Fatalf("deployment removed before the agent confirmed teardown: %v", err)
	}

	// The agent reporting the teardown is what completes the delete.
	if err := st.SetInstanceState(deleteInst.UUID, proto.InstStopped, "torn down"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDeployment(dep.ID); err != store.ErrNotFound {
		t.Fatalf("deleted deployment lookup error = %v, want ErrNotFound", err)
	}

	sglang, err := createTestDeployment(t, st, &store.Deployment{
		Name: "qwen-sglang-dist", ModelID: model.ID, NodeID: nodes[0].ID, NodeIDs: string(nodeIDs),
		ServedName: "qwen-sglang-dist", Engine: "sglang", State: "active", NNodes: 2,
		DistGroup: "group-sglang", MasterAddr: "10.0.0.10", MasterPort: 29501,
		Placement: `{"tp":4}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), sglang.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), sglang.ID); err != nil {
		t.Fatal(err)
	}
	sglangInstances, err := st.InstancesForDeployment(sglang.ID)
	if err != nil || len(sglangInstances) != 2 {
		t.Fatalf("sglang instances = %+v, err=%v", sglangInstances, err)
	}
	for _, inst := range sglangInstances {
		var spec proto.InstanceDesiredState
		if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &spec); err != nil {
			t.Fatal(err)
		}
		command := strings.Join(spec.Entrypoint, " ")
		if !strings.Contains(command, "--tp-size 4") || !strings.Contains(command, "--dist-init-addr 10.0.0.10:29501") {
			t.Fatalf("sglang rank %d command = %q", spec.NodeRank, command)
		}
		if spec.NodeRank == 0 && (spec.Headless || spec.APIPort <= 0 || strings.Contains(command, "--port 0")) {
			t.Fatalf("sglang leader spec = %+v command=%q", spec, command)
		}
		if spec.NodeRank == 1 && (!spec.Headless || spec.APIPort != 0 || !strings.Contains(command, "--port 0")) {
			t.Fatalf("sglang follower spec = %+v command=%q", spec, command)
		}
	}
	if err := st.SetDeploymentState(sglang.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), sglang.ID); err != nil {
		t.Fatal(err)
	}
	// Stopping an active deployment now retains its GPU claims until every
	// agent acknowledges teardown. Complete that lifecycle before scheduling
	// another deployment on the same test GPUs.
	for _, inst := range sglangInstances {
		if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstStopped}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.reconcileDeployment(context.Background(), sglang.ID); err != nil {
		t.Fatal(err)
	}

	// The last allowed attempt leaves every failed leader behind as the
	// evidence; without it the API would derive a pending deployment from an
	// active one with no instances, and clients would wait forever.
	terminal, err := createTestDeployment(t, st, &store.Deployment{
		Name: "terminal-failure", ModelID: model.ID, NodeID: nodes[0].ID, NodeIDs: string(nodeIDs),
		ServedName: "terminal-failure", Engine: "vllm", State: "active", NNodes: 2,
		DistGroup: "group-terminal", MasterAddr: "10.0.0.10", MasterPort: 29502,
		Placement: `{"tp":2,"pp":2}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if err := c.reconcileDeployment(context.Background(), terminal.ID); err != nil {
			t.Fatal(err)
		}
		rows, _ := st.InstancesForDeployment(terminal.ID)
		var failing store.Instance
		for _, row := range rows {
			if row.Headless && row.State != proto.InstFailed {
				failing = row
			}
		}
		if failing.UUID == "" || failing.DistAttempt != attempt {
			t.Fatalf("attempt %d generation = %+v", attempt, rows)
		}
		if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: failing.UUID, State: proto.InstFailed, Message: fmt.Sprintf("collective failure %d", attempt)}); err != nil {
			t.Fatal(err)
		}
		if err := c.reconcileDeployment(context.Background(), terminal.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: failing.UUID, State: proto.InstStopped}); err != nil {
			t.Fatal(err)
		}
	}
	for pass := 0; pass < 2; pass++ {
		if err := c.reconcileDeployment(context.Background(), terminal.ID); err != nil {
			t.Fatal(err)
		}
	}
	terminalRows, _ := st.InstancesForDeployment(terminal.ID)
	if len(terminalRows) != 3 {
		t.Fatalf("terminal distributed failure evidence = %+v", terminalRows)
	}
	for _, row := range terminalRows {
		if row.State != proto.InstFailed || row.Headless || !strings.Contains(row.Message, "collective failure") {
			t.Fatalf("terminal row = %+v", row)
		}
	}
	// A late teardown report does not regress the evidence.
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: terminalRows[0].UUID, State: proto.InstStopped, Message: "late teardown"}); err != nil {
		t.Fatal(err)
	}
	if row, _ := st.InstanceByUUID(terminalRows[0].UUID); row.State != proto.InstFailed {
		t.Fatalf("late teardown regressed terminal evidence: %+v", row)
	}
}

func removedByHub(h *distributedTestHub, nodeUUID, instanceUUID string) bool {
	for _, update := range h.updates[nodeUUID] {
		for _, removed := range update.RemovedInstances {
			if removed == instanceUUID {
				return true
			}
		}
	}
	return false
}

func TestDeploymentCleanupFailureDoesNotRepublishUnchangedPendingState(t *testing.T) {
	st, err := store.Open("file:controller_cleanup_backoff_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("offline-node", store.HashToken("token"), "offline-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{Name: "delete-me", NodeID: node.ID, State: "deleting"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&store.Instance{UUID: "pending-delete", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}, pushErr: errors.New("node not connected")}
	c := New(st, hub, nil)

	if err := c.reconcileDeployment(context.Background(), dep.ID); err == nil {
		t.Fatal("first reconcile succeeded, want connection error")
	}
	firstEvents, err := st.InstanceEvents(inst.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err == nil {
		t.Fatal("second reconcile succeeded, want connection error")
	}
	secondEvents, err := st.InstanceEvents(inst.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondEvents) != len(firstEvents) {
		t.Fatalf("unchanged pending-removal state added events: %d -> %d", len(firstEvents), len(secondEvents))
	}
}

func managementIP(node store.Node) string {
	addr, _ := network.ManagementAddress(node)
	return addr.IP
}
