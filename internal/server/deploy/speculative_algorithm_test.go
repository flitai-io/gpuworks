package deploy

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/localization"
)

// vLLM's speculative method is free text with suggestions, like the parsers:
// a method the list names, one it does not, and one in another case all reach
// --speculative-config, the last in vLLM's own spelling. Whether the image's
// vLLM knows the method is the deployer's to check.
func TestVLLMSpeculativeMethodIsFreeTextWithSuggestions(t *testing.T) {
	engine, _ := SelectEngine(EngineVLLM)
	for _, tc := range []struct{ written, want string }{
		{"dflash", "dflash"}, {"dspark", "dspark"}, {"DFlash", "dflash"},
		{"dflash2", "dflash2"}, {"qwen3_5_mtp", "qwen3_5_mtp"},
	} {
		out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{
			"speculative_algorithm": tc.written, "speculative_draft_model": "/draft", "speculative_tokens": 5,
		}})
		if err != nil {
			t.Fatalf("%s: %v", tc.written, err)
		}
		if err := ValidateLaunchConfigForEngine(EngineVLLM, out.Quick); err != nil {
			t.Fatalf("%s: free-text method refused: %v", tc.written, err)
		}
		if out.Quick.SpeculativeAlgorithm != tc.want {
			t.Fatalf("%s: stored %q, want %q", tc.written, out.Quick.SpeculativeAlgorithm, tc.want)
		}
		built := engine.Build(Params{ModelPath: "/model", APIPort: 8000, Quick: out.Quick})
		want := `{"method":"` + tc.want + `","model":"/draft","num_speculative_tokens":5}`
		if !argvHas(built.Argv, "--speculative-config", want) {
			t.Fatalf("%s: argv %v lacks --speculative-config %s", tc.written, built.Argv, want)
		}
	}
}

// The dictionary publishes the row per engine: a combobox of suggestions for
// vLLM, which includes the draft-mounting methods, and SGLang's closed list.
func TestSpeculativeAlgorithmRowIsOpenForVLLMAndClosedForSGLang(t *testing.T) {
	vllm := engineParams(EngineVLLM)["speculative_algorithm"]
	if vllm.Type != "string" || vllm.Control != "combobox" {
		t.Fatalf("vLLM row type=%s control=%s, want a string combobox", vllm.Type, vllm.Control)
	}
	for _, method := range []string{"mtp", "eagle3", "dflash", "dspark"} {
		if !slices.Contains(vllm.Enum, method) {
			t.Fatalf("vLLM suggestions %v lack %s", vllm.Enum, method)
		}
	}
	for _, option := range vllm.Options {
		if strings.TrimSpace(option.Label[localization.Chinese]) == "" || strings.TrimSpace(option.Label[localization.English]) == "" {
			t.Fatalf("vLLM option %s has no label", option.Value)
		}
	}
	sglang := engineParams(EngineSGLang)["speculative_algorithm"]
	if sglang.Type != "enum" || sglang.Control != "select" {
		t.Fatalf("SGLang row type=%s control=%s, want a closed enum", sglang.Type, sglang.Control)
	}
	if slices.Contains(sglang.Enum, "dflash") || !slices.Contains(sglang.Enum, "DSPARK") {
		t.Fatalf("SGLang options %v", sglang.Enum)
	}
}
