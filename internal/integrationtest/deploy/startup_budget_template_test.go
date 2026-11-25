package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// A template spec's start_timeout_sec replaces the cluster default for a
// recipe with a long first start: the preview and the scheduled instance both
// show the template as the budget's source, a deployment's own value still
// wins, an explicit engine deadline still raises it, and the deployment keeps
// the value it was created with when the catalog later changes.
func TestTemplateStartTimeoutIsTheBudgetBetweenClusterAndDeployment(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("template-budget", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/template-budget", Revision: "main", Name: "template-budget", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "template-budget")}); err != nil {
		t.Fatal(err)
	}
	catalog := func(seconds int) {
		t.Helper()
		if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{{
			ModelID: "fixture/template-budget", DisplayTitle: "Template budget", Source: "upstream", Architecture: "qwen3", ContextLength: 2048,
			Specs: []store.InferenceSpecRow{{Name: "long-first-start", Scenario: "balanced", Engine: "vllm", Image: "vllm/vllm-openai:v0.11.0", Quantization: "bf16", WeightBytes: 1 << 30,
				WeightSources: `[{"provider":"huggingface","repo_id":"fixture/template-budget","revision":"main"}]`, GPUs: `{"vendor":"nvidia","cc":">=8.0"}`,
				StartTimeoutSec: seconds}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	catalog(5400)
	template, err := env.Store().InferenceModelByModelID("fixture/template-budget")
	if err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	request := func() map[string]any {
		return map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "engine": "vllm", "image": "vllm/vllm-openai:v0.11.0", "served_name": "template-budget",
			"quick_params": map[string]any{"max_model_len": 512}, "template_id": template.ID, "template_spec_source": "upstream", "template_spec_name": "long-first-start"}
	}

	for _, tc := range []struct {
		name       string
		explicit   int
		engineEnv  string
		wantBudget int
		wantSource string
		wantEnv    string
	}{
		{name: "template replaces the cluster default", wantBudget: 5400, wantSource: "template", wantEnv: "5370"},
		{name: "deployment value wins over the template", explicit: 3600, wantBudget: 3600, wantSource: "deployment", wantEnv: "3570"},
		{name: "longer engine deadline raises the template value", engineEnv: "7200", wantBudget: 7230, wantSource: "engine", wantEnv: "7200"},
		{name: "shorter engine deadline leaves the template value", engineEnv: "600", wantBudget: 5400, wantSource: "template", wantEnv: "600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := request()
			if tc.explicit > 0 {
				body["start_timeout_sec"] = tc.explicit
			}
			if tc.engineEnv != "" {
				body["container_overrides"] = map[string]any{"env": map[string]string{"VLLM_ENGINE_READY_TIMEOUT_S": tc.engineEnv}}
			}
			wire := startupBudgetEvaluate(t, api, body)
			if errors, ok := wire["errors"].([]any); ok && len(errors) > 0 {
				t.Fatalf("unexpected validation errors: %v", wire)
			}
			if wire["start_timeout_sec"] != float64(tc.wantBudget) || wire["start_timeout_source"] != tc.wantSource {
				t.Fatalf("budget=%v source=%v want %d %s", wire["start_timeout_sec"], wire["start_timeout_source"], tc.wantBudget, tc.wantSource)
			}
			env, _ := wire["env"].(map[string]any)
			if value, _ := env["VLLM_ENGINE_READY_TIMEOUT_S"].(string); value != tc.wantEnv {
				t.Fatalf("engine env=%q want=%q", value, tc.wantEnv)
			}
		})
	}

	encoded, _ := json.Marshal(request())
	response, err := api.PostJSON("/api/v1/deployments", encoded)
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
	type instanceBudget struct {
		StartTimeoutSec int    `json:"start_timeout_sec"`
		Source          string `json:"start_timeout_source"`
	}
	instances := func() []instanceBudget {
		t.Helper()
		var current struct {
			Instances []instanceBudget `json:"instances"`
		}
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			response, err := api.Get(fmt.Sprintf("/api/v1/deployments/%d", created.ID))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if err := json.Unmarshal(raw, &current); err != nil {
				t.Fatal(err)
			}
			if len(current.Instances) > 0 {
				return current.Instances
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("no scheduled instance")
		return nil
	}
	if got := instances(); len(got) != 1 || got[0].StartTimeoutSec != 5400 || got[0].Source != "template" {
		t.Fatalf("instance budget = %+v, want 5400 from the template", got)
	}

	// The deployment's snapshot records the value it was created with, so a
	// later catalog change does not move a running deployment's budget.
	catalog(900)
	dep, err := env.Store().GetDeployment(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := deploy.DecodeCatalogSelection([]byte(dep.CatalogSelectionSnapshot))
	if err != nil || selection.StartTimeoutSec != 5400 {
		t.Fatalf("snapshot start_timeout_sec = %d err=%v", selection.StartTimeoutSec, err)
	}
}
