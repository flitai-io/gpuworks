package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// A long engine deadline is an operator's supported startup configuration,
// not a measured ETA. Its outer health budget must leave the engine time to
// answer, and the exact chosen budget must be visible over the public API.
func TestDeploymentLongEngineDeadlineGetsMatchingHealthBudget(t *testing.T) {
	exerciseLongEngineDeadline(t, "")
}

func TestTemplateWrapperDeadlineGetsMatchingHealthBudget(t *testing.T) {
	// The catalog's Ascend wrapper sources its toolkit before replacing the
	// shell with vLLM; its explicit engine deadline still has the same meaning.
	entrypoint, _ := json.Marshal([]string{"/bin/bash", "-c", `for script in /usr/local/Ascend/ascend-toolkit/set_env.sh /usr/local/Ascend/nnal/atb/set_env.sh; do [ -f "$script" ] && source "$script"; done; exec vllm serve "$@"`, "--"})
	exerciseLongEngineDeadline(t, string(entrypoint))
}

func exerciseLongEngineDeadline(t *testing.T, templateEntrypoint string) {
	t.Helper()
	env := testenv.New(t)
	node := env.AddNode("startup-budget", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/startup-budget", Revision: "main", Name: "startup-budget", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "startup-budget")}); err != nil {
		t.Fatal(err)
	}
	if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "fixture/startup-budget", DisplayTitle: "Startup budget", Source: "upstream", Architecture: "qwen3", ContextLength: 2048,
		Specs: []store.InferenceSpecRow{{Name: "long-start", Scenario: "balanced", Engine: "vllm", Image: "vllm/vllm-openai:v0.11.0", Quantization: "bf16", WeightBytes: 1 << 30,
			WeightSources: `[{"provider":"huggingface","repo_id":"fixture/startup-budget","revision":"main"}]`, GPUs: `{"vendor":"nvidia","cc":">=8.0"}`,
			Env: `{"VLLM_ENGINE_READY_TIMEOUT_S":"36000"}`, Entrypoint: templateEntrypoint}},
	}}); err != nil {
		t.Fatal(err)
	}
	template, err := env.Store().InferenceModelByModelID("fixture/startup-budget")
	if err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "engine": "vllm", "image": "vllm/vllm-openai:v0.11.0", "served_name": "startup-budget", "quick_params": map[string]any{"max_model_len": 512}, "template_id": template.ID, "template_spec_source": "upstream", "template_spec_name": "long-start"})
	response, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.StatusCode, raw)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	var current struct {
		Instances []struct {
			State           string            `json:"state"`
			StartTimeoutSec int               `json:"start_timeout_sec"`
			Source          string            `json:"start_timeout_source"`
			Conflict        bool              `json:"start_timeout_conflict"`
			Env             map[string]string `json:"env"`
		} `json:"instances"`
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response, err = api.Get(fmt.Sprintf("/api/v1/deployments/%d", created.ID))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("get status=%d body=%s", response.StatusCode, raw)
		}
		if err := json.Unmarshal(raw, &current); err != nil {
			t.Fatal(err)
		}
		if len(current.Instances) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(current.Instances) != 1 {
		t.Fatalf("no scheduled instance: %s", raw)
	}
	inst := current.Instances[0]
	if inst.StartTimeoutSec != 36030 || inst.Source != "engine" || inst.Conflict || inst.Env["VLLM_ENGINE_READY_TIMEOUT_S"] != "36000" {
		t.Fatalf("long engine deadline was not coordinated: %s", raw)
	}

	// An explicit platform budget is a deliberate bound; evaluation explains
	// its conflict instead of silently replacing it with the engine deadline.
	var previewRequest map[string]any
	if err := json.Unmarshal(body, &previewRequest); err != nil {
		t.Fatal(err)
	}
	previewRequest["start_timeout_sec"] = 3600
	previewBody, _ := json.Marshal(previewRequest)
	response, err = api.PostJSON("/api/v1/evaluate", previewBody)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(response.Body)
	response.Body.Close()
	var preview struct {
		StartTimeoutSec int               `json:"start_timeout_sec"`
		Source          string            `json:"start_timeout_source"`
		Conflict        bool              `json:"start_timeout_conflict"`
		EngineSeconds   int               `json:"engine_start_timeout_sec"`
		Env             map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &preview); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || preview.StartTimeoutSec != 3600 || preview.Source != "deployment" || !preview.Conflict || preview.EngineSeconds != 36000 || preview.Env["VLLM_ENGINE_READY_TIMEOUT_S"] != "36000" {
		t.Fatalf("explicit preview budget did not explain the conflict: %s", raw)
	}
}

func TestStartupBudgetRespectsExplicitEngineEnvRemoval(t *testing.T) {
	api, body := startupBudgetPreviewFixture(t)
	body["container_overrides"] = map[string]any{"env_unset": []string{"VLLM_ENGINE_READY_TIMEOUT_S"}}
	wire := startupBudgetEvaluate(t, api, body)
	if wire["start_timeout_sec"] != float64(1800) || wire["start_timeout_source"] != "cluster" {
		t.Fatalf("unexpected default budget: %v", wire)
	}
	env, _ := wire["env"].(map[string]any)
	if _, present := env["VLLM_ENGINE_READY_TIMEOUT_S"]; present {
		t.Fatalf("explicit env removal was replaced by an automatic value: %v", env)
	}
	if _, present := wire["engine_start_timeout_sec"]; present {
		t.Fatalf("removed engine env was reported as observed: %v", wire)
	}
}

func startupBudgetPreviewFixture(t *testing.T) (*testenv.APIClient, map[string]any) {
	t.Helper()
	env := testenv.New(t)
	node := env.AddNode("startup-preview", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/startup-preview", Name: "startup-preview", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "startup-preview")}); err != nil {
		t.Fatal(err)
	}
	return env.API(t), map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "engine": "vllm", "image": "vllm/vllm-openai:v0.11.0", "served_name": "startup-preview", "quick_params": map[string]any{"max_model_len": 512}}
}

func startupBudgetEvaluate(t *testing.T, api *testenv.APIClient, body map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := api.PostJSON("/api/v1/evaluate", encoded)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("evaluate=%d body=%s err=%v", response.StatusCode, raw, err)
	}
	return out
}

func TestStartupBudgetPreviewDefaultsBoundsAndCustomCommands(t *testing.T) {
	api, original := startupBudgetPreviewFixture(t)
	for _, tc := range []struct {
		name                string
		explicit            int
		overrides           map[string]any
		wantBudget          int
		wantSource, wantEnv string
		wantConflict        bool
	}{
		{name: "default coordinates the managed engine", wantBudget: 1800, wantSource: "cluster", wantEnv: "1770"},
		{name: "deployment budget coordinates the managed engine", explicit: 3600, wantBudget: 3600, wantSource: "deployment", wantEnv: "3570"},
		{name: "explicit engine value is retained", overrides: map[string]any{"env": map[string]string{"VLLM_ENGINE_READY_TIMEOUT_S": "600"}}, wantBudget: 1800, wantSource: "cluster", wantEnv: "600"},
		{name: "over policy ceiling is visible as a conflict", overrides: map[string]any{"env": map[string]string{"VLLM_ENGINE_READY_TIMEOUT_S": "100000"}}, wantBudget: 86400, wantSource: "engine", wantEnv: "100000", wantConflict: true},
		{name: "manual executable is not assumed to be vllm", overrides: map[string]any{"command_mode": "manual", "cmd_display": "[\"custom-server\"]"}, wantBudget: 1800, wantSource: "cluster"},
		{name: "replaced executable is not assumed to be vllm", overrides: map[string]any{"entrypoint_display": "[\"custom-server\"]"}, wantBudget: 1800, wantSource: "cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{}
			for k, v := range original {
				body[k] = v
			}
			if tc.explicit > 0 {
				body["start_timeout_sec"] = tc.explicit
			}
			if tc.overrides != nil {
				body["container_overrides"] = tc.overrides
			}
			wire := startupBudgetEvaluate(t, api, body)
			if errors, ok := wire["errors"].([]any); ok && len(errors) > 0 {
				t.Fatalf("unexpected validation errors: %v", wire)
			}
			if wire["start_timeout_sec"] != float64(tc.wantBudget) || wire["start_timeout_source"] != tc.wantSource {
				t.Fatalf("budget=%v source=%v want %d %s (%v)", wire["start_timeout_sec"], wire["start_timeout_source"], tc.wantBudget, tc.wantSource, wire)
			}
			conflict, _ := wire["start_timeout_conflict"].(bool)
			if conflict != tc.wantConflict {
				t.Fatalf("conflict=%v want=%v", conflict, tc.wantConflict)
			}
			env, _ := wire["env"].(map[string]any)
			value, _ := env["VLLM_ENGINE_READY_TIMEOUT_S"].(string)
			if value != tc.wantEnv {
				t.Fatalf("engine env=%q want=%q", value, tc.wantEnv)
			}
		})
	}
}

func TestStartupBudgetRejectsInvalidEngineDeadlinesBeforeCreatingDeployment(t *testing.T) {
	api, original := startupBudgetPreviewFixture(t)
	for _, value := range []string{"", "0", "-1", "1.5", " 3600", "99999999999999999999999999"} {
		t.Run(fmt.Sprintf("value=%q", value), func(t *testing.T) {
			body := map[string]any{}
			for k, v := range original {
				body[k] = v
			}
			body["container_overrides"] = map[string]any{"env": map[string]string{"VLLM_ENGINE_READY_TIMEOUT_S": value}}
			wire := startupBudgetEvaluate(t, api, body)
			errors, _ := wire["errors"].([]any)
			if wire["ok"] != false || len(errors) == 0 {
				t.Fatalf("invalid deadline accepted in preview: %v", wire)
			}
			encoded, _ := json.Marshal(body)
			response, err := api.PostJSON("/api/v1/deployments", encoded)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid deadline accepted by create: status=%d body=%s", response.StatusCode, raw)
			}
		})
	}
}
