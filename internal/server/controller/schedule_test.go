package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/planning"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPickPortSkipsReportedHostTCPListeners(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("port-inventory-node", store.HashToken("port-token"), "port-inventory-node")
	if err != nil {
		t.Fatal(err)
	}
	node.NetworkInventory = `{"interfaces":[],"rdma_ports":[],"engine_port_listeners":[30000]}`
	c := New(st, nil, nil)
	c.SetEnginePorts(30000, 30001)
	got, rejected := c.pickPort(node)
	if rejected != nil || got != 30001 {
		t.Fatalf("pickPort = %d, %+v; want 30001 without rejection", got, rejected)
	}
}

func TestPickPortReportsExhaustedRange(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("port-exhausted-node", store.HashToken("port-token"), "port-exhausted-node")
	if err != nil {
		t.Fatal(err)
	}
	node.NetworkInventory = `{"interfaces":[],"rdma_ports":[],"engine_port_listeners":[30000]}`
	c := New(st, nil, nil)
	c.SetEnginePorts(30000, 30000)
	got, rejected := c.pickPort(node)
	if got != 0 || rejected == nil || rejected.Code != scheduler.ReasonEnginePortsExhausted ||
		rejected.Params["start"] != 30000 || rejected.Params["end"] != 30000 || rejected.Params["node"] != "port-exhausted-node" {
		t.Fatalf("pickPort = %d, %+v; want 0 with engine_ports_exhausted for 30000-30000", got, rejected)
	}
}

func TestReconcileDeploymentReportsExhaustedEnginePorts(t *testing.T) {
	st, err := store.Open("file:controller_engine_ports_exhausted?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("ports-full-node", store.HashToken("token"), "ports-full-node")
	if err != nil {
		t.Fatal(err)
	}
	// The one port in range is held by a host service the agent reported.
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).
		Update("network_inventory", `{"interfaces":[],"rdma_ports":[],"engine_port_listeners":[30000]}`).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{
		NodeID: node.ID, Index: 0, UUID: "ports-full-a100", Name: "NVIDIA A100-SXM4-80GB",
		Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0",
		MemoryTotal: 80 << 30,
	}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/ports-full", Revision: "main", Name: "ports-full",
		WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/ports-full"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "ports-full", ModelID: model.ID, NodeID: node.ID, ServedName: "ports-full", Engine: "vllm",
		Placement: `{"tp":1,"gpus":[0]}`, QuickParams: `{"max_model_len":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.SetEnginePorts(30000, 30000)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 0 {
		t.Fatalf("exhausted ports launched instances=%+v err=%v", instances, err)
	}
	if updates := hub.updates[node.UUID]; len(updates) != 0 {
		t.Fatalf("exhausted ports pushed runtime state=%+v", updates)
	}
	event, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found {
		t.Fatalf("schedule event found=%v err=%v", found, err)
	}
	for _, want := range []string{`"code":"engine_ports_exhausted"`, `"node":"ports-full-node"`, `"start":30000`, `"end":30000`} {
		if !strings.Contains(event.Data, want) {
			t.Fatalf("schedule event %s lacks %s", event.Data, want)
		}
	}
}

func TestDeploymentAPIContractPrefersCurrentModelOperations(t *testing.T) {
	tests := []struct {
		name       string
		operations string
		legacy     string
		want       []string
	}{
		{name: "current operation overrides template", operations: `[{"type":"openai.embeddings"}]`, legacy: `{"health_endpoint":"/legacy","operations":[{"type":"openai.audio.transcribe"}]}`, want: []string{inferenceapi.OperationEmbeddings}},
		{name: "empty list falls back to the snapshot", operations: `[]`, legacy: `{"operations":[{"type":"openai.chat"}]}`, want: []string{inferenceapi.OperationChat}},
		{name: "spaced empty list falls back to the snapshot", operations: " [ ]\n", legacy: `{"operations":[{"type":"openai.chat"}]}`, want: []string{inferenceapi.OperationChat}},
		{name: "legacy row keeps snapshot", operations: "", legacy: `{"operations":[{"type":"openai.rerank"}]}`, want: []string{inferenceapi.OperationRerank}},
		{name: "malformed explicit value does not fall back", operations: `{not-json`, legacy: `{"operations":[{"type":"openai.chat"}]}`, want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := &store.Model{Operations: tc.operations}
			got := planning.DeploymentAPIContract(model, []byte(tc.legacy))
			var contract inferenceapi.Contract
			if len(got) == 0 {
				t.Fatalf("contract is empty; want operations %v", tc.want)
			}
			if err := json.Unmarshal(got, &contract); err != nil {
				t.Fatalf("contract JSON=%s: %v", got, err)
			}
			if len(contract.Operations) != len(tc.want) {
				t.Fatalf("operations=%v want %v (raw=%s)", contract.Operations, tc.want, got)
			}
			for i, want := range tc.want {
				if contract.Operations[i].Type != want {
					t.Fatalf("operations[%d]=%q want %q", i, contract.Operations[i].Type, want)
				}
			}
		})
	}
}

func TestVendorAndArchIDOfGPUPreferPersistedFields(t *testing.T) {
	node := &store.Node{GPUs: []store.GPU{{
		Index: 0, Name: "H100", ComputeCapability: "9.0",
		Vendor: "ascend", ArchID: "910B4",
	}}}
	gpu, ok := node.GPUByIndex(0)
	if !ok {
		t.Fatal("node does not report GPU 0")
	}
	if got := node.VendorOf(gpu); got != "ascend" {
		t.Fatalf("VendorOf = %q, want ascend", got)
	}
	if got := node.ArchOf(gpu); got.ID != "910B4" || got.Family != "910b" {
		t.Fatalf("ArchOf = %+v, want 910B4/910b", got)
	}
}

func TestDeploymentRetryRemovesPriorContainersAndKeepsTerminalFailure(t *testing.T) {
	st, err := store.Open("file:controller_retry_cleanup?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("retry-cleanup-node", store.HashToken("token"), "retry-cleanup-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "retry-gpu", Name: "NVIDIA A100", Vendor: "nvidia", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/retry", Revision: "main", Name: "retry", WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/retry"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "retry", Engine: "vllm", State: "active", QuickParams: `{"max_model_len":512}`})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.SetRetryPolicy(0, 3)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}

	var failedUUIDs []string
	for attempt := 0; attempt < 3; attempt++ {
		rows, err := st.InstancesForDeployment(dep.ID)
		if err != nil || len(rows) != attempt+1 {
			t.Fatalf("attempt %d rows=%+v err=%v", attempt+1, rows, err)
		}
		var current store.Instance
		for _, row := range rows {
			if row.State != proto.InstFailed {
				current = row
			}
		}
		if current.UUID == "" {
			t.Fatalf("attempt %d has no active instance: %+v", attempt+1, rows)
		}
		if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: current.UUID, State: proto.InstFailed, Message: "fixture failure"}); err != nil {
			t.Fatal(err)
		}
		failedUUIDs = append(failedUUIDs, current.UUID)
		hub.updates[node.UUID] = nil
		if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
			t.Fatal(err)
		}
		updates := hub.updates[node.UUID]
		if attempt == 2 {
			if len(updates) != 0 {
				t.Fatalf("terminal failure unexpectedly removed debug evidence: %+v", updates)
			}
			break
		}
		if len(updates) < 2 {
			t.Fatalf("retry %d updates=%+v, want removal followed by replacement", attempt+1, updates)
		}
		removed := map[string]bool{}
		for _, uuid := range updates[0].RemovedInstances {
			removed[uuid] = true
		}
		for _, uuid := range failedUUIDs {
			if !removed[uuid] {
				t.Fatalf("retry %d did not remove failed container %s: %+v", attempt+1, uuid, updates[0])
			}
		}
		if len(updates[len(updates)-1].Instances) != 1 {
			t.Fatalf("retry %d did not push one replacement: %+v", attempt+1, updates)
		}
	}
	// Each rebuild is one timeline event on the deployment; the first launch
	// and the exhausted third failure are not rebuilds.
	events, err := st.DeploymentEvents(dep.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var rebuilt []store.InstanceEvent
	for _, ev := range events {
		if ev.Code == store.EventReplicaRebuilt {
			rebuilt = append(rebuilt, ev)
		}
	}
	if len(rebuilt) != 2 {
		t.Fatalf("REPLICA_REBUILT events = %+v, want one per rebuild (2)", rebuilt)
	}
	for i, ev := range rebuilt {
		var params map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &params); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"replica": float64(1), "attempt": float64(i + 2), "reason": "fixture failure", "node": node.DisplayName(), store.EventParamNodeUUIDs: []any{node.UUID}}
		if !reflect.DeepEqual(params, want) || ev.Kind != store.EventKindLifecycle {
			t.Fatalf("rebuild %d = kind %q %v, want %v", i, ev.Kind, params, want)
		}
		// Written on the attempt that failed, so the entry leads to its output.
		if ev.InstanceUUID != failedUUIDs[i] || ev.DeploymentID != dep.ID {
			t.Fatalf("rebuild %d is on instance %q of deployment %d, want the failed attempt %q of %d", i, ev.InstanceUUID, ev.DeploymentID, failedUUIDs[i], dep.ID)
		}
	}
}

func TestVendorAndArchIDOfGPUFallback(t *testing.T) {
	node := &store.Node{GPUs: []store.GPU{{
		Index: 0, Name: "H100", ComputeCapability: "9.0",
	}}}
	gpu, ok := node.GPUByIndex(0)
	if !ok {
		t.Fatal("node does not report GPU 0")
	}
	if got := node.VendorOf(gpu); got != "nvidia" {
		t.Fatalf("VendorOf fallback = %q, want nvidia", got)
	}
	// A row without a vendor or ArchID still has the architecture its
	// compute capability names.
	if got := node.ArchOf(gpu); got.ID != "sm_90" || got.Family != "hopper" {
		t.Fatalf("ArchOf fallback = %+v, want sm_90/hopper", got)
	}
}

func TestReconcileDeploymentPersistsAndPushesManualGPUPlacement(t *testing.T) {
	st, err := store.Open("file:controller_manual_gpu_placement?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("manual-gpu-node", store.HashToken("token"), "manual-gpu-node")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if err := st.DB().Create(&store.GPU{
			NodeID: node.ID, Index: index, UUID: "manual-gpu-" + string(rune('0'+index)), Name: "NVIDIA H100",
			Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0",
			MemoryTotal: 80 << 30,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/manual-gpu", Revision: "main", Name: "manual-gpu",
		WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/manual-gpu"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "manual-gpu", ModelID: model.ID, NodeID: node.ID, ServedName: "manual-gpu", Engine: "vllm",
		Placement: `{"tp":1,"gpus":[1]}`, QuickParams: `{"max_model_len":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances=%+v err=%v", instances, err)
	}
	var indexes []int
	if err := json.Unmarshal([]byte(instances[0].GPUIndexes), &indexes); err != nil || len(indexes) != 1 || indexes[0] != 1 {
		t.Fatalf("persisted GPU indexes=%v raw=%q err=%v", indexes, instances[0].GPUIndexes, err)
	}
	updates := hub.updates[node.UUID]
	if len(updates) != 1 || len(updates[0].Instances) != 1 || len(updates[0].Instances[0].GPUIndexes) != 1 || updates[0].Instances[0].GPUIndexes[0] != 1 {
		t.Fatalf("pushed state update=%+v", updates)
	}
}

// A deployment that named node labels must not be scheduled onto a node that
// stopped matching them between create and reconcile. The distributed path
// always rechecked; the single-node path did not, so a label edit was silently
// ignored for ordinary deployments.
// A placement hint that states an unusable tensor parallel - a fractional or a
// negative one - pins nothing: the controller reads it as unset and lets the
// scheduler choose from the GPUs that are allowed. The api's old reader
// truncated 2.5 to 2, which would have pinned a degree the single allowed GPU
// cannot carry; this is the behaviour the unified parser preserves.
func TestScheduleInstanceTreatsUnusableTPAsUnpinned(t *testing.T) {
	for _, tc := range []struct {
		name      string
		placement string
	}{
		{"fractional tp", `{"tp":2.5,"gpus":[0]}`},
		{"negative tp", `{"tp":-1,"gpus":[0]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:controller_"+strings.ReplaceAll(tc.name, " ", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node, err := st.RegisterNode(tc.name+"-node", store.HashToken("token"), tc.name+"-node")
			if err != nil {
				t.Fatal(err)
			}
			for index := 0; index < 2; index++ {
				if err := st.DB().Create(&store.GPU{
					NodeID: node.ID, Index: index, UUID: tc.name + fmt.Sprintf("-a100-%d", index), Name: "NVIDIA A100-SXM4-80GB",
					Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0",
					MemoryTotal: 80 << 30,
				}).Error; err != nil {
					t.Fatal(err)
				}
			}
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "fixture/" + tc.name, Revision: "main", Name: tc.name,
				WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/" + tc.name}); err != nil {
				t.Fatal(err)
			}
			dep, err := createTestDeployment(t, st, &store.Deployment{
				Name: tc.name, ModelID: model.ID, NodeID: node.ID, ServedName: tc.name, Engine: "vllm",
				Placement: tc.placement, QuickParams: `{"max_model_len":512}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
			c := New(st, hub, nil)
			if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatal(err)
			}
			instances, err := st.InstancesForDeployment(dep.ID)
			if err != nil || len(instances) != 1 {
				t.Fatalf("instances=%+v err=%v", instances, err)
			}
			var spec proto.InstanceDesiredState
			if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &spec); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(spec.GPUIndexes, []int{0}) {
				t.Fatalf("gpu_indexes = %v, want exactly the allowed GPU", spec.GPUIndexes)
			}
			if argc := engineTensorParallel(t, spec); argc != 1 {
				t.Fatalf("the command carries tensor parallel %d, want the scheduler's 1: %v", argc, spec.Entrypoint)
			}
		})
	}
}

// engineTensorParallel reads the tensor-parallel degree out of a desired
// state's command, which is where the scheduler's choice reaches the engine.
func engineTensorParallel(t *testing.T, spec proto.InstanceDesiredState) int {
	t.Helper()
	command := append(append([]string{}, spec.Entrypoint...), spec.Argv...)
	for index, token := range command {
		if token == "--tensor-parallel-size" && index+1 < len(command) {
			value, err := strconv.Atoi(command[index+1])
			if err != nil {
				t.Fatalf("tensor parallel %q is not a number", command[index+1])
			}
			return value
		}
	}
	t.Fatalf("no --tensor-parallel-size in %v", command)
	return 0
}

func TestReconcileDeploymentRejectsPinnedGPUAfterHardwareDrift(t *testing.T) {
	st, err := store.Open("file:controller_template_gpu_drift?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("template-gpu-drift-node", store.HashToken("token"), "template-gpu-drift-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{
		NodeID: node.ID, Index: 0, UUID: "replacement-a100", Name: "NVIDIA A100-SXM4-80GB",
		Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0",
		MemoryTotal: 80 << 30,
	}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/template-gpu-drift", Revision: "main", Name: "template-gpu-drift",
		WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/template-gpu-drift"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "template-gpu-drift", ModelID: model.ID, NodeID: node.ID, ServedName: "template-gpu-drift", Engine: "vllm",
		Placement: `{"tp":1,"gpus":[0]}`, QuickParams: `{"max_model_len":512}`,
		CatalogSelectionSnapshot: `{"spec_name":"h100-vllm","quantization":"bf16","gpus":{"vendor":"nvidia","name":"H100"},"unsupported":[]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 0 {
		t.Fatalf("hardware drift launched instances=%+v err=%v", instances, err)
	}
	if updates := hub.updates[node.UUID]; len(updates) != 0 {
		t.Fatalf("hardware drift pushed runtime state=%+v", updates)
	}
	event, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found || !strings.Contains(event.Data, "placement_rejected") {
		t.Fatalf("hardware drift schedule event=%+v found=%v err=%v", event, found, err)
	}
}

func TestReconcileDeploymentEnforcesUnsupportedRuleWithoutGPUPredicate(t *testing.T) {
	st, err := store.Open("file:controller_template_unsupported_without_predicate?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("template-unsupported-node", store.HashToken("token"), "template-unsupported-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{
		NodeID: node.ID, Index: 0, UUID: "unsupported-a100", Name: "NVIDIA A100-SXM4-80GB",
		Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0",
		MemoryTotal: 80 << 30,
	}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/template-unsupported", Revision: "main", Name: "template-unsupported",
		WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/template-unsupported"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "template-unsupported", ModelID: model.ID, NodeID: node.ID, ServedName: "template-unsupported", Engine: "vllm",
		Placement: `{"tp":1,"gpus":[0]}`, QuickParams: `{"max_model_len":512}`,
		CatalogSelectionSnapshot: `{"spec_name":"generic-vllm","quantization":"bf16","unsupported":[{"gpus":{"vendor":"nvidia","name":"A100"},"quantization":"bf16","reason":"A100 bf16 disabled"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 0 {
		t.Fatalf("unsupported GPU launched instances=%+v err=%v", instances, err)
	}
	if updates := hub.updates[node.UUID]; len(updates) != 0 {
		t.Fatalf("unsupported GPU pushed runtime state=%+v", updates)
	}
	event, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found || !strings.Contains(event.Data, "pool_no_fit") || !strings.Contains(event.Data, "placement_rejected") || !strings.Contains(event.Data, "A100 bf16 disabled") {
		t.Fatalf("unsupported GPU schedule event=%+v found=%v err=%v", event, found, err)
	}
}

func TestReconcileDeploymentUsesCurrentModelOperationsOverTemplateContract(t *testing.T) {
	st, err := store.Open("file:controller_current_operations?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("current-operations-node", store.HashToken("token"), "current-operations-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{
		NodeID: node.ID, Index: 0, UUID: "current-operations-gpu", Name: "NVIDIA H100",
		Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0",
		MemoryTotal: 80 << 30,
	}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/current-operations", Revision: "main", Name: "current-operations",
		WeightBytes: 1 << 30,
		Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
		Operations:  `[{"type":"openai.embeddings"}]`, InputModalities: `["text"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/current-operations"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "current-operations", ModelID: model.ID, NodeID: node.ID, ServedName: "current-operations", Engine: "vllm",
		QuickParams: `{"max_model_len":512}`,
		// This represents a deployment created with the old template API. It
		// must not replace the model's current editable operation contract.
		CatalogSelectionSnapshot: `{"api":{"health_endpoint":"/legacy","operations":[{"type":"openai.chat"}]}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	updates := hub.updates[node.UUID]
	if len(updates) != 1 || len(updates[0].Instances) != 1 {
		t.Fatalf("state updates=%+v", updates)
	}
	var contract inferenceapi.Contract
	launchContract := updates[0].Instances[0].ResolvedLaunch.APIContract
	if err := json.Unmarshal(launchContract, &contract); err != nil {
		t.Fatalf("launch API contract=%s: %v", launchContract, err)
	}
	if len(contract.Operations) != 1 || contract.Operations[0].Type != inferenceapi.OperationEmbeddings {
		t.Fatalf("launch operations=%+v, want current model embeddings operation", contract.Operations)
	}
}

func TestReconcileDeploymentEmptyModelOperationsFallBackToTemplate(t *testing.T) {
	st, err := store.Open("file:controller_cleared_operations?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("cleared-operations-node", store.HashToken("token"), "cleared-operations-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "cleared-operations-gpu", Name: "NVIDIA A100", Vendor: "nvidia", ArchID: "sm_80", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/cleared-operations", Revision: "main", Name: "cleared-operations", WeightBytes: 1 << 30,
		Meta:       `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
		Operations: `[]`, InputModalities: `[]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/cleared-operations"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "cleared-operations", ModelID: model.ID, NodeID: node.ID, ServedName: "cleared-operations", Engine: "vllm", QuickParams: `{"max_model_len":512}`,
		CatalogSelectionSnapshot: `{"api":{"operations":[{"type":"openai.chat"}]}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	updates := hub.updates[node.UUID]
	if len(updates) != 1 || len(updates[0].Instances) != 1 {
		t.Fatalf("state updates=%+v", updates)
	}
	var contract inferenceapi.Contract
	if err := json.Unmarshal(updates[0].Instances[0].ResolvedLaunch.APIContract, &contract); err != nil {
		t.Fatal(err)
	}
	if len(contract.Operations) != 1 || contract.Operations[0].Type != inferenceapi.OperationChat {
		t.Fatalf("empty model operations did not fall back to the template contract: %+v", contract.Operations)
	}
}

func TestPickPortUsesTheNodesOwnRange(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("own-range-node", store.HashToken("port-token"), "own-range-node")
	if err != nil {
		t.Fatal(err)
	}
	node.NetworkInventory = `{"interfaces":[],"rdma_ports":[],"engine_port_listeners":[40000],"engine_port_range":{"start":40000,"end":40001}}`
	c := New(st, nil, nil)
	c.SetEnginePorts(30000, 30010)
	got, rejected := c.pickPort(node)
	if rejected != nil || got != 40001 {
		t.Fatalf("pickPort = %d, %+v; want 40001 from the node's own range", got, rejected)
	}
	node.NetworkInventory = `{"interfaces":[],"rdma_ports":[],"engine_port_range":{"start":0,"end":-1}}`
	if got, rejected = c.pickPort(node); rejected != nil || got != 30000 {
		t.Fatalf("pickPort with an invalid own range = %d, %+v; want the server's 30000", got, rejected)
	}
}
