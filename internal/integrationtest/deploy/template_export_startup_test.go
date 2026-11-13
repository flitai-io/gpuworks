package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// Startup policy edits apply without replacing the running container. Export
// must replay its recorded environment while still refusing pending launch edits.
func TestTemplateExportKeepsTheRunningStartupBudgetThroughHTTP(t *testing.T) {
	exerciseTemplateExportStartup(t, 0, false, true)
}

func TestTemplateExportRespectsExplicitStartupChoicesThroughHTTP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timeout   int
		removeEnv bool
	}{
		{name: "explicit deployment budget", timeout: 120},
		{name: "explicit engine environment removal", removeEnv: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exerciseTemplateExportStartup(t, tc.timeout, tc.removeEnv, false)
		})
	}
}

func exerciseTemplateExportStartup(t *testing.T, timeout int, removeEnv, editPolicies bool) {
	t.Helper()
	env := testenv.New(t)
	node := env.AddNode("template-budget", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, node.Name)
	storedNode, err := env.Store().NodeByUUID(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	modelDir := testenv.ModelDir(t, "template-budget")
	if err := env.Store().SetStorageNodePath(env.Store().DefaultLocalStorageID(), storedNode.ID, filepath.Dir(modelDir)); err != nil {
		t.Fatal(err)
	}
	const commit = "48d788eca847d4d7548f375ad03d3c9312f6139e"
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/startup-budget", Revision: commit, Name: "template-budget",
		Arch: "Qwen3ForCausalLM", Precision: "bf16", WeightBytes: 1 << 20,
		Meta:       `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
		Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: storedNode.ID, State: proto.MFPresent, Path: modelDir, Revision: commit, SizeBytes: model.WeightBytes}); err != nil {
		t.Fatal(err)
	}
	client := env.API(t)
	call := func(method, path string, body any, want int) []byte {
		t.Helper()
		var encoded []byte
		if body != nil {
			var err error
			encoded, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		request, err := http.NewRequest(method, path, strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %s (read error %v)", method, path, response.StatusCode, want, raw, err)
		}
		return raw
	}
	var created struct {
		ID uint `json:"id"`
	}
	createBody := map[string]any{
		"name": "template-budget", "model_id": model.ID, "node_uuid": node.UUID,
		"engine": "vllm", "image": "example/template-budget:v1", "served_name": "template-budget",
		"quick_params": map[string]any{"max_model_len": 512}, "extra_args": "--seed 7",
	}
	if timeout > 0 {
		createBody["start_timeout_sec"] = timeout
	}
	if removeEnv {
		createBody["container_overrides"] = map[string]any{"env_unset": []string{"VLLM_ENGINE_READY_TIMEOUT_S"}}
	}
	if err := json.Unmarshal(call(http.MethodPost, "/api/v1/deployments", createBody, http.StatusCreated), &created); err != nil || created.ID == 0 {
		t.Fatalf("created=%+v: %v", created, err)
	}
	deploymentPath := fmt.Sprintf("/api/v1/deployments/%d", created.ID)
	type instanceState struct {
		UUID            string            `json:"uuid"`
		State           string            `json:"state"`
		StartTimeoutSec int               `json:"start_timeout_sec"`
		Env             map[string]string `json:"env"`
	}
	readInstance := func() instanceState {
		t.Helper()
		var detail struct {
			Instances []instanceState `json:"instances"`
		}
		if err := json.Unmarshal(call(http.MethodGet, deploymentPath, nil, http.StatusOK), &detail); err != nil {
			t.Fatal(err)
		}
		if len(detail.Instances) == 0 {
			return instanceState{}
		}
		if len(detail.Instances) != 1 {
			t.Fatalf("unexpected instances: %+v", detail.Instances)
		}
		return detail.Instances[0]
	}
	deadline := time.Now().Add(30 * time.Second)
	var original instanceState
	for {
		original = readInstance()
		if original.State == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment did not become running: %+v", original)
		}
		time.Sleep(25 * time.Millisecond)
	}
	wantTimeout, wantEnv := 1800, "1770"
	if timeout > 0 {
		wantTimeout, wantEnv = timeout, fmt.Sprint(timeout-30)
	}
	if removeEnv {
		wantEnv = ""
	}
	if original.StartTimeoutSec != wantTimeout || original.Env["VLLM_ENGINE_READY_TIMEOUT_S"] != wantEnv {
		t.Fatalf("initial managed startup budget=%+v", original)
	}
	if _, present := original.Env["VLLM_ENGINE_READY_TIMEOUT_S"]; removeEnv && present {
		t.Fatalf("explicit environment removal was restored in the running launch: %+v", original)
	}
	assertUnchanged := func() {
		t.Helper()
		current := readInstance()
		if current.UUID != original.UUID || current.State != "running" || !reflect.DeepEqual(current.Env, original.Env) || current.StartTimeoutSec != original.StartTimeoutSec {
			t.Fatalf("policy edit replaced or changed the running launch: before=%+v after=%+v", original, current)
		}
	}
	export := func(stage string) {
		t.Helper()
		t.Log(stage)
		var result struct {
			Saveable bool   `json:"saveable"`
			YAML     string `json:"yaml"`
		}
		if err := json.Unmarshal(call(http.MethodPost, deploymentPath+"/catalog-template", map[string]any{}, http.StatusOK), &result); err != nil {
			t.Fatal(err)
		}
		if !result.Saveable || !strings.Contains(result.YAML, "huggingface:") {
			t.Fatalf("%s did not produce a reusable template: %+v", stage, result)
		}
		if removeEnv && strings.Contains(result.YAML, "VLLM_ENGINE_READY_TIMEOUT_S") {
			t.Fatalf("export restored an explicitly removed environment variable: %s", result.YAML)
		}
	}
	export("initial startup budget")
	if !editPolicies {
		return
	}
	call(http.MethodPut, "/api/v1/settings/deployment", map[string]any{"start_timeout_sec": 900}, http.StatusOK)
	assertUnchanged()
	export("changed cluster default keeps the recorded startup budget")
	edit := func(mutate func(map[string]any)) map[string]any {
		t.Helper()
		var draft struct {
			Deployment map[string]any `json:"deployment"`
			Revision   string         `json:"revision"`
		}
		if err := json.Unmarshal(call(http.MethodGet, deploymentPath+"/draft", nil, http.StatusOK), &draft); err != nil {
			t.Fatal(err)
		}
		mutate(draft.Deployment)
		var result map[string]any
		if err := json.Unmarshal(call(http.MethodPut, deploymentPath, map[string]any{
			"deployment": draft.Deployment, "base_revision": draft.Revision, "apply": "none",
		}, http.StatusOK), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	live := edit(func(draft map[string]any) { draft["start_timeout_sec"] = 600 })
	plan, _ := live["plan"].(map[string]any)
	if plan["restart"] != false {
		t.Fatalf("startup timeout edit should be live: %+v", live)
	}
	assertUnchanged()
	export("live deployment timeout edit keeps the recorded startup budget")
	edit(func(draft map[string]any) { draft["extra_args"] = "--seed 9" })
	assertUnchanged()
	var refusal struct {
		Code   string `json:"code"`
		Params struct {
			Reason string `json:"reason"`
		} `json:"params"`
	}
	if err := json.Unmarshal(call(http.MethodPost, deploymentPath+"/catalog-template", map[string]any{}, http.StatusConflict), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Code != "TEMPLATE_EXPORT_UNAVAILABLE" || refusal.Params.Reason != "launch_changed" {
		t.Fatalf("pending engine arguments did not produce the stable refusal: %+v", refusal)
	}
}
