package scheduling

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

func TestSchedulingEvaluateUsesRealAppAndHeterogeneousNodes(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	env := testenv.NewWithModelEndpoint(t, hub.Endpoint)
	big := env.AddNode("sched-24g", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	small := env.AddNode("sched-10g", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A10", VRAMMB: 10240}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "sched-24g")
	env.WaitNodeOnline(t, "sched-10g")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 2 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: hub.Repo, Revision: hub.Revision, Name: "sched-model", Arch: "Qwen3ForCausalLM", WeightBytes: hub.WeightBytes, Meta: `{"model_type":"qwen3","num_hidden_layers":36,"hidden_size":2560,"num_attention_heads":32,"num_key_value_heads":8,"max_position_embeddings":40960}`})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: n.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
			t.Fatal(err)
		}
	}
	api := env.API(t)
	evaluate := func(uuid string) (map[string]any, string) {
		body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": uuid, "engine": "vllm", "quick_params": map[string]any{"max_model_len": 60000}})
		resp, err := api.PostJSON("/api/v1/evaluate", body)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("evaluate %s: %d %s", uuid, resp.StatusCode, data)
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out, string(data)
	}
	bigNode := findNodeByName(t, env, big)
	smallNode := findNodeByName(t, env, small)
	good, goodRaw := evaluate(bigNode.UUID)
	if ok, _ := good["ok"].(bool); !ok {
		t.Fatalf("24GiB evaluate rejected: %s", goodRaw)
	}
	bad, badRaw := evaluate(smallNode.UUID)
	if ok, _ := bad["ok"].(bool); ok || !strings.Contains(strings.ToLower(badRaw), "fit") {
		t.Fatalf("10GiB evaluate missing startup memory rejection: %s", badRaw)
	}
}

func findNodeByName(t *testing.T, env *testenv.Env, node *testenv.Node) store.Node {
	t.Helper()
	rows, err := env.Store().ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	prefix := "GPU-" + node.Name + "-"
	for _, row := range rows {
		with, err := env.Store().NodeWithGPUs(row.UUID)
		if err == nil && len(with.GPUs) > 0 && strings.HasPrefix(with.GPUs[0].UUID, prefix) {
			return *with
		}
	}
	t.Fatalf("node %s not found", node.Name)
	return store.Node{}
}

func TestFakeQwenMetadataDrivesHeterogeneousScheduling(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	meta, err := modelmeta.Fetch(context.Background(), modelrepo.SourceHF, hub.Repo, hub.Revision, "", hub.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	// Startup admission uses fixed requirements only. The long context affects the
	// informational KV capacity below, not whether the process can launch, which
	// is why the request carries no KV bytes at all. The engine reserve is not KV:
	// it is what the engine holds back besides the weights, and the scheduler
	// charges it for every request.
	req := scheduler.Request{
		WeightBytes: meta.WeightBytes, HeadCount: estimate.HeadCount(meta.Config),
		DiskNeedBytes: meta.WeightBytes, AllowedTPs: []int{1}, GPUMemUtil: 0.9,
	}
	fit := scheduler.Schedule(req, scheduler.NodeSnapshot{UUID: "gpu-24g", DtypeSupported: true, DiskAvailBytes: 10 << 30, GPUs: []scheduler.GPU{{Index: 0, MemoryTotal: 24 << 30, Schedulable: true}}})
	if !fit.OK {
		t.Fatalf("24 GiB node rejected fake model: %+v; explain=%+v", fit.Message, fit.Explain)
	}
	if fit.Plan == nil || fit.Plan.TP != 1 {
		t.Fatalf("unexpected plan: %+v", fit.Plan)
	}
	// This model has no catalog entry, so no engine produced a KV cost for it.
	// The pool is still sized - the startup requirement and the fit verdict do
	// not depend on the KV layout - but no token capacity is claimed, because a
	// derived one is what the catalog figures replaced.
	breakdown := estimate.Compute(estimate.Params{Config: meta.Config, WeightBytes: meta.WeightBytes, KVDtype: "bf16", MaxModelLen: 40960, TP: 1, GPUCapacityBytes: 24 << 30, GPUMemUtil: 0.9})
	if breakdown.KVPoolBytes <= 0 || !breakdown.Fits {
		t.Fatalf("fixed admission/KV capacity breakdown = %+v", breakdown)
	}
	if breakdown.EstimatedKVTokens != 0 || breakdown.KVEstimateReason == "" {
		t.Fatalf("unmeasured model claimed a token capacity: %+v", breakdown)
	}
	// The same model with the catalog's figure does report one.
	measured := estimate.Compute(estimate.Params{Config: meta.Config, WeightBytes: meta.WeightBytes, KVDtype: "bf16", KVBytesPerToken: 262144, MaxModelLen: 40960, TP: 1, GPUCapacityBytes: 24 << 30, GPUMemUtil: 0.9})
	if measured.EstimatedKVTokens <= 0 || measured.KVEstimateConfidence != "measured" {
		t.Fatalf("measured model reported no token capacity: %+v", measured)
	}
	tooSmall := scheduler.Schedule(req, scheduler.NodeSnapshot{UUID: "gpu-10g", DtypeSupported: true, DiskAvailBytes: 10 << 30, GPUs: []scheduler.GPU{{Index: 0, MemoryTotal: 10 << 30, Schedulable: true}}})
	if tooSmall.OK {
		t.Fatalf("16 GiB node unexpectedly accepted fake model: %+v", tooSmall.Plan)
	}
	if len(tooSmall.Explain.Rejected) == 0 && len(tooSmall.Explain.Filters) == 0 {
		t.Fatalf("no schedule explanation for rejection: %+v", tooSmall)
	}
	if req.DiskNeedBytes != hub.WeightBytes {
		t.Fatalf("disk reservation = %d, want declared %d", req.DiskNeedBytes, hub.WeightBytes)
	}
}
