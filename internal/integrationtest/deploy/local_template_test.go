package deploy

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv"
)

// A model registered from an existing directory has no public repository.
// Saving its working launch must still produce a reusable local template.
func TestLocalModelCanBeSavedAndReusedThroughHTTP(t *testing.T) {
	for _, sourceID := range []string{"company-tuned-model", "Company Tuned Model", "公司模型:微调"} {
		t.Run(sourceID, func(t *testing.T) {
			env := testenv.New(t)
			node := env.AddNode("local-template-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
			env.WaitNodeOnline(t, node.Name)
			client := env.API(t)
			call := func(method, path, contentType string, body []byte, want int) []byte {
				t.Helper()
				req, err := http.NewRequest(method, path, strings.NewReader(string(body)))
				if err != nil {
					t.Fatal(err)
				}
				if contentType != "" {
					req.Header.Set("Content-Type", contentType)
				}
				response, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != want {
					t.Fatalf("%s %s = %d, want %d: %s (read error %v)", method, path, response.StatusCode, want, data, err)
				}
				return data
			}
			post := func(path string, body any, want int) []byte {
				t.Helper()
				data, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				return call(http.MethodPost, path, "application/json", data, want)
			}
			dir := testenv.ModelDir(t, strings.ReplaceAll(sourceID, ":", "-"))
			const config = `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"torch_dtype":"bfloat16","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048,"vocab_size":32}`
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			// A real, tiny safetensors file: one BF16 element, rather than a permissive
			// directory fixture whose arbitrary bytes bypass model inspection.
			header := `{"weight":{"dtype":"BF16","shape":[1],"data_offsets":[0,2]}}`
			header += strings.Repeat(" ", (8-len(header)%8)%8)
			weights := make([]byte, 8+len(header)+2)
			binary.LittleEndian.PutUint64(weights, uint64(len(header)))
			copy(weights[8:], header)
			if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), weights, 0o600); err != nil {
				t.Fatal(err)
			}
			var storage, model, original struct {
				ID uint `json:"id"`
			}
			if err := json.Unmarshal(post("/api/v1/storages", map[string]any{
				"name": "local-template-weights", "type": "local", "path": filepath.Dir(dir), "all_nodes": true, "enabled": true,
			}, http.StatusCreated), &storage); err != nil || storage.ID == 0 {
				t.Fatalf("storage = %+v: %v", storage, err)
			}
			registration := map[string]any{"node_uuid": node.UUID, "storage_id": storage.ID, "path": dir, "name": "company-tuned-model"}
			if filepath.Base(dir) != sourceID {
				registration["source_id"] = sourceID
			}
			if err := json.Unmarshal(post("/api/v1/models/local", registration, http.StatusCreated), &model); err != nil || model.ID == 0 {
				t.Fatalf("local model = %+v: %v", model, err)
			}
			if err := json.Unmarshal(post("/api/v1/deployments", map[string]any{
				"name": "working-local-model", "model_id": model.ID, "node_uuid": node.UUID,
				"engine": "vllm", "image": "example/local-template:v1", "served_name": "local-tuned",
				"model_source": map[string]any{"provider": "local", "storage_id": storage.ID},
				"quick_params": map[string]any{"max_model_len": 512, "gpu_memory_utilization": 0.8}, "extra_args": "--seed 7",
			}, http.StatusCreated), &original); err != nil || original.ID == 0 {
				t.Fatalf("original deployment = %+v: %v", original, err)
			}
			waitRunning := func(id uint) string {
				t.Helper()
				deadline := time.Now().Add(30 * time.Second)
				for {
					var detail struct {
						RuntimeState string `json:"runtime_state"`
						Instances    []struct {
							UUID  string `json:"uuid"`
							State string `json:"state"`
						} `json:"instances"`
					}
					if err := json.Unmarshal(call(http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", id), "", nil, http.StatusOK), &detail); err != nil {
						t.Fatal(err)
					}
					if detail.RuntimeState == "running" && len(detail.Instances) == 1 && detail.Instances[0].State == "running" {
						return detail.Instances[0].UUID
					}
					if time.Now().After(deadline) {
						t.Fatalf("local deployment did not become running: %+v", detail)
					}
					time.Sleep(25 * time.Millisecond)
				}
			}
			waitRunning(original.ID)
			var exported struct {
				YAML     string `json:"yaml"`
				Saveable bool   `json:"saveable"`
				ModelID  string `json:"model_id"`
				SpecName string `json:"spec_name"`
			}
			exportResponse := post(fmt.Sprintf("/api/v1/deployments/%d/catalog-template", original.ID), map[string]any{}, http.StatusOK)
			if err := json.Unmarshal(exportResponse, &exported); err != nil {
				t.Fatal(err)
			}
			if sourceID == "company-tuned-model" {
				checkLocalTemplateUIFixture(t, exportResponse)
				var schema map[string]any
				if err := json.Unmarshal(call(http.MethodGet, "/api/v1/inference-templates/schema", "", nil, http.StatusOK), &schema); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"properties", "models", "items", "properties", "specs", "items", "properties", "weights", "properties", "sources"} {
					next, ok := schema[key].(map[string]any)
					if !ok {
						t.Fatalf("weight source schema contract is missing %q", key)
					}
					schema = next
				}
				data, err := json.Marshal(schema)
				if err != nil {
					t.Fatal(err)
				}
				checkLocalTemplateUIFixture(t, data, "weight-source-schema")
			}
			if !exported.Saveable || !strings.Contains(exported.YAML, "local:") || strings.Contains(exported.YAML, dir) {
				t.Fatalf("local export lost its usable identity or leaked the node path: %s", exported.YAML)
			}
			for _, invalid := range []string{"/private/tuned-model", "../tuned-model", "C:/models/tuned-model"} {
				invalidYAML := strings.Replace(exported.YAML, "local: "+sourceID, "local: "+invalid, 1)
				if invalidYAML == exported.YAML {
					t.Fatal("the local source fixture did not expose its registered identity")
				}
				call(http.MethodPost, "/api/v1/inference-templates/import", "application/yaml", []byte(invalidYAML), http.StatusBadRequest)
			}
			unsupportedURL := strings.Replace(exported.YAML, "local: "+sourceID, "url: "+sourceID, 1)
			call(http.MethodPost, "/api/v1/inference-templates/import", "application/yaml", []byte(unsupportedURL), http.StatusBadRequest)
			call(http.MethodPost, "/api/v1/inference-templates/import", "application/yaml", []byte(exported.YAML), http.StatusOK)
			segments := strings.Split(exported.ModelID, "/")
			for i := range segments {
				segments[i] = url.PathEscape(segments[i])
			}
			templatePath := "/api/v1/inference-templates/model/" + strings.Join(segments, "/")
			var saved struct {
				ID uint `json:"id"`
			}
			if err := json.Unmarshal(call(http.MethodGet, templatePath, "", nil, http.StatusOK), &saved); err != nil || saved.ID == 0 {
				t.Fatalf("saved template = %+v: %v", saved, err)
			}
			// A public model can have the same display/repository identity as a private
			// fine-tune. It is not a replacement for the local source the user saved.
			post("/api/v1/models", map[string]any{
				"source": "huggingface", "source_id": sourceID, "revision": "main", "name": "remote-with-same-name",
				"browser_metadata": true, "config_raw": config, "weight_bytes": len(config) + len(weights),
			}, http.StatusCreated)
			query := url.Values{"template_id": {fmt.Sprint(saved.ID)}, "spec_source": {"user"}, "spec_name": {exported.SpecName}}
			var matching struct {
				Models []struct {
					ID     uint   `json:"id"`
					Source string `json:"source"`
				} `json:"models"`
				Sources []struct {
					Provider     string `json:"provider"`
					RepoID       string `json:"repo_id"`
					ReuseModelID uint   `json:"reuse_model_id"`
				} `json:"sources"`
			}
			if err := json.Unmarshal(call(http.MethodGet, "/api/v1/inference-templates/weight-models?"+query.Encode(), "", nil, http.StatusOK), &matching); err != nil {
				t.Fatal(err)
			}
			if len(matching.Models) != 1 || matching.Models[0].ID != model.ID || matching.Models[0].Source != "local" ||
				len(matching.Sources) != 1 || matching.Sources[0].Provider != "local" || matching.Sources[0].RepoID != sourceID || matching.Sources[0].ReuseModelID != model.ID {
				t.Fatalf("saved template did not offer the registered local weights: %+v", matching)
			}
			call(http.MethodDelete, fmt.Sprintf("/api/v1/deployments/%d", original.ID), "", nil, http.StatusAccepted)
			deadline := time.Now().Add(30 * time.Second)
			for {
				response, err := client.Get(fmt.Sprintf("/api/v1/deployments/%d", original.ID))
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode == http.StatusNotFound {
					break
				}
				if response.StatusCode != http.StatusOK || time.Now().After(deadline) {
					t.Fatalf("source deployment was not deleted: HTTP %d", response.StatusCode)
				}
				time.Sleep(25 * time.Millisecond)
			}
			call(http.MethodGet, templatePath, "", nil, http.StatusOK)
			var reused struct {
				ID      uint `json:"id"`
				ModelID uint `json:"model_id"`
			}
			if err := json.Unmarshal(post("/api/v1/deployments", map[string]any{
				"name": "reused-local-model", "template_id": saved.ID, "template_spec_source": "user", "template_spec_name": exported.SpecName,
				"node_uuid": node.UUID, "served_name": "local-tuned", "model_source": map[string]any{"provider": "local", "storage_id": storage.ID},
				"quick_params": map[string]any{},
			}, http.StatusCreated), &reused); err != nil || reused.ID == 0 || reused.ModelID != model.ID {
				t.Fatalf("template did not reuse the local model: %+v, %v", reused, err)
			}
			instanceUUID := waitRunning(reused.ID)
			var facts struct {
				Resolved struct {
					Value struct {
						Argv []string `json:"argv"`
					} `json:"value"`
				} `json:"resolved"`
			}
			if err := json.Unmarshal(call(http.MethodGet, "/api/v1/instances/"+instanceUUID+"/launch-facts", "", nil, http.StatusOK), &facts); err != nil {
				t.Fatal(err)
			}
			for flag, want := range map[string]string{"--seed": "7", "--max-model-len": "512", "--gpu-memory-utilization": "0.8"} {
				got := ""
				for i, token := range facts.Resolved.Value.Argv {
					if token == flag && i+1 < len(facts.Resolved.Value.Argv) {
						got = facts.Resolved.Value.Argv[i+1]
					}
				}
				if got != want {
					t.Fatalf("saved launch %s=%q, want %q; argv=%v", flag, got, want, facts.Resolved.Value.Argv)
				}
			}
			if err := json.Unmarshal(post(fmt.Sprintf("/api/v1/deployments/%d/catalog-template", reused.ID), map[string]any{}, http.StatusOK), &exported); err != nil || !exported.Saveable || !strings.Contains(exported.YAML, "local:") {
				t.Fatalf("the reused local deployment cannot be saved again: %+v, %v", exported, err)
			}

			// Importing a local template into a cluster without its registered weights
			// must explain what is missing, not create a fake downloadable model.
			missingEnv := testenv.New(t)
			missingNode := missingEnv.AddNode("missing-local-weights", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
			missingEnv.WaitNodeOnline(t, missingNode.Name)
			client = missingEnv.API(t)
			call(http.MethodPost, "/api/v1/inference-templates/import", "application/yaml", []byte(exported.YAML), http.StatusOK)
			if err := json.Unmarshal(call(http.MethodGet, templatePath, "", nil, http.StatusOK), &saved); err != nil {
				t.Fatal(err)
			}
			var refused struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(post("/api/v1/deployments", map[string]any{
				"name": "missing-local-model", "template_id": saved.ID, "template_spec_source": "user", "template_spec_name": exported.SpecName,
				"node_uuid": missingNode.UUID, "model_source": map[string]any{"provider": "local"}, "quick_params": map[string]any{},
			}, http.StatusBadRequest), &refused); err != nil || refused.Code != "MODEL_SOURCE_UNAVAILABLE" {
				t.Fatalf("missing local weights did not produce an explicit refusal: %+v, %v", refused, err)
			}
			var registered struct {
				Total int `json:"total"`
			}
			if err := json.Unmarshal(call(http.MethodGet, "/api/v1/models", "", nil, http.StatusOK), &registered); err != nil || registered.Total != 0 {
				t.Fatalf("a missing local source created an empty model: %+v, %v", registered, err)
			}
		})
	}

}
