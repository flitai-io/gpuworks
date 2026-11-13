package deploy

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// estimatePreview decodes the KV part of the deploy form's wire contract by
// literal field names: a rename would silently empty the KV row in the UI.
type estimatePreview struct {
	OK   bool `json:"ok"`
	Plan *struct {
		OK      bool `json:"ok"`
		Message struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		} `json:"message"`
		Explain struct {
			Rejected []struct {
				TP     int `json:"tp"`
				Reason struct {
					Code   string         `json:"code"`
					Params map[string]any `json:"params"`
				} `json:"reason"`
			} `json:"rejected"`
		} `json:"explain"`
	} `json:"plan"`
	Breakdown *struct {
		KVBytesPerToken      float64 `json:"kv_bytes_per_token"`
		KVPoolBytes          int64   `json:"kv_pool_bytes"`
		EstimatedKVTokens    int     `json:"estimated_kv_tokens"`
		KVEstimateConfidence string  `json:"kv_estimate_confidence"`
		KVEstimateReason     string  `json:"kv_estimate_reason"`
		Grade                string  `json:"grade"`
		MaxModelLen          int     `json:"max_model_len"`
	} `json:"breakdown"`
}

func evaluateTemplate(t *testing.T, env *testenv.Env, body map[string]any) estimatePreview {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := env.API(t).PostJSON("/api/v1/evaluate", encoded)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("evaluate status = %d body=%s", response.StatusCode, raw)
	}
	var preview estimatePreview
	if err := json.Unmarshal(raw, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Breakdown == nil {
		t.Fatalf("evaluate returned no breakdown: %s", raw)
	}
	return preview
}

// seedEstimateCatalog registers one template whose model row is created by the
// template itself: no model was ever registered, so nothing fetched a
// config.json for it.
func seedEstimateCatalog(t *testing.T, env *testenv.Env, modelID, modelConfig, kvBytesPerToken string) store.InferenceModel {
	t.Helper()
	catalog := store.InferenceModel{
		ModelID: modelID, Source: "upstream", Architecture: "Qwen3ForCausalLM", ContextLength: 40960,
		ModelConfig: modelConfig,
		// The KV cost is the catalog's own figure now, keyed by engine, not
		// something derived from model_config at deploy time.
		KVBytesPerToken: kvBytesPerToken,
		Specs: []store.InferenceSpecRow{{
			Name: "nvidia-general", Engine: "vllm", Image: "gpuworks/runtime-nvidia:test",
			Quantization: "bf16", WeightSources: `[{"provider":"huggingface","repo_id":"` + modelID + `","revision":"main"}]`,
			WeightBytes: 1 << 30, GPUs: `{"vendor":"nvidia","cc":">=7.5"}`,
			Args: `["--max-model-len","40960"]`, Env: `{"CATALOG_LAYER":"nvidia"}`,
		}},
	}
	if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	row, err := env.Store().InferenceModelByModelID(modelID)
	if err != nil {
		t.Fatal(err)
	}
	return *row
}

// A template deployment must produce a KV-cache capacity even though the model
// was never registered and its repository is never fetched: the catalog carries
// the estimation facts, and the resolved model row must keep them so the
// scheduler and the deployment detail page see the same numbers.
func TestTemplateDeploymentEstimatesKVCapacityFromTheCatalog(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("kv-catalog", testenv.WithGPUs(1, testenv.GPUSpec{Model: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ComputeCapability: "8.0", VRAMMB: 81920}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "kv-catalog")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes: %v %v", nodes, err)
	}

	// A Qwen3-32B-shaped model: 64 layers, 8 KV heads, head_dim 128, which is
	// 64*8*128*2*2 = 262144 B/token, the figure the seed catalog carries for it.
	const modelID = "acme/kv-catalog"
	const wantKVBytesPerToken = 262144
	template := seedEstimateCatalog(t, env, modelID,
		`{"model_type":"qwen3","num_hidden_layers":64,"hidden_size":5120,"num_attention_heads":64,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":40960}`, `{"vllm":262144,"sglang":262144}`)

	preview := evaluateTemplate(t, env, map[string]any{
		"template_id": template.ID, "template_spec_source": "upstream", "template_spec_name": "nvidia-general",
		"node_uuid": nodes[0].UUID, "engine": "vllm",
	})
	if preview.Breakdown.KVBytesPerToken != wantKVBytesPerToken {
		t.Fatalf("kv_bytes_per_token = %v, want 262144", preview.Breakdown.KVBytesPerToken)
	}
	if preview.Breakdown.EstimatedKVTokens <= 0 {
		t.Fatalf("estimated_kv_tokens = %d (reason %q), want a positive capacity",
			preview.Breakdown.EstimatedKVTokens, preview.Breakdown.KVEstimateReason)
	}
	// "measured" is the confident answer now: the figure is the engine's own, so
	// there is no sizing rule left for a lower grade to describe.
	if preview.Breakdown.KVEstimateConfidence != "measured" || preview.Breakdown.KVEstimateReason != "" {
		t.Fatalf("confidence = %q reason = %q, want the measured figure",
			preview.Breakdown.KVEstimateConfidence, preview.Breakdown.KVEstimateReason)
	}

	// The facts must persist on the model the template resolved, otherwise every
	// later evaluation, launch and detail page would lose them again.
	model, err := env.Store().FindModelBySourceID(modelID, "huggingface")
	if err != nil {
		t.Fatal(err)
	}
	if model.Meta == "" {
		t.Fatal("resolved model row carries no config.json facts")
	}
	second := evaluateTemplate(t, env, map[string]any{
		"template_id": template.ID, "template_spec_source": "upstream", "template_spec_name": "nvidia-general",
		"node_uuid": nodes[0].UUID, "engine": "vllm",
	})
	if second.Breakdown.EstimatedKVTokens != preview.Breakdown.EstimatedKVTokens {
		t.Fatalf("second evaluation = %d tokens, want the same %d",
			second.Breakdown.EstimatedKVTokens, preview.Breakdown.EstimatedKVTokens)
	}
}

// A model that keeps no KV cache must say so. Reporting it as an unreadable
// configuration would make a finished answer look like a missing one.
func TestTemplateDeploymentReportsAModelWithoutKVCache(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("kv-none", testenv.WithGPUs(1, testenv.GPUSpec{Model: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ComputeCapability: "8.0", VRAMMB: 81920}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "kv-none")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes: %v %v", nodes, err)
	}
	const modelID = "acme/kv-none"
	template := seedEstimateCatalog(t, env, modelID, `{"kv_cache":false}`, "")

	preview := evaluateTemplate(t, env, map[string]any{
		"template_id": template.ID, "template_spec_source": "upstream", "template_spec_name": "nvidia-general",
		"node_uuid": nodes[0].UUID, "engine": "vllm",
	})
	if preview.Breakdown.EstimatedKVTokens != 0 {
		t.Fatalf("estimated_kv_tokens = %d, want none for a model without a KV cache", preview.Breakdown.EstimatedKVTokens)
	}
	if preview.Breakdown.KVEstimateConfidence != "not_applicable" {
		t.Fatalf("confidence = %q, want not_applicable", preview.Breakdown.KVEstimateConfidence)
	}
	if preview.Breakdown.KVEstimateReason == "" {
		t.Fatal("a model without a KV cache must explain itself")
	}
}

// A tensor-parallel size that cannot shard the model's attention heads is a
// configuration error, and the evaluation has to name it. Reporting a memory
// shortfall here was self-contradictory: the figures it printed ("needs 14.6 GiB,
// 72.0 GiB available") proved the candidate fits.
func TestEvaluationNamesAnImpossibleTensorParallel(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("kv-tp", testenv.WithGPUs(8, testenv.GPUSpec{Model: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ComputeCapability: "8.0", VRAMMB: 81920}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "kv-tp")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes: %v %v", nodes, err)
	}
	const modelID = "acme/kv-tp"
	// 64 attention heads, so tp=5 cannot shard them while the memory is ample.
	template := seedEstimateCatalog(t, env, modelID,
		`{"model_type":"qwen3","num_hidden_layers":64,"hidden_size":5120,"num_attention_heads":64,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":40960}`, `{"vllm":262144,"sglang":262144}`)

	preview := evaluateTemplate(t, env, map[string]any{
		"template_id": template.ID, "template_spec_source": "upstream", "template_spec_name": "nvidia-general",
		"node_uuid": nodes[0].UUID, "engine": "vllm", "placement": map[string]any{"tp": 5},
	})
	if preview.OK || preview.Plan == nil || preview.Plan.OK {
		t.Fatalf("tp=5 was accepted: %+v", preview.Plan)
	}
	if preview.Plan.Message.Code != "tp_head_count_not_divisible" {
		t.Fatalf("message = %+v, want the divisibility rejection", preview.Plan.Message)
	}
	if got := preview.Plan.Message.Params["head_count"]; got != float64(64) {
		t.Fatalf("head_count = %#v, want 64", got)
	}
	for _, key := range []string{"required_gib", "available_gib"} {
		if _, ok := preview.Plan.Message.Params[key]; ok {
			t.Fatalf("the verdict carries a memory figure (%s) it did not check: %+v", key, preview.Plan.Message.Params)
		}
	}
	if len(preview.Plan.Explain.Rejected) != 1 || preview.Plan.Explain.Rejected[0].TP != 5 ||
		preview.Plan.Explain.Rejected[0].Reason.Code != "tp_head_count_not_divisible" {
		t.Fatalf("explain = %+v", preview.Plan.Explain.Rejected)
	}
}

// A catalog model is reachable two ways: by picking one of its presets, which
// carries the catalog entry with it, and by deploying a registered model whose
// upstream identity is that entry. Both must produce the same KV figure, or the
// estimate depends on which door the user came through - the same catalog model
// reported its bytes per token by template and nothing at all by model id,
// because only the template path ever consulted the catalog.
func TestRegisteredModelFindsItsCatalogEntryForKVEstimate(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("kv-by-model", testenv.WithGPUs(1, testenv.GPUSpec{Model: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ComputeCapability: "8.0", VRAMMB: 81920}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "kv-by-model")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes: %v %v", nodes, err)
	}
	const modelID = "acme/kv-by-model-id"
	template := seedEstimateCatalog(t, env, modelID,
		`{"model_type":"qwen3","num_hidden_layers":64,"hidden_size":5120,"num_attention_heads":64,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":40960}`,
		`{"vllm":262144,"sglang":262144}`)

	// The catalog's own key is lowercase; the registered model keeps the upstream
	// spelling. The lookup has to bridge that.
	const upstreamID = "Acme/KV-By-Model-Id"
	// The same weight figure the catalog preset carries, so the two routes start
	// from identical inputs and any difference is the lookup itself.
	registered, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: upstreamID, Revision: "main", Name: upstreamID, WeightBytes: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	// No template: the catalog entry is only reachable through the model's own
	// upstream id.
	byModel := evaluateTemplate(t, env, map[string]any{
		"model_id": registered.ID, "node_uuid": nodes[0].UUID, "engine": "vllm",
	})
	if byModel.Breakdown.KVBytesPerToken <= 0 || byModel.Breakdown.EstimatedKVTokens <= 0 {
		t.Fatalf("registered model reported no KV cost: %+v", byModel.Breakdown)
	}
	if byModel.Breakdown.KVEstimateReason != "" {
		t.Fatalf("registered model gave a reason instead of a figure: %q", byModel.Breakdown.KVEstimateReason)
	}

	// The same model by template must answer identically.
	byTemplate := evaluateTemplate(t, env, map[string]any{
		"template_id": template.ID, "template_spec_source": "upstream",
		"template_spec_name": "nvidia-general", "node_uuid": nodes[0].UUID, "engine": "vllm",
	})
	if byModel.Breakdown.KVBytesPerToken != byTemplate.Breakdown.KVBytesPerToken {
		t.Fatalf("bytes/token differ by route: %v by model id, %v by template",
			byModel.Breakdown.KVBytesPerToken, byTemplate.Breakdown.KVBytesPerToken)
	}
	if byModel.Breakdown.EstimatedKVTokens != byTemplate.Breakdown.EstimatedKVTokens {
		t.Fatalf("token capacity differ by route: %d by model id, %d by template",
			byModel.Breakdown.EstimatedKVTokens, byTemplate.Breakdown.EstimatedKVTokens)
	}
}

// A model the catalog has no figure for reports no capacity, and says so with a
// code the client translates rather than with a sentence in the server's
// language.
func TestUnmeasuredModelReportsAReasonCode(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("kv-unmeasured", testenv.WithGPUs(1, testenv.GPUSpec{Model: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ComputeCapability: "8.0", VRAMMB: 81920}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "kv-unmeasured")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes: %v %v", nodes, err)
	}
	unmeasured, err := env.Store().UpsertModel(&store.Model{
		Source: "modelscope", SourceID: "Acme/Not-In-The-Catalog", Revision: "main", Name: "unmeasured",
	})
	if err != nil {
		t.Fatal(err)
	}
	preview := evaluateTemplate(t, env, map[string]any{
		"model_id": unmeasured.ID, "node_uuid": nodes[0].UUID, "engine": "vllm",
	})
	if preview.Breakdown.EstimatedKVTokens != 0 {
		t.Fatalf("unmeasured model claimed a capacity: %+v", preview.Breakdown)
	}
	if preview.Breakdown.KVEstimateReason != estimate.KVReasonNoMeasurement {
		t.Fatalf("reason = %q, want the stable code %q", preview.Breakdown.KVEstimateReason, estimate.KVReasonNoMeasurement)
	}
}

// A registered model is often a quantized release: a different repository
// serving the same cache layout as the model it quantizes. The catalog declares
// those repositories as weight sources of the model they belong to, and the
// lookup has to read that relationship - matching on the registered id alone
// answers nothing for a release the catalog explicitly knows about.
func TestQuantizedReleaseFindsTheKVEstimateOfTheModelItQuantizes(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("kv-quant", testenv.WithGPUs(1, testenv.GPUSpec{Model: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ComputeCapability: "8.0", VRAMMB: 81920}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "kv-quant")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes: %v %v", nodes, err)
	}
	const modelID = "acme/kv-quantized-base"
	const quantRepo = "Acme/KV-Quantized-Base-NVFP4"
	catalog := store.InferenceModel{
		ModelID: modelID, Source: "upstream", ContextLength: 40960,
		KVBytesPerToken: `{"vllm":262144}`,
		Specs: []store.InferenceSpecRow{{
			Name: "nvidia-general", Engine: "vllm", Image: "gpuworks/runtime-nvidia:test",
			Quantization: "bf16", WeightBytes: 1 << 30, GPUs: `{"vendor":"nvidia","cc":">=7.5"}`,
			Args: `[]`, Env: `{}`,
			// The quantized release is declared as this model's weight source.
			WeightSources: `[{"provider":"huggingface","repo_id":"` + quantRepo + `"}]`,
		}},
	}
	if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	registered, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: quantRepo, Revision: "main", Name: quantRepo, WeightBytes: 1 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	preview := evaluateTemplate(t, env, map[string]any{
		"model_id": registered.ID, "node_uuid": nodes[0].UUID, "engine": "vllm",
	})
	if preview.Breakdown.KVBytesPerToken != 262144 || preview.Breakdown.EstimatedKVTokens <= 0 {
		t.Fatalf("quantized release reported no KV cost: %+v", preview.Breakdown)
	}
	if preview.Breakdown.KVEstimateReason != "" {
		t.Fatalf("quantized release gave a reason instead of a figure: %q", preview.Breakdown.KVEstimateReason)
	}
}
