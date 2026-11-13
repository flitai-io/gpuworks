package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/store"
)

func flatAPIFixture(modelID, specName string) store.InferenceModel {
	return store.InferenceModel{ModelID: modelID, DisplayTitle: modelID, ReleaseDate: "2025-04-03", Source: "upstream", Unsupported: `[]`, Specs: []store.InferenceSpecRow{{
		Name: specName, DisplayNameZHCN: "Turing 及以上 · BF16", DisplayNameEN: "Turing+ · BF16", Scenario: "balanced", Engine: "vllm", Image: "vllm/vllm-openai:latest", Quantization: "bf16",
		WeightSources: `[{"provider":"huggingface","repo_id":"` + modelID + `"}]`, GPUs: `{"vendor":"nvidia","cc":">=7.5","display_target":"nvidia"}`, Args: `[]`, Env: `{}`, PreStartArgv: `[]`,
	}}}
}
