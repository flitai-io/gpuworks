package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func deploymentFlatFixture() store.InferenceModel {
	return store.InferenceModel{
		ModelID: "acme/deploy", DisplayTitle: "Deploy", Source: "upstream", Architecture: "qwen3", ContextLength: 32768,
		Unsupported: `[{"gpus":{"vendor":"nvidia","cc":"<8.9"},"quantization":"fp8","reason":"FP8 requires Ada or newer"}]`,
		Specs: []store.InferenceSpecRow{{
			Name: "fp8-balanced", Scenario: "balanced", Engine: "vllm", Image: "vllm/vllm-openai:latest", Quantization: "fp8", WeightBytes: 16 << 30, RuntimeWeightBytes: 8 << 30,
			WeightSources: `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"},{"provider":"huggingface","repo_id":"Acme/Deploy-FP8"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=8.9"}`, Args: `["--max-model-len","32768"]`, Env: `{"CATALOG":"yes"}`,
			PreStartArgv: `["prepare-tool","--safe"]`,
		}},
	}
}

// TestEvaluateKeepsQuotedJSONExtraArgsInArgv replays the deploy-wizard payload
// for a speculative-draft template: the wizard prefills the extra-args box with
// the remaining template arguments single-quoted, so the server's
// ParseArgsText reparse must land the JSON blob in argv verbatim. When the
// wizard still emitted the bare join, this round trip stripped the double
// quotes and vLLM rejected --speculative-config.
// The preview falls back to the catalog spec's weight total for a legacy model
// row that has none, and the evaluate handler is what persists it: the write
// used to happen inside the estimator, which made planning a writer.
func TestEvaluatePersistsTheCatalogWeightItPreviewedWith(t *testing.T) {
	st, err := store.Open("file:api_evaluate_weight_backfill?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "Acme/Deploy-FP8", Revision: "main", Name: "acme/deploy",
		WeightBytes: 0, Meta: `{"model_type":"qwen3","num_hidden_layers":2,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("weight-backfill-node", store.HashToken("token"), "weight-backfill-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("weight-backfill-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "weight-backfill-node", GPUVendor: "nvidia", Framework: "cuda"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","model_id":%d,"engine":"vllm","node_uuid":"weight-backfill-node","placement":{}}`, template.ID, model.ID)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	stored, err := st.GetModel(model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WeightBytes != fixture.Specs[0].WeightBytes {
		t.Fatalf("stored weight = %d, want the catalog figure %d", stored.WeightBytes, fixture.Specs[0].WeightBytes)
	}
}

func TestEvaluateKeepsQuotedJSONExtraArgsInArgv(t *testing.T) {
	st, err := store.Open("file:api_quoted_json_extra_args?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.Specs[0].Name = "nvfp4-speculative-draft"
	fixture.Specs[0].Args = `["--load-format","fastsafetensors","--speculative-config",` + `"{\"model\":\"/models/draft\",\"num_speculative_tokens\":4}"` + `,"--enable-auto-tool-choice","--tool-call-parser","gemma4","--reasoning-parser","gemma4"]`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"nvfp4-speculative-draft","engine":"vllm","node_uuid":"","extra_args":"--load-format\nfastsafetensors\n--speculative-config\n'{\"model\":\"/models/draft\",\"num_speculative_tokens\":4}'\n--enable-auto-tool-choice","quick_params":{"reasoning_parser":"gemma4","tool_call_parser":"gemma4"},"placement":{}}`, template.ID)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var got dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	const want = `{"model":"/models/draft","num_speculative_tokens":4}`
	for index, token := range got.Argv {
		if token == "--speculative-config" {
			if index+1 >= len(got.Argv) || got.Argv[index+1] != want {
				t.Fatalf("argv=%#v: value %q, want %q", got.Argv, got.Argv[index+1:index+2], want)
			}
			return
		}
	}
	t.Fatalf("argv=%#v has no --speculative-config flag", got.Argv)
}
