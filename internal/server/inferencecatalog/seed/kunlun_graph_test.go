package seed

import (
	"encoding/json"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// Use the write boundary and real engine renderer: dropping the template flag
// alone is insufficient if materialization restores eager mode or loses the
// runner-specific Gated DeltaNet graph boundaries.
func TestBuiltinKunlunQwen38DefaultsToPiecewiseGraph(t *testing.T) {
	_, spec := catalogSpec(t, "qwen/qwen3.8-27b", "bf16-vllm-kunlun-p800-xre502146")
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		raw   map[string]any
		eager bool
	}{
		{name: "template defaults"},
		{name: "explicit off", raw: map[string]any{"enforce_eager": false}},
		{name: "compatibility override", raw: map[string]any{"enforce_eager": true}, eager: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			materialized, err := deploy.Materialize(deploy.MaterializeInput{
				Engine: spec.Engine, EngineVersion: "0.25.1", Raw: tc.raw, TemplateArgs: spec.Args,
			})
			if err != nil {
				t.Fatal(err)
			}
			launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
				Engine: engine, Image: spec.Image, Entrypoint: spec.Entrypoint, APIContract: spec.API,
				Params: deploy.Params{
					Vendor: "kunlunxin", RuntimeDriver: "docker", ModelPath: "/model", APIPort: 30000,
					AssignedGPUs: 1, TensorParallel: 1, NNodes: 1,
					Quick: materialized.Quick, TemplateArgs: materialized.TemplateResidual,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, eager := deploy.LaunchFlag(launch.Argv, "--enforce-eager"); eager != tc.eager {
				t.Fatalf("enforce-eager = %t, want %t", eager, tc.eager)
			}
			graph, found := deploy.LaunchFlag(launch.Argv, "--compilation-config")
			var config struct {
				Mode         int             `json:"mode"`
				GraphMode    string          `json:"cudagraph_mode"`
				Backend      string          `json:"backend"`
				Sizes        []int           `json:"cudagraph_capture_sizes"`
				SplittingOps []string        `json:"splitting_ops"`
				PassConfig   map[string]bool `json:"pass_config"`
			}
			if !found || json.Unmarshal([]byte(graph), &config) != nil || config.Mode != 3 || config.GraphMode != "PIECEWISE" || config.Backend != "eager" {
				t.Fatalf("missing runner-specific piecewise graph configuration: %q", graph)
			}
			if !slices.Equal(config.Sizes, []int{1}) || materialized.Quick.MaxNumSeqs != 1 || materialized.Quick.MaxModelLen != 8192 || materialized.Quick.Dtype != "float16" {
				t.Fatal("graph capture no longer matches the validated single-sequence FP16 recipe")
			}
			for _, pass := range []string{"fuse_norm_quant", "fuse_act_quant"} {
				if enabled, present := config.PassConfig[pass]; !present || enabled {
					t.Fatalf("unvalidated fusion %s must be explicitly disabled", pass)
				}
			}
			// These stateful operations must stay outside captured regions. In
			// particular both the model wrapper and XPU GDN kernel need splits.
			for _, op := range []string{
				"vllm::unified_attention_with_output", "vllm::qwen_gdn_attention_core",
				"vllm::gdn_attention_core_xpu", "vllm::gdn_attention_core",
				"vllm::unified_kv_cache_update", "vllm::unified_mla_kv_cache_update",
			} {
				if !slices.Contains(config.SplittingOps, op) {
					t.Errorf("missing stateful graph boundary %s", op)
				}
			}
			if len(config.SplittingOps) != 18 || commandFlagCount(launch.Argv, "--compilation-config") != 1 {
				t.Fatal("runner graph boundary list or argument count changed")
			}
		})
	}
}
