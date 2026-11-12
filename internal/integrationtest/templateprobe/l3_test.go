package templateprobe_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/templateprobe"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestL3HarnessUsesRealDeploymentControllerAndGateway(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("l3-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "l3-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "gpuworks/l3-fixture", Revision: "main", Name: "l3-fixture",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "l3-fixture"),
	}); err != nil {
		t.Fatal(err)
	}
	const runtimeImage = "gpuworks.test/l3-runtime:v1"
	// Match the present ledger row to the runtime before publishing the ref triggers inventory.
	if err := node.Driver.PullImage(context.Background(), runtimeImage, nil); err != nil {
		t.Fatal(err)
	}
	image, err := env.Store().UpsertImage(&store.Image{
		ImageRef: runtimeImage, Engine: "vllm", Source: "community",
		Framework: "cuda", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertImageNode(&store.ImageNode{
		ImageID: image.ID, NodeID: nodes[0].ID, State: store.ImageStatePresent, ImageDigestID: "sha256:l3-integration",
	}); err != nil {
		t.Fatal(err)
	}
	management := env.API(t)
	apiKey, err := env.Store().CreateAPIKey("l3-harness", 0)
	if err != nil {
		t.Fatal(err)
	}
	deployer, err := templateprobe.NewHTTPDeploymentClient(env.URL(), management.Token, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	deployer.PollInterval = 25 * time.Millisecond
	payload, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "name": "l3-harness",
		"served_name": "l3-harness", "engine": "vllm", "image": image.ImageRef,
		"quick_params": map[string]any{"max_model_len": 256},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, err := (templateprobe.L3Runner{
		Deployer:       deployer,
		Evaluator:      templateprobe.HTTPTaskEvaluator{Client: &http.Client{}, APIKey: apiKey},
		CleanupTimeout: 15 * time.Second,
	}).Run(ctx, templateprobe.L3Input{
		Deployment: payload,
		Case: templateprobe.L3Case{
			CaseID: "l3-controller-gateway", Task: templateprobe.L3TaskText,
			Prompt: "Return a greeting", ExpectedKeywords: []string{"hello"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != templateprobe.L3StatusPass || !result.PlatformCompatible || !result.Cleanup.Completed {
		t.Fatalf("result = %+v", result)
	}
	deployments, _ := env.Store().ListDeployments()
	instances, _ := env.Store().ListInstances()
	if len(deployments) != 0 || len(instances) != 0 {
		t.Fatalf("temporary resources remain: deployments=%+v instances=%+v", deployments, instances)
	}
}
