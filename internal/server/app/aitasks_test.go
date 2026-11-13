package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A scheduling question is seeded with what one replica needs, where it may
// go and the scheduler's own refusal - the counts by candidate status and
// the closest nodes - not with a recomputation.
func TestDeploymentContextCarriesTheSchedulersRefusal(t *testing.T) {
	f := newAIFixture(t, "app_ai_deployment_context")
	if err := f.st.SetDeploymentRequirements(f.dep.ID, nodelabel.Requirements{"gpuworks.ai/gpu-model": {"H100"}}, store.PlacementBinpack); err != nil {
		t.Fatal(err)
	}
	f.instance(t, "ai-dep-running", proto.InstRunning, "", nil)
	f.st.AddDeploymentEventCode(f.dep.ID, store.EventKindSchedule, "pool_no_fit", map[string]any{"scheduler": map[string]any{"code": "pool_no_fit", "params": map[string]any{"busy": 3, "closest": []any{map[string]any{"node": "n9", "name": "gpu-b9"}}}}})
	time.Sleep(5 * time.Millisecond)
	f.st.AddDeploymentEventCode(f.dep.ID, store.EventKindSchedule, "pool_no_fit", map[string]any{"scheduler": map[string]any{"code": "pool_no_fit", "params": map[string]any{"busy": 4}}})

	c, err := f.ai.deploymentContext(f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != f.dep.Name || c.Strategy != string(store.PlacementBinpack) || len(c.Instances) != 1 || c.Instances[0].Node == "" {
		t.Fatalf("context = %+v", c)
	}
	if c.RefusalAtMS == 0 || c.FirstRefusalMS == 0 || c.FirstRefusalMS > c.RefusalAtMS {
		t.Fatalf("refusal times %d / %d", c.FirstRefusalMS, c.RefusalAtMS)
	}
	p := c.Prompt()
	for _, want := range []string{"not being placed", `"code":"pool_no_fit"`, `"busy":4`, "H100", "binpack", "ai-dep-running"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if _, err := f.ai.deploymentContext(9999); err == nil {
		t.Fatal("unknown deployment should fail")
	}
}

// A node health check sees each card's level by the UI's rule, the fast
// interconnect, what runs there, and only the node's and agent's warnings -
// not the instances' own log lines.
func TestNodeContextCarriesHealthTopologyAndInfraWarnings(t *testing.T) {
	f := newAIFixture(t, "app_ai_node_context")
	if err := f.st.SetNodeState(f.node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	for _, g := range []store.GPU{
		{NodeID: f.node.ID, Index: 0, Name: "H100"},
		{NodeID: f.node.ID, Index: 1, Name: "H100", HealthStatus: "Warning", HealthCode: "temperature_high"},
		{NodeID: f.node.ID, Index: 2, Name: "H100", HealthStatus: "Error", HealthCode: "xid_79", HealthMessage: "XID 79: GPU has fallen off the bus"},
		// Neither is faulty, and neither can serve: the seed has to say why.
		{NodeID: f.node.ID, Index: 3, Name: "H100", ForeignProcesses: `[{"pid":4242,"name":"python3"}]`},
		{NodeID: f.node.ID, Index: -1, Name: "H100", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
	} {
		if err := f.st.DB().Create(&g).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.DB().Model(&store.Node{}).Where("id = ?", f.node.ID).Updates(map[string]any{
		"topo_json": `{"FastCliques":[[0,1]]}`, "runtime_gpu_blocked": accelerator.RuntimeGPUBlockedRuntimeUnconfigured,
		"runtime_driver": "udocker", "runtime_fallback_from": "docker", "runtime_fallback_reason": accelerator.RuntimeGPUBlockedPluginMissing,
	}).Error; err != nil {
		t.Fatal(err)
	}
	f.instance(t, "ai-node-inst", proto.InstRunning, "", nil)
	now := time.Now().UnixMilli()
	if err := f.logs.Append([]logstore.Entry{
		{Node: f.node.UUID, Source: logstore.SourceAgent, Level: logstore.LevelError, TimeMS: now - 2000, Line: "nvidia-smi: GPU 2 is lost"},
		{Node: f.node.UUID, Source: logstore.SourceNode, Level: logstore.LevelWarn, TimeMS: now - 1000, Line: "kernel: NVRM: Xid 79"},
		{Node: f.node.UUID, Source: logstore.SourceNode, Level: logstore.LevelInfo, TimeMS: now - 500, Line: "routine info"},
		{Node: f.node.UUID, Instance: "ai-node-inst", Source: logstore.SourceInstance, Level: logstore.LevelError, TimeMS: now, Line: "engine traceback"},
	}); err != nil {
		t.Fatal(err)
	}

	c, err := f.ai.nodeContext(f.node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	levels := []string{}
	for _, g := range c.GPUs {
		levels = append(levels, g.Level)
	}
	if strings.Join(levels, ",") != "ok,warning,error,ok,ok" || c.GPUs[2].Code != "xid_79" {
		t.Fatalf("gpus = %+v", c.GPUs)
	}
	if len(c.FastCliques) != 1 || len(c.Instances) != 1 || c.Scheduling != "schedulable" {
		t.Fatalf("context = %+v", c)
	}
	if len(c.Logs) != 2 || !strings.Contains(c.Logs[0].Line, "[agent] nvidia-smi") || !strings.Contains(c.Logs[1].Line, "[node] kernel") {
		t.Fatalf("logs = %+v", c.Logs)
	}
	p := c.Prompt()
	for _, want := range []string{"Check this node's health", "level=error code=xid_79", "fallen off the bus", "fast interconnect groups: [[0 1]]", ".items[].points[-1][1]", "Do not re-read this node",
		"pci 0000:1b:00.0 H100 level=ok code= can_serve=no (" + accelerator.CardPassedThrough + ")", "can_serve=no (in use outside the platform: python3 pid 4242)",
		"container runtime cannot use the accelerators: " + accelerator.RuntimeGPUBlockedRuntimeUnconfigured,
		"container runtime: udocker", "runtime passed over for runveil (udocker): docker (" + accelerator.RuntimeGPUBlockedPluginMissing + ")"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "engine traceback") || strings.Contains(p, "routine info") {
		t.Fatalf("prompt carries instance or info lines:\n%s", p)
	}
}

// A first turn folds the question into the seed; a follow-up keeps the
// task's own seed, then the thread, then the new question.
func TestPrepareLaysOutFirstTurnsAndFollowUps(t *testing.T) {
	f := newAIFixture(t, "app_ai_prepare")
	f.configureLLM(t, &scriptedLLM{})
	in := f.instance(t, "ai-prep-inst", proto.InstFailed, "engine exited", nil)

	first, err := f.ai.prepare(aihelper.AskRequest{Task: aihelper.TaskInstanceDiagnose, Target: aihelper.Target{InstanceUUID: in.UUID}, Question: "only on gpu 1?", Locale: "zh"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.messages) != 2 || !strings.HasPrefix(first.messages[1].Content, "Question: only on gpu 1?") || !strings.Contains(first.messages[0].Content, "Simplified Chinese") {
		t.Fatalf("first turn = %+v", first.messages)
	}

	follow, err := f.ai.prepare(aihelper.AskRequest{Task: aihelper.TaskInstanceDiagnose, Target: aihelper.Target{InstanceUUID: in.UUID}, Question: "and the fix?",
		History: []aihelper.Message{{Role: "user", Content: "diagnose"}, {Role: "assistant", Content: "OOM"}, {Role: "tool", Content: "dropped"}}})
	if err != nil {
		t.Fatal(err)
	}
	roles := []string{}
	for _, m := range follow.messages {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,user,assistant,user" || !strings.HasPrefix(follow.messages[1].Content, "Diagnose why") || follow.messages[4].Content != "and the fix?" {
		t.Fatalf("follow-up = %+v", follow.messages)
	}

	// A free question about a subject is seeded like the subject's task
	// under the general prompt; it follows the user's language.
	ask, err := f.ai.prepare(aihelper.AskRequest{Task: aihelper.TaskAsk, Target: aihelper.Target{InstanceUUID: in.UUID}, Question: "why?", Locale: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ask.messages[0].Content, aihelper.SystemAsk) || !strings.Contains(ask.messages[0].Content, "different language") || !strings.Contains(ask.messages[1].Content, "engine exited") {
		t.Fatalf("ask = %+v", ask.messages)
	}
	if _, err := f.ai.prepare(aihelper.AskRequest{Task: aihelper.TaskNodeHealth, Target: aihelper.Target{NodeUUID: "nope"}}); err == nil {
		t.Fatal("unknown node should fail")
	}
	health, err := f.ai.prepare(aihelper.AskRequest{Task: aihelper.TaskNodeHealth, Target: aihelper.Target{NodeUUID: f.node.UUID}, Locale: "zh"})
	if err != nil {
		t.Fatal(err)
	}
	sys := health.messages[0].Content
	if strings.Index(sys, aihelper.NodeHealthClose) < strings.Index(sys, "Read before you answer") || !strings.Contains(health.messages[1].Content, ".items[].points[-1][1]") {
		t.Fatalf("node health prompt =\n%s\n\n%s", sys, health.messages[1].Content)
	}
}

// The scheduling task runs end to end on the real store.
func TestAskDeploymentUnscheduledRuns(t *testing.T) {
	f := newAIFixture(t, "app_ai_ask_unscheduled")
	llm := &scriptedLLM{turns: []string{finalTurn}}
	f.configureLLM(t, llm)
	if _, err := f.ai.Ask(context.Background(), aihelper.AskRequest{Task: aihelper.TaskDeploymentUnscheduled, Target: aihelper.Target{DeploymentID: f.dep.ID}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(llm.reqs[0].Messages[0].Content, aihelper.SystemSchedulingDiagnosis) || !strings.Contains(llm.reqs[0].Messages[1].Content, "(none recorded)") {
		t.Fatalf("messages = %+v", llm.reqs[0].Messages)
	}
}
