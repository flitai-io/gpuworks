package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// The public estimate must work on a fresh node, without image identity or
// earlier starts, and remain independent of the health-check policy.
func TestStartupEstimateUsesReferenceFormulaWithoutHistory(t *testing.T) {
	env, model, nodeID := startupEstimateFixture(t)
	raw := createStartupEstimateDeployment(t, env, model.ID, nodeID, "reference-formula", map[string]any{"start_timeout_sec": 120})
	estimate, ok := raw["startup_estimate"].(map[string]any)
	if !ok {
		t.Fatalf("missing estimate: %+v", raw)
	}
	want := map[string]any{"method": "weight_reference", "weight_bytes": float64(1 << 30), "estimated_seconds": float64(302), "reference_mib_per_sec": float64(512), "initialization_seconds": float64(300)}
	if !reflect.DeepEqual(estimate, want) {
		t.Fatalf("estimate=%+v want=%+v", estimate, want)
	}
	if raw["start_timeout_sec"] != float64(120) || raw["start_timeout_source"] != "deployment" {
		t.Fatalf("estimate changed explicit budget: %+v", raw)
	}
	response, err := env.API(t).PostJSON("/api/v1/evaluate", []byte(fmt.Sprintf(`{"model_id":%d,"node_uuid":%q,"engine":"vllm","image":"vllm/vllm-openai:v0.11.0","quick_params":{"max_model_len":512},"start_timeout_sec":120}`, model.ID, nodeID)))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var preview map[string]any
	if err = json.NewDecoder(response.Body).Decode(&preview); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("evaluate status=%d err=%v body=%+v", response.StatusCode, err, preview)
	}
	if !reflect.DeepEqual(preview["startup_estimate"], want) {
		t.Fatalf("preview estimate differs: %+v", preview)
	}
}

func TestStartupEstimateSelectsInitializationFromFinalEngineOverHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, engine, image, extra string
		initialization, estimated  float64
	}{
		{name: "omni", engine: "vllm", image: "vllm/vllm-openai:v0.11.0", extra: "--omni", initialization: 420, estimated: 422},
		{name: "sglang", engine: "sglang", image: "lmsysorg/sglang:v0.5.2", initialization: 240, estimated: 242},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, model, nodeID := startupEstimateFixture(t)
			if _, err := env.Store().UpsertImage(&store.Image{Name: "startup-profile-" + tc.name, ImageRef: tc.image, Source: "custom", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			changes := map[string]any{"engine": tc.engine, "image": tc.image, "extra_args": tc.extra, "start_timeout_sec": 120}
			raw := createStartupEstimateDeployment(t, env, model.ID, nodeID, "reference-"+tc.name, changes)
			want := map[string]any{"method": "weight_reference", "weight_bytes": float64(1 << 30), "estimated_seconds": tc.estimated, "reference_mib_per_sec": float64(512), "initialization_seconds": tc.initialization}
			if !reflect.DeepEqual(raw["startup_estimate"], want) || raw["start_timeout_sec"] != float64(120) || raw["start_timeout_source"] != "deployment" {
				t.Fatalf("instance reference/budget=%+v want=%+v", raw, want)
			}
			request := map[string]any{"model_id": model.ID, "node_uuid": nodeID, "engine": tc.engine, "image": tc.image, "extra_args": tc.extra, "quick_params": map[string]any{"max_model_len": 512}, "start_timeout_sec": 120}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			response, err := env.API(t).PostJSON("/api/v1/evaluate", body)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var preview map[string]any
			if err := json.NewDecoder(response.Body).Decode(&preview); err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("preview status=%d err=%v", response.StatusCode, err)
			}
			if !reflect.DeepEqual(preview["startup_estimate"], want) || preview["start_timeout_sec"] != float64(120) {
				t.Fatalf("preview reference/budget=%+v want=%+v", preview, want)
			}
		})
	}
}

func startupEstimateFixture(t *testing.T) (*testenv.Env, *store.Model, string) {
	t.Helper()
	env := testenv.New(t)
	node := env.AddNode("startup-estimate", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/startup-estimate", Name: "startup-estimate", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	modelDir := testenv.ModelDir(t, "startup-estimate")
	if err = env.Store().SetStorageNodePath(env.Store().DefaultLocalStorageID(), nodes[0].ID, filepath.Dir(modelDir)); err != nil {
		t.Fatal(err)
	}
	_, err = env.Store().UpsertImage(&store.Image{Name: "startup-estimate-image", ImageRef: "vllm/vllm-openai:v0.11.0", Source: "custom", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: modelDir}); err != nil {
		t.Fatal(err)
	}
	return env, model, nodes[0].UUID
}

func createStartupEstimateDeployment(t *testing.T, env *testenv.Env, modelID uint, nodeUUID, name string, changes ...map[string]any) map[string]any {
	t.Helper()
	api := env.API(t)
	request := map[string]any{"model_id": modelID, "node_uuid": nodeUUID, "engine": "vllm", "image": "vllm/vllm-openai:v0.11.0", "served_name": name, "quick_params": map[string]any{"max_model_len": 512}}
	for _, override := range changes {
		for key, value := range override {
			request[key] = value
		}
	}
	body, _ := json.Marshal(request)
	response, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("POST deployment: %d %s err=%v", response.StatusCode, raw, err)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err = json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response, err = api.Get(fmt.Sprintf("/api/v1/deployments/%d", created.ID))
		if err != nil {
			t.Fatal(err)
		}
		raw, err = io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("GET deployment: %d %s err=%v", response.StatusCode, raw, err)
		}
		var current struct {
			Instances []map[string]any `json:"instances"`
		}
		if err = json.Unmarshal(raw, &current); err != nil {
			t.Fatal(err)
		}
		if len(current.Instances) == 1 {
			return current.Instances[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("instance was not scheduled: %s", raw)
	return nil
}
