package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/templateexport"
)

func TestTemplateExportRejectsPartialAndMixedBoundCards(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "mixed"}[mixed], func(t *testing.T) {
			f := newTemplateFixture(t)
			mustCreateRow(t, f.st.DB().Model(&store.Instance{}).Where("id = ?", f.instance.ID).Update("gpu_idents", `["GPU-aaa","GPU-bbb"]`).Error)
			node, err := f.st.NodeByID(f.instance.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			if mixed {
				node.GPUs[1].Name = "NVIDIA A100"
			} else {
				node.GPUs = node.GPUs[:1]
			}
			mustCreateRow(t, f.st.DB().Save(node).Error)
			if _, err := loadTemplateSource(f.st, f.deployment.ID, nil); err == nil {
				t.Fatal("accepted incomplete or heterogeneous facts as two identical tested cards")
			}
		})
	}
}

func TestTemplateExportRefusesSavedEditsThatTheInstanceHasNotRun(t *testing.T) {
	for _, field := range []string{"quick", "env", "image"} {
		t.Run(field, func(t *testing.T) {
			f := newTemplateFixture(t)
			read, err := f.st.GetDeployment(f.deployment.ID)
			if err != nil {
				t.Fatal(err)
			}
			next := *read
			switch field {
			case "quick":
				next.QuickParams = `{"max_model_len":8192,"gpu_memory_utilization":0.9}`
			case "env":
				next.ContainerOverrides = `{"env":{"HF_TOKEN":"different"}}`
			case "image":
				next.Image = "different:runner"
			}
			if err := f.st.EditDeployment(read, nil, store.DeploymentEdit{Next: &next, Apply: store.EditApplyNone}); err != nil {
				t.Fatal(err)
			}
			_, err = loadTemplateSource(f.st, read.ID, nil)
			assertTemplateReason(t, err, templateexport.ReasonLaunchChanged)
		})
	}
}

func TestTemplateExportRequiresTheImmutableExecutableAndRechecksAfterProbing(t *testing.T) {
	t.Run("missing executable", func(t *testing.T) {
		f := newTemplateFixture(t)
		mustCreateRow(t, f.st.DB().Model(&store.Instance{}).Where("id = ?", f.instance.ID).Update("spec", "").Error)
		_, err := loadTemplateSource(f.st, f.deployment.ID, nil)
		assertTemplateReason(t, err, templateexport.ReasonLaunchMissing)
	})
	t.Run("changed while probing", func(t *testing.T) {
		f := newTemplateFixture(t)
		exporter := catalogTemplates{store: f.st, probe: func(context.Context, uint, string, string, map[string]any) templateexport.ProbeReply {
			f.update(t, "extra_args", "--seed 999")
			return templateexport.ProbeReply{HTTPStatus: 200, InstanceUUID: f.instance.UUID}
		}}
		_, err := exporter.Export(context.Background(), f.deployment.ID, nil)
		assertTemplateReason(t, err, templateexport.ReasonLaunchChanged)
	})
}

func TestTemplateExportUsesTheExecutedSnapshotAfterCatalogChanges(t *testing.T) {
	f := newTemplateFixture(t)
	const modelID = "allenai/olmo-2-0425-1b-instruct"
	if err := f.st.ReconcileInferenceCatalog([]store.InferenceModel{{ModelID: modelID, Source: "upstream", Specs: []store.InferenceSpecRow{{
		Name: "original", Source: "upstream", Engine: "vllm", Image: "runner:vllm", Quantization: "bf16", GPUs: `{"vendor":"nvidia"}`,
		WeightSources: `[{"provider":"huggingface","repo_id":"original/weights","revision":"` + templateCommit + `"}]`,
	}}}}); err != nil {
		t.Fatal(err)
	}
	row, err := f.st.InferenceModelByModelID(modelID)
	if err != nil {
		t.Fatal(err)
	}
	f.update(t, "template_id", row.ID)
	f.update(t, "template_spec_name", "original")
	f.update(t, "template_spec_source", "upstream")
	f.update(t, "template_snapshot", `{"template_id":1,"unsupported":[{"gpus":{"vendor":"nvidia","name":"^NVIDIA V100$"},"reason":"original rule"}],"model_id":"`+modelID+`","spec_name":"original","spec_source":"upstream","scenario":"balanced","engine":"vllm","image":"runner:vllm","image_sources":["mirror:old"],"quantization":"bf16","weight_provider":"huggingface","weight_repo_id":"original/weights","weight_revision":"`+templateCommit+`","env":{"SERVICE_PORT":"${API_PORT}"},"pre_start_argv":["echo","original"],"api":{"operations":[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]}}`)
	f.persistRuntimeLaunchAt(t, 31001)
	// A later catalog sync may remove/rename that spec and change its launch.
	mustCreateRow(t, f.st.DB().Model(&store.InferenceSpecRow{}).Where("model_id = ?", row.ID).Updates(map[string]any{"name": "replacement", "pre_start_argv": `["echo","changed"]`, "weight_sources": `[{"provider":"huggingface","repo_id":"changed/weights"}]`}).Error)
	mustCreateRow(t, f.st.DB().Model(&store.InferenceModel{}).Where("id = ?", row.ID).Update("unsupported", `[{"gpus":{"vendor":"nvidia","name":"^NVIDIA H20$"},"reason":"changed rule"}]`).Error)
	source, err := loadTemplateSource(f.st, f.deployment.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := templateexport.Build(source.input)
	if err != nil {
		t.Fatal(err)
	}
	spec := out.Model.Specs[0]
	if len(out.Model.Unsupported) != 1 || out.Model.Unsupported[0].Reason != "original rule" {
		t.Fatalf("a later catalog replaced the original GPU restrictions: %v", out.Model.Unsupported)
	}
	if spec.Weights.Sources[0].RepoID != "original/weights" || strings.Join(spec.PreStartArgv, " ") != "echo original" {
		t.Fatalf("a later catalog replaced executed facts: %+v", spec)
	}
	if len(spec.ImageSources) != 0 {
		t.Fatalf("unrelated image aliases survived the deployment's image override: %v", spec.ImageSources)
	}
	if spec.Env["SERVICE_PORT"] != "${API_PORT}" {
		t.Fatalf("the old instance's concrete port reached the portable template: %v", spec.Env)
	}
	materialized, err := deploy.Materialize(deploy.MaterializeInput{Engine: spec.Engine, TemplateArgs: spec.Args})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	again, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{Engine: engine, Image: spec.Image, Params: deploy.Params{ModelPath: "/model", ServedName: "new-model", APIPort: 31002, AssignedGPUs: 1, TensorParallel: 1, Quick: materialized.Quick, TemplateArgs: materialized.TemplateResidual, TemplateEnv: spec.Env}, Entrypoint: spec.Entrypoint, PreStartArgv: spec.PreStartArgv})
	if err != nil {
		t.Fatal(err)
	}
	if again.Env["SERVICE_PORT"] != "31002" {
		t.Fatalf("redeployment retained old port: %v", again.Env)
	}
	if _, err := inferencecatalog.ParseYAML([]byte(out.YAML)); err != nil {
		t.Fatalf("snapshot YAML cannot import: %v", err)
	}
}

func TestTemplateSnapshotKeepsImageAliasesOnlyForTheSameImage(t *testing.T) {
	selection := deploy.CatalogSelection{Image: "runner:old", ImageSources: []string{"mirror:old"}, WeightProvider: "huggingface", WeightRepoID: "org/model", Quantization: "bf16"}
	for _, image := range []string{"runner:old", "runner:new"} {
		spec, err := templateSpecFromSnapshot(selection, image)
		if err != nil {
			t.Fatal(err)
		}
		if (len(spec.ImageSources) > 0) != (image == "runner:old") {
			t.Fatalf("image %s sources %v", image, spec.ImageSources)
		}
	}
}

func assertTemplateReason(t *testing.T, err error, reason string) {
	t.Helper()
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Params()["reason"] != reason {
		t.Fatalf("error %v, want %s", err, reason)
	}
}

func (f *templateFixture) installSnapshot(t *testing.T, selection deploy.CatalogSelection) {
	t.Helper()
	const modelID = "allenai/olmo-2-0425-1b-instruct"
	if selection.Engine == "" {
		selection.Engine = "vllm"
	}
	selection.ModelID, selection.SpecName, selection.SpecSource, selection.Scenario = modelID, "runtime", "upstream", "balanced"
	selection.Image, selection.WeightProvider, selection.WeightRepoID, selection.WeightRevision, selection.Quantization = f.instance.Image, "huggingface", f.model.SourceID, templateCommit, "bf16"
	if err := f.st.ReconcileInferenceCatalog([]store.InferenceModel{{ModelID: modelID, Source: "upstream", Specs: []store.InferenceSpecRow{{
		Name: "runtime", Source: "upstream", Engine: selection.Engine, Image: selection.Image, Quantization: selection.Quantization, GPUs: `{"vendor":"nvidia"}`,
		WeightSources: `[{"provider":"huggingface","repo_id":"` + f.model.SourceID + `","revision":"` + templateCommit + `"}]`,
	}}}}); err != nil {
		t.Fatal(err)
	}
	row, err := f.st.InferenceModelByModelID(modelID)
	if err != nil {
		t.Fatal(err)
	}
	selection.TemplateID = row.ID
	raw, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	f.update(t, "engine", selection.Engine)
	f.update(t, "template_id", row.ID)
	f.update(t, "template_spec_name", selection.SpecName)
	f.update(t, "template_spec_source", selection.SpecSource)
	f.update(t, "template_snapshot", string(raw))
}

func TestTemplateExportSupportsAnExecutedCallableWithoutTopologyFlags(t *testing.T) {
	f := newTemplateFixture(t)
	f.installSnapshot(t, deploy.CatalogSelection{Engine: deploy.EngineCallable, Entrypoint: []string{"python3", "serve.py"}, ExtraArgs: []string{"--port", "${API_PORT}", "--model", "${MODEL_PATH}"}})
	f.persistRuntimeLaunch(t)
	exporter := catalogTemplates{store: f.st, probe: func(_ context.Context, _ uint, instance, _ string, _ map[string]any) templateexport.ProbeReply {
		return templateexport.ProbeReply{HTTPStatus: 200, InstanceUUID: instance}
	}}
	out, err := exporter.Export(context.Background(), f.deployment.ID, nil)
	if err != nil || !out.Verified || !out.Saveable {
		t.Fatalf("executed callable export: %+v %v", out, err)
	}
	parsed, err := inferencecatalog.ParseYAML([]byte(out.YAML))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(parsed.Models[0].Specs[0].Args, " ") != "--port ${API_PORT} --model ${MODEL_PATH} --seed 7" {
		t.Fatalf("callable parameters changed: %s", out.YAML)
	}
}

func TestTemplateExportDoesNotRestoreExplicitlyUnsetEnvironment(t *testing.T) {
	f := newTemplateFixture(t)
	f.installSnapshot(t, deploy.CatalogSelection{Env: map[string]string{"REMOVED": "template-value", "SERVICE_PORT": "${API_PORT}"}})
	f.update(t, "container_overrides", `{"env":{"HF_TOKEN":"secret"},"env_unset":["REMOVED"]}`)
	f.persistRuntimeLaunchAt(t, 31001)
	source, err := loadTemplateSource(f.st, f.deployment.ID, []string{"REMOVED"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := templateexport.Build(source.input)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := out.Model.Specs[0].Env["REMOVED"]; present {
		t.Fatalf("an explicitly unset variable returned: %v", out.Model.Specs[0].Env)
	}
	if out.Model.Specs[0].Env["SERVICE_PORT"] != "${API_PORT}" {
		t.Fatal("remaining env lost its portable expression")
	}
}

func TestTemplateExportUsesCapturedTensorParallelismRatherThanGPUCount(t *testing.T) {
	f := newTemplateFixture(t)
	f.update(t, "quick_params", `{"max_model_len":4096,"gpu_memory_utilization":0.9,"data_parallel_size":2}`)
	f.persistAllocatedRuntimeLaunch(t, 30000, []int{0, 1}, parallelism.Spec{TP: 1, PP: 1, NNodes: 1})
	source, err := loadTemplateSource(f.st, f.deployment.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source.input.GPUCount != 2 {
		t.Fatalf("card count %d", source.input.GPUCount)
	}
	var actual struct {
		Launch struct{ Entrypoint, Argv []string } `json:"launch"`
	}
	if err := json.Unmarshal([]byte(f.instance.DesiredStateJSON), &actual); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(append(actual.Launch.Entrypoint, actual.Launch.Argv...), " "), "--tensor-parallel-size 1") {
		t.Fatal("fixture did not exercise different TP and assigned-GPU counts")
	}
}

func TestTemplateExportKeepsUnsetTemplateDefaultsOutWhenUserEnvIsExcluded(t *testing.T) {
	f := newTemplateFixture(t)
	f.installSnapshot(t, deploy.CatalogSelection{Env: map[string]string{"REPLACED": "old-template-default"}})
	f.update(t, "container_overrides", `{"env_unset":["REPLACED"],"env":{"REPLACED":"new-user-value"}}`)
	f.persistRuntimeLaunch(t)
	for _, include := range [][]string{nil, {"REPLACED"}} {
		source, err := loadTemplateSource(f.st, f.deployment.ID, include)
		if err != nil {
			t.Fatal(err)
		}
		out, err := templateexport.Build(source.input)
		if err != nil {
			t.Fatal(err)
		}
		value, present := out.Model.Specs[0].Env["REPLACED"]
		if len(include) == 0 && present {
			t.Fatalf("excluding user env restored an unset, untested template value: %q", value)
		}
		if len(include) > 0 && value != "new-user-value" {
			t.Fatalf("explicit user opt-in did not preserve the executed replacement: %q", value)
		}
	}
}

func TestTemplateExportRejectsChecksFromAnotherReplica(t *testing.T) {
	f := newTemplateFixture(t)
	exporter := catalogTemplates{store: f.st, probe: func(context.Context, uint, string, string, map[string]any) templateexport.ProbeReply {
		return templateexport.ProbeReply{HTTPStatus: 200, InstanceUUID: "another-replica"}
	}}
	out, err := exporter.Export(context.Background(), f.deployment.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Verified || out.Saveable {
		t.Fatal("another replica verified the selected instance's hardware and weights")
	}
}

// An alias-less instance keeps the executable it was launched with. One
// launched before alias-less launches were named after the model mount, or on
// a host agent without proto.CapLaunchNameVerbatim, carries no
// --served-model-name; the export must re-plan with that naming instead of
// reporting a launch change.
func TestTemplateExportAcceptsAliasLessInstancesWhicheverWayTheyWereNamed(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "named after the mount", true: "no served-model-name"}[implicit], func(t *testing.T) {
			f := newTemplateFixture(t)
			f.update(t, "quick_params", `{"served_model_name":"","max_model_len":4096,"gpu_memory_utilization":0.9}`)
			f.implicitServedName = implicit
			f.persistRuntimeLaunch(t)
			instance, err := f.st.InstanceByUUID(f.instance.UUID)
			if err != nil {
				t.Fatal(err)
			}
			desired, err := instance.DesiredState()
			if err != nil {
				t.Fatal(err)
			}
			want := map[bool]string{false: "/model", true: ""}[implicit]
			if got := deploy.ServedModelName(desired.ResolvedLaunch); got != want {
				t.Fatalf("fixture launch names the model %q, want %q: %v", got, want, desired.Argv)
			}
			if _, err := loadTemplateSource(f.st, f.deployment.ID, nil); err != nil {
				t.Fatalf("export refused the instance's own launch: %v", err)
			}
		})
	}
}
