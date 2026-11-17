package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A new memory limit reaches a running slice in place: the instance the
// change rewrote keeps its UUID (and so its dGPU cache, which the agent
// rewrites) and its new desired state is pushed to its node. An instance the
// change left out keeps its limit and is not replaced for it either; it takes
// the deployment's limit at its next restart.
func TestDGPULimitChangeIsPushedInPlace(t *testing.T) {
	st, err := store.Open("file:controller_quota_change_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("quota-node", store.HashToken("quota-token"), "quota-host")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{
		"runtime_driver":            "docker",
		"partition_safety_protocol": 1,
		"partition_assets":          `[{"backend":"nvidia-dgpu","root":"embedded://dgpu","files":["libvgpu.so"]}]`,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "quota-gpu", Name: "NVIDIA A100", Vendor: "nvidia", ArchID: "sm_80", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "quota/model", Revision: "main", Name: "quota", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/quota"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "quota", Engine: "vllm", State: "active", VRAMLimitBytes: 4 << 30, PartitionBackend: "nvidia-dgpu", QuickParams: `{"max_model_len":512}`})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("initial instances=%+v err=%v", rows, err)
	}
	old := rows[0]
	var oldSpec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(old.DesiredStateJSON), &oldSpec); err != nil {
		t.Fatal(err)
	}
	if oldSpec.VRAMLimitBytes != 4<<30 || oldSpec.UUID == "" {
		t.Fatalf("initial spec=%+v", oldSpec)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: old.UUID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	pushed := func() []proto.InstanceDesiredState {
		var out []proto.InstanceDesiredState
		for _, update := range hub.updates[node.UUID] {
			out = append(out, update.Instances...)
		}
		return out
	}
	before := len(pushed())

	// The change arrives the way the API makes it: the store setter's event
	// marks the deployment and queues it.
	sub := st.Bus.Subscribe(16, "deployment")
	defer sub.Close()
	if err := st.SetDeploymentDGPUQuota(dep.ID, 6<<30, []string{old.UUID}); err != nil {
		t.Fatal(err)
	}
	ev := <-sub.C()
	if ev.Reason != store.DeploymentChangeDGPUQuota {
		t.Fatalf("event = %+v", ev)
	}
	c.q.mu.Lock()
	delete(c.q.pending, depKey(dep.ID))
	c.q.mu.Unlock()
	c.onEvent(ev)
	c.q.mu.Lock()
	_, queued := c.q.pending[depKey(dep.ID)]
	c.q.mu.Unlock()
	if !queued {
		t.Fatal("the quota change did not queue the deployment")
	}
	for range 2 {
		if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = st.InstancesForDeployment(dep.ID)
	if err != nil || len(rows) != 1 || rows[0].UUID != old.UUID || rows[0].State != proto.InstRunning {
		t.Fatalf("the slice was replaced: %+v err=%v", rows, err)
	}
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(rows[0].DesiredStateJSON), &spec); err != nil || spec.VRAMLimitBytes != 6<<30 {
		t.Fatalf("stored spec=%+v err=%v", spec, err)
	}
	got := pushed()[before:]
	if len(got) != 1 || got[0].UUID != old.UUID || got[0].VRAMLimitBytes != 6<<30 {
		t.Fatalf("pushed after the change = %+v, want the slice once at 6 GiB", got)
	}

	// A push that fails is tried again on a later pass; an offline node is
	// not pushed to (its reconnect hands it the full desired state).
	if err := st.SetDeploymentDGPUQuota(dep.ID, 7<<30, []string{old.UUID}); err != nil {
		t.Fatal(err)
	}
	c.dgpuPushes.mark(dep.ID)
	hub.pushErr = errors.New("connection reset")
	c.pushDGPULimits(dep, instancesOf(t, st, dep.ID))
	hub.pushErr = nil
	c.pushDGPULimits(dep, instancesOf(t, st, dep.ID))
	if got := pushed()[before:]; len(got) != 2 || got[1].VRAMLimitBytes != 7<<30 {
		t.Fatalf("pushed after a failed push = %+v", got)
	}
	c.dgpuPushes.mark(dep.ID)
	hub.online[node.UUID] = false
	c.pushDGPULimits(dep, instancesOf(t, st, dep.ID))
	hub.online[node.UUID] = true
	if got := pushed()[before:]; len(got) != 2 {
		t.Fatalf("pushed to an offline node: %+v", got)
	}

	// Left out (here: a lower limit it could not take at once), it keeps
	// its limit and is not replaced; nothing is pushed for it.
	if err := st.SetDeploymentDGPUQuota(dep.ID, 3<<30, nil); err != nil {
		t.Fatal(err)
	}
	c.dgpuPushes.mark(dep.ID)
	for range 2 {
		if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ = st.InstancesForDeployment(dep.ID)
	if len(rows) != 1 || rows[0].UUID != old.UUID || rows[0].State != proto.InstRunning {
		t.Fatalf("a pending limit replaced the slice: %+v", rows)
	}
	if err := json.Unmarshal([]byte(rows[0].DesiredStateJSON), &spec); err != nil || spec.VRAMLimitBytes != 7<<30 {
		t.Fatalf("a left-out slice's limit changed: %+v", spec)
	}
}

func TestPartitionBackendDriftTriggersInstanceReplacement(t *testing.T) {
	st, err := store.Open("file:controller_backend_drift_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("backend-drift-node", store.HashToken("token"), "host")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{NodeID: node.ID, State: "active", Replicas: 1, VRAMLimitBytes: 4 << 30, PartitionBackend: "nvidia-dgpu"})
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := json.Marshal(proto.InstanceDesiredState{UUID: "stale-backend-instance", VRAMLimitBytes: 4 << 30, PartitionBackend: "ascend-dgpu"})
	if _, err := st.CreateInstance(&store.Instance{UUID: "stale-backend-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, DesiredStateJSON: string(stale)}); err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(rows) != 1 || rows[0].State != proto.InstRemoving {
		t.Fatalf("stale backend generation should await teardown: %+v err=%v", rows, err)
	}
}

// While a partition change waits for its node to confirm the old
// generation's teardown, every retry finds the instance already marked:
// the timeline gets one entry for it, not one per retry.
func TestPartitionChangeMarksRemovalOnce(t *testing.T) {
	st, err := store.Open("file:controller_partition_once_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("partition-once-node", store.HashToken("token"), "host")
	if err != nil {
		t.Fatal(err)
	}
	// The deployment now runs on whole cards; its instance still holds a slice.
	dep, err := createTestDeployment(t, st, &store.Deployment{NodeID: node.ID, State: "active", Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	slice, _ := json.Marshal(proto.InstanceDesiredState{UUID: "slice-instance", VRAMLimitBytes: 4 << 30, PartitionBackend: "nvidia-dgpu"})
	if _, err := st.CreateInstance(&store.Instance{UUID: "slice-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, DesiredStateJSON: string(slice)}); err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	for range 5 {
		if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.DeploymentEvents(dep.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	marked := 0
	for _, event := range events {
		if strings.Contains(event.Data, quotaChangeRemovalPending) {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("the removal was recorded %d times over 5 retries, want once", marked)
	}
	if rows, _ := st.InstancesForDeployment(dep.ID); len(rows) != 1 || rows[0].State != proto.InstRemoving {
		t.Fatalf("instance = %+v, want removing", rows)
	}
}
