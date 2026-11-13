package templateexport

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

const commit = "48d788eca847d4d7548f375ad03d3c9312f6139e"

func customInput() Input {
	return Input{
		DeploymentID: 12, DeploymentName: "olmo", Engine: deploy.EngineVLLM,
		Image:    "gpuworks/gpuworks-runner:cuda13.0-vllm0.31.0-r1",
		Quick:    deploy.LaunchConfig{MaxModelLen: 4096, GPUMemUtil: 0.88, CPUCores: 4},
		UserArgs: []string{"--seed", "7"},
		UserEnv:  map[string]string{"HF_TOKEN": "secret", "VLLM_LOGGING_LEVEL": "DEBUG"},
		Model: ModelFacts{
			Source: "huggingface", RepoID: "allenai/OLMo-2-0425-1B-Instruct", Quantization: "bfloat16",
			WeightBytes: 2969854224, ContextLength: 4096,
			ConfigJSON:        `{"model_type":"olmo2","num_hidden_layers":16,"hidden_size":2048,"num_attention_heads":16,"num_key_value_heads":16,"max_position_embeddings":4096}`,
			InputModalities:   []string{"text"},
			Operations:        []inferenceapi.Operation{{Type: inferenceapi.OperationChat, Endpoint: "/v1/chat/completions", Mode: "stream"}},
			ResolvedRevisions: []string{commit},
		},
		GPUs:     []GPUFact{{Name: "NVIDIA H20", Vendor: "nvidia", MemoryMiB: 97871, ComputeCap: "9.0"}},
		GPUCount: 1,
		Checks:   []Check{{Operation: inferenceapi.OperationChat, Endpoint: "/v1/chat/completions", Tested: true, Passed: true, HTTPStatus: 200}},
	}
}

func TestBuildWritesACustomDeploymentAsANewModel(t *testing.T) {
	out, err := Build(customInput())
	if err != nil {
		t.Fatal(err)
	}
	if !out.Verified || !out.Saveable {
		t.Fatalf("verified=%v saveable=%v, want both", out.Verified, out.Saveable)
	}
	model := out.Model
	if model.ModelID != "allenai/olmo-2-0425-1b-instruct" || model.Source != "user" || model.DisplayTitle != "OLMo-2-0425-1B-Instruct" {
		t.Fatalf("model identity = %q %q %q", model.ModelID, model.Source, model.DisplayTitle)
	}
	if model.ModelConfig == nil || model.ModelConfig.NumHiddenLayers != 16 {
		t.Fatalf("model_config = %#v, want parsed from config.json", model.ModelConfig)
	}
	if model.KVBytesPerToken != nil {
		t.Fatalf("kv_bytes_per_token = %v, want none without a catalog value", model.KVBytesPerToken)
	}
	spec := model.Specs[0]
	if spec.Name != "vllm-1xnvidia-h20-d12" || spec.Weights.Quantization != "bf16" || spec.Placement != nil {
		t.Fatalf("spec = %q quantization %q placement %v", spec.Name, spec.Weights.Quantization, spec.Placement)
	}
	wantSource := inferencecatalog.WeightSource{Provider: "huggingface", RepoID: "allenai/OLMo-2-0425-1B-Instruct", Revision: commit}
	if !reflect.DeepEqual(spec.Weights.Sources, []inferencecatalog.WeightSource{wantSource}) {
		t.Fatalf("weights = %#v", spec.Weights.Sources)
	}
	if spec.GPUs.Vendor != "nvidia" || spec.GPUs.Name != `^NVIDIA H20$` {
		t.Fatalf("gpus = %#v", spec.GPUs)
	}
	if spec.Env != nil {
		t.Fatalf("env = %v, want the deployment's own env left out", spec.Env)
	}
	if !reflect.DeepEqual(out.ExcludedEnv, []string{"HF_TOKEN", "VLLM_LOGGING_LEVEL"}) {
		t.Fatalf("excluded env = %q", out.ExcludedEnv)
	}
	for _, want := range []string{"--max-model-len", "4096", "--gpu-memory-utilization", "0.88", "--seed", "7"} {
		if !slices.Contains(spec.Args, want) {
			t.Fatalf("args %q lack %q", spec.Args, want)
		}
	}
	for _, code := range []string{NoteVerifiedOn, NoteKVBytesMissing, NotePlacementOmitted, NoteEnvExcluded, NoteParametersDropped, NoteOperationPassed} {
		if !hasNote(out.Notes, code) {
			t.Fatalf("notes %+v lack %s", out.Notes, code)
		}
	}
	if strings.Contains(out.YAML, "secret") || strings.Contains(out.Evidence, "secret") {
		t.Fatal("an env value reached the export")
	}
}

func TestBuildYAMLImportsAsTheModelItDescribes(t *testing.T) {
	out, err := Build(customInput())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.YAML, "# Saved from GPUWorks deployment \"olmo\" (id 12).\n") ||
		!strings.Contains(out.YAML, "# Missing kv_bytes_per_token for vllm") || !strings.Contains(out.YAML, "# Tested openai.chat: HTTP 200.") {
		t.Fatalf("YAML header:\n%s", out.YAML)
	}
	parsed, err := inferencecatalog.ParseYAML([]byte(out.YAML))
	if err != nil {
		t.Fatalf("exported YAML does not import: %v\n%s", err, out.YAML)
	}
	bare, err := inferencecatalog.MarshalYAML(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{out.Model}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := inferencecatalog.MarshalYAML(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(bare) {
		t.Fatalf("the comments changed what the document says:\n%s\nvs\n%s", again, bare)
	}
}

func TestBuildFromATemplateKeepsWhatTheDeploymentDidNotChange(t *testing.T) {
	template := inferencecatalog.InferenceModel{
		ModelID: "allenai/olmo-2-0425-1b-instruct", Source: "upstream", DisplayTitle: "OLMo",
		ReadmeEN: "README.en.md", KVBytesPerToken: map[string]int64{"vllm": 524288},
	}
	source := inferencecatalog.InferenceSpec{
		Name: "bf16-vllm-nvidia", DisplayNameEN: "H100 · BF16", Scenario: "latency", Engine: "vllm", Image: "old:image",
		Weights:   inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "allenai/OLMo-2-0425-1B-Instruct", Revision: commit}}},
		GPUs:      inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"},
		Args:      []string{"--max-model-len", "4096", "--upstream-only"},
		Placement: &inferencecatalog.Placement{Language: "starlark", Resolver: "def resolve(gpu):\n    return accept(gpu_memory_mib_per_device=8192)\n"},
	}
	template.Specs = []inferencecatalog.InferenceSpec{source}
	in := customInput()
	in.Template, in.TemplateSpec = &template, &template.Specs[0]
	in.TemplateArgs = []string{"--upstream-only"}
	in.TemplateEnv = map[string]string{"VLLM_LOGGING_LEVEL": "DEBUG"}
	in.Quick = deploy.LaunchConfig{MaxModelLen: 8192}
	in.UserArgs = nil
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	spec := out.Model.Specs[0]
	if out.Model.ModelID != template.ModelID || out.Model.ReadmeEN != "" || out.Model.KVBytesPerToken["vllm"] != 524288 {
		t.Fatalf("model = %q readme %q kv %v", out.Model.ModelID, out.Model.ReadmeEN, out.Model.KVBytesPerToken)
	}
	if spec.Placement == nil || spec.Scenario != "latency" || spec.DisplayNameEN != "" || spec.Image != in.Image {
		t.Fatalf("spec placement %v scenario %q display %q image %q", spec.Placement, spec.Scenario, spec.DisplayNameEN, spec.Image)
	}
	if !reflect.DeepEqual(spec.Weights, source.Weights) {
		t.Fatalf("weights = %#v, want the template's", spec.Weights)
	}
	if at := slices.Index(spec.Args, "--max-model-len"); at < 0 || spec.Args[at+1] != "8192" || !slices.Contains(spec.Args, "--upstream-only") {
		t.Fatalf("args = %q, want the deployment's 8192 and the template residual", spec.Args)
	}
	if spec.Env["VLLM_LOGGING_LEVEL"] != "DEBUG" || len(out.ExcludedEnv) != 1 || out.ExcludedEnv[0] != "HF_TOKEN" {
		t.Fatalf("env = %v excluded = %q: a user value equal to the template's is the template's", spec.Env, out.ExcludedEnv)
	}
	if hasNote(out.Notes, NotePlacementOmitted) || hasNote(out.Notes, NoteKVBytesMissing) {
		t.Fatalf("notes %+v: a template spec keeps its placement and the model its KV figure", out.Notes)
	}
	if _, err := inferencecatalog.ParseYAML([]byte(out.YAML)); err != nil {
		t.Fatalf("exported YAML does not import: %v", err)
	}
}

func TestBuildKeepsOnlyTheUserEnvAskedFor(t *testing.T) {
	in := customInput()
	in.IncludeEnv = []string{"VLLM_LOGGING_LEVEL"}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Model.Specs[0].Env; !reflect.DeepEqual(got, map[string]string{"VLLM_LOGGING_LEVEL": "DEBUG"}) {
		t.Fatalf("env = %v", got)
	}
	if !reflect.DeepEqual(out.ExcludedEnv, []string{"HF_TOKEN"}) {
		t.Fatalf("excluded = %q", out.ExcludedEnv)
	}
}

func TestBuildRecordsChecksThatFailedOrWereNotSent(t *testing.T) {
	in := customInput()
	in.Checks = []Check{
		{Operation: inferenceapi.OperationChat, Tested: true, HTTPStatus: 500, Message: "engine error"},
		{Operation: "openai.audio.speech"},
	}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Verified || out.Saveable {
		t.Fatal("a failed check left the export saveable")
	}
	if !hasNote(out.Notes, NoteOperationFailed) || !hasNote(out.Notes, NoteOperationNotTested) {
		t.Fatalf("notes = %+v", out.Notes)
	}
	if !strings.Contains(out.YAML, "# FAILED openai.chat: HTTP 500 engine error") || !strings.Contains(out.YAML, "# Not tested: openai.audio.speech") {
		t.Fatalf("YAML:\n%s", out.YAML)
	}

	in.Checks = []Check{{Operation: "openai.audio.speech"}}
	if out, err = Build(in); err != nil || !out.Saveable {
		t.Fatalf("an untested operation alone blocks saving: %v saveable=%v", err, out.Saveable)
	}
}

func TestBuildNotesTheDGPULimitAsTheDeploymentsChoice(t *testing.T) {
	in := customInput()
	in.VRAMLimitBytes = 24 << 30
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if note := findNote(out.Notes, NoteVRAMLimit); note == nil || note.Params["vram_limit_mib"] != int64(24576) {
		t.Fatalf("notes = %+v", out.Notes)
	}
	if note := findNote(out.Notes, NotePlacementOmitted); note == nil || note.Params["gpu_memory_mib"] != int64(24576) {
		t.Fatalf("placement note = %+v, want the limited memory", note)
	}
	if out.Model.Specs[0].Placement != nil {
		t.Fatal("the dGPU limit became a placement requirement")
	}
}

func TestBuildRefusesWhatASpecCannotExpress(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Input)
		reason string
	}{
		{"no cards", func(in *Input) { in.GPUs, in.GPUCount = nil, 0 }, ReasonNoGPUFacts},
		{"URL weights", func(in *Input) { in.Model.Source = "url" }, ReasonWeightsNotPublic},
		{"branch revision", func(in *Input) { in.Model.ResolvedRevisions = []string{"main"} }, ReasonRevisionUnpinned},
		{"copies disagree", func(in *Input) { in.Model.ResolvedRevisions = []string{commit, "a1b2c3d4e5f6"} }, ReasonRevisionsDiffer},
		{"template assets", func(in *Input) {
			model := inferencecatalog.InferenceModel{ModelID: "x/y"}
			spec := inferencecatalog.InferenceSpec{Assets: []inferencecatalog.Asset{{File: "a.py"}}}
			in.Template, in.TemplateSpec = &model, &spec
		}, ReasonTemplateAssets},
		{"template components", func(in *Input) {
			model := inferencecatalog.InferenceModel{ModelID: "x/y"}
			spec := inferencecatalog.InferenceSpec{Components: []inferencecatalog.Component{{Name: "draft"}}}
			in.Template, in.TemplateSpec = &model, &spec
		}, ReasonComponents},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := customInput()
			tc.mutate(&in)
			_, err := Build(in)
			var product *messages.ProductError
			if !errors.As(err, &product) || product.Code() != ErrorUnavailable || product.Params()["reason"] != tc.reason {
				t.Fatalf("error = %v, want %s/%s", err, ErrorUnavailable, tc.reason)
			}
		})
	}
}

func TestSelectorMatchesTheCardAndNotItsNeighbours(t *testing.T) {
	cases := []struct {
		card     GPUFact
		matches  []string
		excludes []string
	}{
		{GPUFact{Name: "NVIDIA H20", Vendor: "nvidia"}, []string{"NVIDIA H20"}, []string{"NVIDIA H200", "NVIDIA H20 NVL", "NVIDIA H100"}},
		{GPUFact{Name: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia"}, []string{"NVIDIA A100-SXM4-80GB"}, []string{"NVIDIA A100-SXM4-40GB", "NVIDIA A100X"}},
		{GPUFact{Name: "Ascend 910B4", Vendor: "ascend"}, []string{"Ascend 910B4"}, []string{"Ascend 910B3"}},
	}
	for _, tc := range cases {
		selector, err := Selector(tc.card)
		if err != nil {
			t.Fatal(err)
		}
		predicate := chipmatch.Predicate{Vendor: selector.Vendor, Name: selector.Name}
		for _, name := range tc.matches {
			if !chipmatch.Evaluate(chipmatch.ReportedGPU(name, tc.card.Vendor, "", ""), predicate).Matched {
				t.Fatalf("%q does not match %q", selector.Name, name)
			}
		}
		for _, name := range tc.excludes {
			if chipmatch.Evaluate(chipmatch.ReportedGPU(name, tc.card.Vendor, "", ""), predicate).Matched {
				t.Fatalf("%q matches %q", selector.Name, name)
			}
		}
	}
	if _, err := Selector(GPUFact{}); err == nil {
		t.Fatal("a card with no name produced a selector")
	}
}

func TestEvidenceNamesTheCardsAndEveryNote(t *testing.T) {
	in := customInput()
	in.GPUs[0].DriverVersion = "580.65"
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- Cards: 1 x NVIDIA H20 (nvidia, 97871 MiB, compute capability 9.0, driver 580.65)", "- Engine: vllm", "- Image: gpuworks/gpuworks-runner:cuda13.0-vllm0.31.0-r1", "- Tested openai.chat: HTTP 200."} {
		if !strings.Contains(out.Evidence, want) {
			t.Fatalf("evidence lacks %q:\n%s", want, out.Evidence)
		}
	}
}

func hasNote(notes []Note, code string) bool { return findNote(notes, code) != nil }

func findNote(notes []Note, code string) *Note {
	for index := range notes {
		if notes[index].Code == code {
			return &notes[index]
		}
	}
	return nil
}

func TestBuildNamesTheDefaultEngineForARowThatStatesNone(t *testing.T) {
	in := customInput()
	in.Engine = ""
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Model.Specs[0].Engine != deploy.DefaultEngine || !strings.HasPrefix(out.Model.Specs[0].Name, deploy.DefaultEngine+"-") {
		t.Fatalf("spec engine %q name %q", out.Model.Specs[0].Engine, out.Model.Specs[0].Name)
	}
	if _, err := inferencecatalog.ParseYAML([]byte(out.YAML)); err != nil {
		t.Fatal(err)
	}
}
