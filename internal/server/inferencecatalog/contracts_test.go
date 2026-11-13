package inferencecatalog

import (
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// validateFileValuedArguments exists because a spec declared
// --reasoning-parser-plugin nano_v3_reasoning_parser.py and the file was in no
// image, in no weights repository and in no asset, so the spec could not start
// under any image. These cases hold the check to the four outcomes it claims:
// supplied by an asset, supplied by a mount, supplied by form (an absolute
// path), and supplied by nothing.
func TestFileValuedArgumentsMustBeSuppliedByTheSpec(t *testing.T) {
	pluginArgs := []string{"--reasoning-parser", "nemotron_v3"}

	t.Run("asset unpack_to supplies the file", func(t *testing.T) {
		catalog := flatFixture()
		spec := &catalog.Models[0].Specs[0]
		spec.Assets = []Asset{{File: "assets/parser.tar.gz", SHA256: strings.Repeat("a", 64), UnpackTo: "/opt/gpuworks/runtime"}}
		spec.Args = append(append([]string{}, pluginArgs...), "--reasoning-parser-plugin", "opt/gpuworks/runtime/nano_v3_reasoning_parser.py")
		if err := ValidateFlatCatalog(&catalog); err != nil {
			t.Fatalf("asset supplied plugin rejected: %v", err)
		}
	})

	t.Run("component mount supplies the file", func(t *testing.T) {
		catalog := flatFixture()
		spec := &catalog.Models[0].Specs[0]
		spec.Components = []Component{{Name: "primary", Role: "primary", Weights: spec.Weights, WeightBytes: spec.WeightBytes, MountPath: "/models/plugin"}}
		spec.Args = append(append([]string{}, pluginArgs...), "--tool-call-parser-plugin", "models/plugin/nemotron_toolcall_parser_no_streaming.py")
		if err := ValidateFlatCatalog(&catalog); err != nil {
			t.Fatalf("mount supplied plugin rejected: %v", err)
		}
	})

	t.Run("start script supplies the file", func(t *testing.T) {
		catalog := flatFixture()
		spec := &catalog.Models[0].Specs[0]
		spec.PreStartArgv = nil
		spec.PreStartScript = "cp -R /opt/gpuworks/overlay/nano_v3_reasoning_parser.py /"
		spec.Args = append(append([]string{}, pluginArgs...), "--reasoning-parser-plugin", "nano_v3_reasoning_parser.py")
		if err := ValidateFlatCatalog(&catalog); err != nil {
			t.Fatalf("start script supplied plugin rejected: %v", err)
		}
	})

	t.Run("absolute path is left to the image", func(t *testing.T) {
		catalog := flatFixture()
		spec := &catalog.Models[0].Specs[0]
		spec.Args = append(append([]string{}, pluginArgs...), "--reasoning-parser-plugin", "/opt/gpuworks/runtime/nano_v3_reasoning_parser.py")
		if err := ValidateFlatCatalog(&catalog); err != nil {
			t.Fatalf("absolute plugin path rejected: %v", err)
		}
	})

	t.Run("bare filename is rejected with a stable code", func(t *testing.T) {
		catalog := flatFixture()
		spec := &catalog.Models[0].Specs[0]
		spec.Args = append(append([]string{}, pluginArgs...), "--reasoning-parser-plugin", "nano_v3_reasoning_parser.py")

		err := ValidateFlatCatalog(&catalog)
		var productErr *messages.ProductError
		if !errors.As(err, &productErr) || productErr.Code() != messages.ErrorCatalogFileValuedArgUnresolved {
			t.Fatalf("error=%v, want code %s", err, messages.ErrorCatalogFileValuedArgUnresolved)
		}
		params := productErr.Params()
		if params["flag"] != "--reasoning-parser-plugin" || params["value"] != "nano_v3_reasoning_parser.py" || params["spec"] != spec.Name {
			t.Fatalf("params=%v", params)
		}

		// The value, not the flag, is what makes the spec undeployable: the
		// same spec becomes valid the moment the file has a source.
		spec.PreStartArgv = nil
		spec.PreStartScript = "cp /opt/gpuworks/overlay/nano_v3_reasoning_parser.py ."
		if err := ValidateFlatCatalog(&catalog); err != nil {
			t.Fatalf("supplied plugin still rejected: %v", err)
		}
	})

	t.Run("inline form is judged the same way", func(t *testing.T) {
		catalog := flatFixture()
		spec := &catalog.Models[0].Specs[0]
		spec.Args = append(append([]string{}, pluginArgs...), "--reasoning-parser-plugin=nano_v3_reasoning_parser.py", "--tool-call-parser-plugin=/opt/gpuworks/runtime/tool.py")
		err := ValidateFlatCatalog(&catalog)
		var productErr *messages.ProductError
		if !errors.As(err, &productErr) || productErr.Code() != messages.ErrorCatalogFileValuedArgUnresolved || productErr.Params()["value"] != "nano_v3_reasoning_parser.py" {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("flag without a value is rejected", func(t *testing.T) {
		catalog := flatFixture()
		spec := &catalog.Models[0].Specs[0]
		spec.Args = append(append([]string{}, pluginArgs...), "--tool-call-parser-plugin")
		err := ValidateFlatCatalog(&catalog)
		var productErr *messages.ProductError
		if !errors.As(err, &productErr) || productErr.Code() != messages.ErrorCatalogFileValuedArgUnresolved || productErr.Params()["value"] != "" {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestExecutableContractsExpressH3FusedAndExternalDrafts(t *testing.T) {
	h3 := flatFixture().Models[0]
	h3.ModelID = "minimaxai/minimax-h3"
	h3.Specs[0].Entrypoint = []string{"python3", "-m", "sglang.launch_server"}
	h3.Specs[0].Args = []string{"--model-type", "diffusion", "--model-variant", "fl2va", "--tp-size", "${TP_SIZE}", "--sp-degree", "${SP_DEGREE}"}
	h3.Specs[0].Placement = &Placement{Language: "starlark", Resolver: "def resolve(gpu):\n    return {\"TP_SIZE\": gpu.count, \"SP_DEGREE\": 1}\n"}
	h3.Specs[0].API = &APIContract{HealthEndpoint: "/health", Operations: []APIOperation{{Type: "openai.video.generate", Endpoint: "/v1/videos", Mode: "async"}}}

	fused := flatFixture().Models[0]
	fused.ModelID = "deepseek-ai/deepseek-v4-flash-dspark"
	fused.Specs[0].Args = append(fused.Specs[0].Args, "--speculative-config", `{"method":"dspark","num_speculative_tokens":7}`)

	draft := flatFixture().Models[0]
	draft.ModelID = "redhatai/glm-5.2-speculator.dspark"
	draft.Specs[0].Name = "draft"
	target := flatFixture().Models[0]
	target.ModelID = "zai-org/glm-5.2"
	target.Specs[0].Components = []Component{
		{Name: "target", Role: "primary", Weights: target.Specs[0].Weights, WeightBytes: target.Specs[0].WeightBytes, MountPath: "/models/target"},
		{Name: "dspark", Role: "speculative_draft", ModelID: draft.ModelID, Weights: draft.Specs[0].Weights, WeightBytes: draft.Specs[0].WeightBytes, MountPath: "/models/draft"},
	}
	target.Specs[0].Args = append(target.Specs[0].Args, "--speculative-config", `{"method":"dspark","model":"/models/draft"}`)

	eagle := flatFixture().Models[0]
	eagle.ModelID = "acme/eagle3-draft"
	eagleWeights := eagle.Specs[0].Weights
	eagle.Specs = nil
	eagleTarget := flatFixture().Models[0]
	eagleTarget.ModelID = "acme/eagle3-target"
	eagleTarget.Specs[0].Components = []Component{
		{Name: "target", Role: "primary", Weights: eagleTarget.Specs[0].Weights, WeightBytes: eagleTarget.Specs[0].WeightBytes, MountPath: "/models/target"},
		{Name: "draft", Role: "speculative_draft", ModelID: eagle.ModelID, Weights: eagleWeights, WeightBytes: eagleTarget.Specs[0].WeightBytes, MountPath: "/models/draft"},
		{Name: "verify", Role: "verifier", ModelID: eagle.ModelID, Weights: eagleWeights, WeightBytes: eagleTarget.Specs[0].WeightBytes, MountPath: "/models/verifier"},
	}
	eagleTarget.Specs[0].Args = append(eagleTarget.Specs[0].Args, "--speculative-draft-model", "/models/draft", "--speculative-verifier", "/models/verifier")

	catalog := FlatCatalog{Models: []InferenceModel{h3, fused, draft, target, eagle, eagleTarget}}
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalYAML(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"entrypoint:", "placement:", "openai.video.generate", "speculative_draft", "verifier"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("schema example missing %q:\n%s", want, encoded)
		}
	}
}

func TestExecutableContractsRejectBrokenReferencesAndUnsafePaths(t *testing.T) {
	base := flatFixture()
	tests := []struct {
		name string
		edit func(*FlatCatalog)
		want string
	}{
		{name: "asset escape", edit: func(c *FlatCatalog) {
			c.Models[0].Specs[0].Assets = []Asset{{File: "../patch.py", SHA256: strings.Repeat("a", 64)}}
		}, want: "own assets"},
		{name: "asset digest", edit: func(c *FlatCatalog) {
			c.Models[0].Specs[0].Assets = []Asset{{File: "assets/patch.py", SHA256: "short"}}
		}, want: "SHA-256"},
		{name: "unknown operation", edit: func(c *FlatCatalog) {
			c.Models[0].Specs[0].API = &APIContract{Operations: []APIOperation{{Type: "custom.magic"}}}
		}, want: "unsupported type"},
		{name: "missing draft", edit: func(c *FlatCatalog) {
			weights := c.Models[0].Specs[0].Weights
			c.Models[0].Specs[0].Components = []Component{{Name: "target", Role: "primary", Weights: weights, WeightBytes: c.Models[0].Specs[0].WeightBytes, MountPath: "/models/target"}, {Name: "draft", Role: "speculative_draft", ModelID: "missing/draft", Weights: weights, WeightBytes: c.Models[0].Specs[0].WeightBytes, MountPath: "/models/draft"}}
			c.Models[0].Specs[0].Args = append(c.Models[0].Specs[0].Args, "--speculative-draft-model", "/models/draft")
		}, want: "missing original model"},
		{name: "unreferenced model without specs", edit: func(c *FlatCatalog) {
			c.Models = append(c.Models, InferenceModel{ModelID: "acme/orphan", Source: "upstream"})
		}, want: "unless referenced as a runtime component"},
		{name: "two primaries", edit: func(c *FlatCatalog) {
			weights := c.Models[0].Specs[0].Weights
			c.Models[0].Specs[0].Components = []Component{{Name: "a", Role: "primary", Weights: weights, WeightBytes: c.Models[0].Specs[0].WeightBytes, MountPath: "/models/a"}, {Name: "b", Role: "primary", Weights: weights, WeightBytes: c.Models[0].Specs[0].WeightBytes, MountPath: "/models/b"}}
		}, want: "exactly one primary"},
		{name: "component weight bytes", edit: func(c *FlatCatalog) {
			weights := c.Models[0].Specs[0].Weights
			c.Models[0].Specs[0].Components = []Component{{Name: "target", Role: "primary", Weights: weights, MountPath: "/models/target"}}
		}, want: "positive weight_bytes"},
		{name: "nested component mounts", edit: func(c *FlatCatalog) {
			weights := c.Models[0].Specs[0].Weights
			c.Models[0].Specs[0].Components = []Component{
				{Name: "target", Role: "primary", Weights: weights, WeightBytes: c.Models[0].Specs[0].WeightBytes, MountPath: "/model"},
				{Name: "draft", Role: "speculative_draft", ModelID: c.Models[0].ModelID, Weights: weights, WeightBytes: c.Models[0].Specs[0].WeightBytes, MountPath: "/model/frontend"},
			}
			c.Models[0].Specs[0].Args = append(c.Models[0].Specs[0].Args, "--speculative-draft-model", "/model/frontend")
		}, want: "nested component mount_paths"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalog := base
			catalog.Models = append([]InferenceModel(nil), base.Models...)
			catalog.Models[0].Specs = append([]InferenceSpec(nil), base.Models[0].Specs...)
			test.edit(&catalog)
			if err := ValidateFlatCatalog(&catalog); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

// A launch no deployment could expand is refused at import, not at launch:
// a callable spec that never tells its adapter the platform's port, and a
// spec without a resolver that names a variable nobody states. The
// preparation script's other names belong to its shell.
func TestImportRefusesTemplateVariablesNoLaunchCanExpand(t *testing.T) {
	serve := []string{"--model", "${MODEL_PATH}", "--host", "${API_HOST}", "--port", "${API_PORT}"}
	for _, test := range []struct {
		name string
		edit func(*InferenceSpec)
		want string
	}{
		{name: "callable places the port", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append([]string(nil), serve...)
		}},
		{name: "callable without the port", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", []string{"--model", "${MODEL_PATH}"}
		}, want: "must reference ${API_PORT}"},
		{name: "callable port from the preparation script", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.PreStartArgv, spec.PreStartScript = "callable", nil, "echo ${API_PORT} > /tmp/port"
		}},
		{name: "distributed facts are platform variables", edit: func(spec *InferenceSpec) {
			spec.Engine = "callable"
			spec.Args = append(append([]string(nil), serve...), "--rank", "${NODE_RANK}", "--master", "${MASTER_ADDR}:${MASTER_PORT}")
			spec.Env = map[string]string{"WORLD": "${NNODES}", "FOLLOWER": "${HEADLESS}"}
		}},
		{name: "typo without a resolver", edit: func(spec *InferenceSpec) {
			spec.Engine, spec.Args = "callable", append(append([]string(nil), serve...), "--rank", "${NODE_RNAK}")
		}, want: "${NODE_RNAK}"},
		{name: "pipeline degree has no platform default", edit: func(spec *InferenceSpec) {
			spec.Args = append(spec.Args, "--pipeline-parallel-size", "${PP_SIZE}")
		}, want: "${PP_SIZE}"},
		{name: "a resolver may supply its own names", edit: func(spec *InferenceSpec) {
			spec.Args = append(spec.Args, "--sp", "${SP_DEGREE}")
			spec.Placement = &Placement{Language: "starlark", Resolver: "def resolve(gpu):\n    return {\"SP_DEGREE\": 1}\n"}
		}},
		{name: "the script's shell names are the shell's", edit: func(spec *InferenceSpec) {
			spec.PreStartArgv, spec.PreStartScript = nil, `export PATH="${HOME}/bin:${PATH}"`
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := flatFixture()
			test.edit(&catalog.Models[0].Specs[0])
			err := ValidateFlatCatalog(&catalog)
			if test.want == "" {
				if err != nil {
					t.Fatalf("unexpected error=%v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestConcreteRegisteredOperationValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		api  *APIContract
		want string
	}{
		{name: "missing operation is allowed", want: ""},
		{name: "video endpoint cannot drift", api: &APIContract{Operations: []APIOperation{{Type: "openai.video.generate", Endpoint: "/v1/chat/completions", Mode: "async"}}}, want: "outside its allowed routes"},
		{name: "video must remain async", api: &APIContract{Operations: []APIOperation{{Type: "openai.video.generate", Endpoint: "/v1/videos", Mode: "sync"}}}, want: "does not support mode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := flatFixture()
			catalog.Models[0].Specs[0].API = test.api
			err := ValidateFlatCatalog(&catalog)
			if test.want == "" {
				if err != nil {
					t.Fatalf("unexpected error=%v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}

	catalog := flatFixture()
	catalog.Models[0].Specs[0].API = &APIContract{HealthEndpoint: "/ready", Operations: []APIOperation{{Type: "openai.video.generate"}}}
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatal(err)
	}
	operation := catalog.Models[0].Specs[0].API.Operations[0]
	if operation.Endpoint != "/v1/videos" || operation.Mode != "async" {
		t.Fatalf("normalized operation=%+v", operation)
	}
}

// A template's api.metrics is checked at import like the rest of its api
// contract: an import naming a metric the platform does not define is refused
// with the stable code the editor shows, not stored as a mapping that
// collects nothing.
func TestImportValidatesTheMetricsDeclaration(t *testing.T) {
	catalog := flatFixture()
	catalog.Models[0].Specs[0].API = &APIContract{
		Operations: []APIOperation{{Type: "openai.chat"}},
		Metrics:    &inferenceapi.Metrics{Map: inferenceapi.MetricMap{"tensorfold:requests_running": {To: "requests_unknown"}}},
	}
	err := ValidateFlatCatalog(&catalog)
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorCatalogMetricsUnknownMetric {
		t.Fatalf("err = %v, want %s", err, messages.ErrorCatalogMetricsUnknownMetric)
	}

	catalog = flatFixture()
	catalog.Models[0].Specs[0].API = &APIContract{
		Operations: []APIOperation{{Type: "openai.chat"}},
		Metrics:    &inferenceapi.Metrics{Map: inferenceapi.MetricMap{" tensorfold:requests_running ": {To: "requests_running"}}},
	}
	if err := ValidateFlatCatalog(&catalog); err != nil {
		t.Fatal(err)
	}
	if _, trimmed := catalog.Models[0].Specs[0].API.Metrics.Map["tensorfold:requests_running"]; !trimmed {
		t.Fatalf("normalized metrics = %+v", catalog.Models[0].Specs[0].API.Metrics)
	}
}

// An auxiliary component carries weights the engine reads beside the primary
// model (IndexTTS 2.5's Wav2Vec2-BERT and vocoder). It names no catalog model
// of its own, and like every non-primary component its mount must be used by
// the launch.
func TestAuxiliaryComponentsNeedNoModelButMustBeUsed(t *testing.T) {
	for _, test := range []struct {
		name   string
		role   string
		script string
		want   string
	}{
		{name: "auxiliary referenced by the preparation script", role: "auxiliary", script: "ln -s /models/vocoder /tmp/model/bigvgan"},
		{name: "auxiliary nobody uses", role: "auxiliary", script: "true", want: "/models/vocoder"},
		{name: "unknown role", role: "asset", script: "ln -s /models/vocoder /tmp/model/bigvgan", want: `invalid role "asset"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := flatFixture()
			spec := &catalog.Models[0].Specs[0]
			weights := spec.Weights
			spec.Components = []Component{
				{Name: "target", Role: "primary", Weights: weights, WeightBytes: spec.WeightBytes, MountPath: "/models/target"},
				{Name: "vocoder", Role: test.role, Weights: weights, WeightBytes: 1 << 20, MountPath: "/models/vocoder"},
			}
			spec.PreStartArgv, spec.PreStartScript = nil, test.script
			err := ValidateFlatCatalog(&catalog)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}
