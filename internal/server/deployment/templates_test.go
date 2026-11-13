package deployment

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
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

func TestEffectiveTemplateAPIContractDoesNotInferFromLegacyKinds(t *testing.T) {
	contract, err := EffectiveTemplateAPIContract(store.InferenceModel{}, store.InferenceSpecRow{Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	var got inferenceapi.Contract
	if err := json.Unmarshal(contract, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Operations) != 0 {
		t.Fatalf("legacy catalog kinds synthesized operations: %+v", got.Operations)
	}
}

func TestApplyCurrentModelAPIContractOverridesTemplateContract(t *testing.T) {
	selection := &templateSelection{APIContract: json.RawMessage(`{"operations":[{"type":"openai.chat"}]}`)}
	applyCurrentModelAPIContract(selection, &store.Model{
		Operations: `[{"type":"openai.embeddings"}]`,
	})
	var got inferenceapi.Contract
	if err := json.Unmarshal(selection.APIContract, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Operations) != 1 || got.Operations[0].Type != inferenceapi.OperationEmbeddings {
		t.Fatalf("current model operations did not override template contract: %+v", got.Operations)
	}

	selection.APIContract = json.RawMessage(`{"operations":[{"type":"openai.chat"}]}`)
	applyCurrentModelAPIContract(selection, &store.Model{Operations: `[]`})
	if string(selection.APIContract) != `{"operations":[{"type":"openai.chat"}]}` {
		t.Fatalf("empty model operations replaced the template contract: %s", selection.APIContract)
	}
}

func TestFlatTemplateWeightSourcePrefersReachableNodeCopy(t *testing.T) {
	st, err := store.Open("file:flat_deployment_weight_source?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("source-node", store.HashToken("token"), "source-node")
	if err != nil {
		t.Fatal(err)
	}
	hf, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Deploy-FP8", Name: "local-hf"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: hf.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/acme"}); err != nil {
		t.Fatal(err)
	}
	selected, err := (New(Deps{Store: st})).selectWeightSource(deploymentFlatFixture().Specs[0], node.ID, nil)
	if err != nil || selected.Provider != "huggingface" {
		t.Fatalf("source=%+v err=%v", selected, err)
	}
}

func TestFlatTemplateSubfolderReusesCompleteRepositoryOnTargetNode(t *testing.T) {
	st, err := store.Open("file:flat_deployment_whole_repo_reuse?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("whole-source-node", store.HashToken("token"), "whole-source-node")
	if err != nil {
		t.Fatal(err)
	}
	whole, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "MiniMax/MiniMax-H3", Revision: "commit", Name: "complete"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: whole.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/h3"}); err != nil {
		t.Fatal(err)
	}
	spec := store.InferenceSpecRow{Name: "fl2va", WeightSources: `[{"provider":"modelscope","repo_id":"MiniMax/MiniMax-H3","revision":"commit","subfolder":"FL2VA"}]`}
	selected, err := (New(Deps{Store: st})).selectWeightSource(spec, node.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Subfolder != "" || !TemplateWeightMatchesModel(weightSource{Provider: "modelscope", RepoID: "MiniMax/MiniMax-H3", Revision: "commit", Subfolder: "FL2VA"}, whole) {
		t.Fatalf("complete repository was not reused: selected=%+v model=%+v", selected, whole)
	}
}

func TestTemplateWeightModelReusesEquivalentCrossSourceArtifact(t *testing.T) {
	st, err := store.Open("file:template_cross_source_reuse?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	original := &store.InferenceModel{ModelID: "acme/cross", Architecture: "qwen3", ContextLength: 32768}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Cross", Revision: "hf-rev", Arch: "qwen3", ContextLength: 32768, WeightBytes: 16 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	got, err := h.resolveTemplateWeightModel(weightSource{Provider: "modelscope", RepoID: "Acme/Cross", Revision: "ms-rev"}, original, "vllm", "fp8", 16<<30)
	if err != nil || got.ID != model.ID {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestDeploymentPrefersReadyEquivalentArtifactOnTargetNode(t *testing.T) {
	st, err := store.Open("file:template_target_node_reuse?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("target-node", store.HashToken("token"), "target-node")
	if err != nil {
		t.Fatal(err)
	}
	original := &store.InferenceModel{ModelID: "acme/target", Architecture: "qwen3", ContextLength: 32768}
	_, err = st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Target", Revision: "hf-rev", Arch: "qwen3", ContextLength: 32768, WeightBytes: 16 << 30})
	if err != nil {
		t.Fatal(err)
	}
	modelscope, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Target", Revision: "ms-rev", Arch: "qwen3", ContextLength: 32768, WeightBytes: 16 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: modelscope.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/acme-target"}); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	selection := &templateSelection{
		NodeID: node.ID, Model: original,
		Spec:   &store.InferenceSpecRow{Engine: "vllm", Quantization: "bf16", WeightBytes: 16 << 30},
		Weight: weightSource{Provider: "huggingface", RepoID: "Acme/Target", Revision: "hf-rev"},
	}
	got, err := h.resolveDeploymentModelFromTemplate(context.Background(), 0, selection, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != modelscope.ID || selection.Weight.Provider != "modelscope" || selection.Weight.Revision != "ms-rev" {
		t.Fatalf("got=%+v selection weight=%+v, want ready ModelScope artifact", got, selection.Weight)
	}
}

func TestFlatTemplateSelectionSnapshotAndModelRegistration(t *testing.T) {
	st, err := store.Open("file:flat_deployment_selection?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	model, _ := st.InferenceModelByModelID(fixture.ModelID)
	h := New(Deps{Store: st})
	req := dto.DeployRequest{TemplateID: model.ID, NodeUUID: "node", Placement: map[string]any{}}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "GeForce RTX 4090", Vendor: "nvidia", CC: 8.9}, HBMBytes: 24 << 30, CardCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Spec.Source != "upstream" || selection.Spec.Name != "fp8-balanced" || req.TemplateSpecSource != "upstream" || req.Engine != "vllm" || req.Image != "vllm/vllm-openai:latest" || selection.Weight.Provider != "modelscope" {
		t.Fatalf("selection=%+v req=%+v", selection, req)
	}
	if len(selection.PreStartArgv) != 2 || selection.PreStartArgv[0] != "prepare-tool" {
		t.Fatalf("pre-start=%#v", selection.PreStartArgv)
	}
	snapshot := snapshotTemplateSelection(selection, req.Image)
	if snapshot.SpecSource != "upstream" || snapshot.SpecName != "fp8-balanced" || snapshot.WeightRepoID != "Acme/Deploy-FP8" || snapshot.RuntimeWeightBytes != 8<<30 || !strings.Contains(snapshot.MatchedBy, "spec:fp8-balanced") || len(snapshot.PreStartArgv) != 2 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	registered, err := h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if registered.Source != "modelscope" || registered.SourceID != "Acme/Deploy-FP8" || registered.WeightBytes != 16<<30 || registered.QuantLabel != "fp8" {
		t.Fatalf("registered=%+v", registered)
	}
}

// A user who registered the repository before the catalog pinned a commit must
// still be able to deploy the template with that model: the template's revision
// is a preference, and the deployment records the identity actually used.
func TestTemplateExplicitModelKeepsUnpinnedRegistrationOfTheSameRepository(t *testing.T) {
	st, err := store.Open("file:template_explicit_model_revision?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.Specs[0].WeightSources = `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8","revision":"exact-revision"}]`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	catalogModel, _ := st.InferenceModelByModelID(fixture.ModelID)
	legacy, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-FP8", Revision: "", Name: "legacy-main"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	req := dto.DeployRequest{TemplateID: catalogModel.ID, TemplateSpecName: "fp8-balanced"}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.resolveDeploymentModelFromTemplate(context.Background(), legacy.ID, &selection, catalogModel.ID)
	if err != nil {
		t.Fatalf("registered model was rejected by its own template: %v", err)
	}
	if got.ID != legacy.ID {
		t.Fatalf("resolved model=%+v, want the registered row %d", got, legacy.ID)
	}
	if selection.Weight.Revision != "" || selection.Weight.Provider != "modelscope" || selection.Weight.RepoID != "Acme/Deploy-FP8" {
		t.Fatalf("deployment snapshot did not record the artifact actually used: %+v", selection.Weight)
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v, want no duplicate registration", models, err)
	}
}

// A different repository is a different model: the template must still refuse
// it rather than deploying weights the user did not ask for.
func TestTemplateExplicitModelFromAnotherRepositoryStillConflicts(t *testing.T) {
	st, err := store.Open("file:template_explicit_model_other_repo?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	catalogModel, _ := st.InferenceModelByModelID(fixture.ModelID)
	other, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Other-FP8", Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	req := dto.DeployRequest{TemplateID: catalogModel.ID, TemplateSpecName: "fp8-balanced"}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.resolveDeploymentModelFromTemplate(context.Background(), other.ID, &selection, catalogModel.ID); err == nil || !strings.Contains(err.Error(), "conflicts with template model") {
		t.Fatalf("expected explicit model conflict, err=%v", err)
	}
}

// A separately published quantization is its own repository. Matching by
// repository id must not let Acme/Deploy stand in for Acme/Deploy-FP8.
func TestTemplateQuantizedRepositoryIsNotInterchangeableWithTheBaseModel(t *testing.T) {
	st, err := store.Open("file:template_quantized_repo_identity?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	catalogModel, _ := st.InferenceModelByModelID(fixture.ModelID)
	base, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy", Name: "base"})
	if err != nil {
		t.Fatal(err)
	}
	quantized, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/deploy-fp8", Revision: "whatever", Name: "quantized"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	req := dto.DeployRequest{TemplateID: catalogModel.ID, TemplateSpecName: "fp8-balanced"}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.resolveDeploymentModelFromTemplate(context.Background(), base.ID, &selection, catalogModel.ID); err == nil || !strings.Contains(err.Error(), "conflicts with template model") {
		t.Fatalf("the unquantized repository was accepted for an FP8 spec: %v", err)
	}
	got, err := h.resolveDeploymentModelFromTemplate(context.Background(), quantized.ID, &selection, catalogModel.ID)
	if err != nil || got.ID != quantized.ID {
		t.Fatalf("got=%+v err=%v, want the quantized repository regardless of its provider/revision", got, err)
	}
}

// Providers may publish the same weights under different repository ids. A
// model registered from one of them must satisfy the spec even while another
// provider's source is the selected one.
func TestTemplateAcceptsAModelDeclaredByAnotherProviderRepository(t *testing.T) {
	st, err := store.Open("file:template_cross_provider_repo_ids?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"nvidia/Y-NVFP4","revision":"hf-pin"},{"provider":"modelscope","repo_id":"nv-community/Y-NVFP4","revision":"ms-pin"}]`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	catalogModel, _ := st.InferenceModelByModelID(fixture.ModelID)
	mirror, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "nv-community/Y-NVFP4", Name: "mirror"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st})
	req := dto.DeployRequest{TemplateID: catalogModel.ID, TemplateSpecName: "fp8-balanced"}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Weight.RepoID != "nvidia/Y-NVFP4" {
		t.Fatalf("fixture precondition: selected weight=%+v, want the first declared source", selection.Weight)
	}
	got, err := h.resolveDeploymentModelFromTemplate(context.Background(), mirror.ID, &selection, catalogModel.ID)
	if err != nil || got.ID != mirror.ID {
		t.Fatalf("got=%+v err=%v, want the registered mirror repository", got, err)
	}
	if selection.Weight.RepoID != "nv-community/Y-NVFP4" || selection.Weight.Provider != "modelscope" {
		t.Fatalf("snapshot weight=%+v, want the identity actually deployed", selection.Weight)
	}
}

func TestFlatTemplateSelectionRejectsUnsupportedGPUWithReason(t *testing.T) {
	st, err := store.Open("file:flat_deployment_unsupported?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{deploymentFlatFixture()}); err != nil {
		t.Fatal(err)
	}
	model, _ := st.InferenceModelByModelID("acme/deploy")
	_, err = (New(Deps{Store: st})).applyTemplateDefaults(
		&dto.DeployRequest{TemplateID: model.ID, TemplateSpecName: "fp8-balanced"},
		templateHardware{GPU: chipmatch.GPU{Name: "NVIDIA V100", Vendor: "nvidia", CC: 7}},
	)
	if err == nil || !strings.Contains(err.Error(), "FP8 requires Ada or newer") {
		t.Fatalf("error=%v", err)
	}
}

// A request may still override a template in all three dimensions: a field the
// spec does not set, an argument the dictionary does not describe, and the
// container environment. Each is reported separately.
func TestFlatTemplateExplicitArgsRemainUserOverridable(t *testing.T) {
	selection := templateSelection{Spec: &store.InferenceSpecRow{Name: "balanced", Engine: "vllm", Image: "image"}}
	req := &dto.DeployRequest{QuickParams: map[string]any{"max_model_len": 4096}, ExtraArgs: "--load-format fastsafetensors", ContainerOverrides: map[string]any{"env": map[string]string{"A": "B"}}}
	diff := flatTemplateRequestDiff("", "", "", "", req, selection)
	joined := strings.Join(diff, ",")
	for _, field := range []string{"env", "extra_args", "params"} {
		if !strings.Contains(joined, field) {
			t.Fatalf("diff=%v missing %s", diff, field)
		}
	}
}

// A field and an extra_args token that disagree are no longer a precedence
// question; the request cannot be materialized at all and is reported as
// departing from its template rather than silently resolved.
func TestFlatTemplateConflictingFieldAndExtraArgIsDrift(t *testing.T) {
	selection := templateSelection{Spec: &store.InferenceSpecRow{Name: "balanced", Engine: "vllm", Image: "image"}}
	req := &dto.DeployRequest{QuickParams: map[string]any{"max_model_len": 4096}, ExtraArgs: "--max-model-len 8192"}
	if _, err := materializeDeployRequest("vllm", req, selection, nil, ""); err == nil {
		t.Fatal("a field disagreeing with extra_args must not materialize")
	}
	if diff := flatTemplateRequestDiff("", "", "", "", req, selection); !strings.Contains(strings.Join(diff, ","), "params") {
		t.Fatalf("diff=%v", diff)
	}
}

func TestFlatTemplateDiffNormalizesGeneratedDefaults(t *testing.T) {
	selection := templateSelection{Spec: &store.InferenceSpecRow{Name: "balanced", Engine: "vllm", Image: "image"}, ExtraArgs: []string{"--max-model-len", "8192"}, Env: map[string]string{"A": "B"}}
	req := &dto.DeployRequest{ExtraArgs: "--max-model-len 8192", ContainerOverrides: map[string]any{"env": map[string]any{"A": "B"}}}
	if got := flatTemplateRequestDiff("", "", "", "", req, selection); len(got) != 0 {
		t.Fatalf("generated defaults were marked as drift: %v", got)
	}
}

func TestFlatTemplateAutomaticSelectionIsNotDeviated(t *testing.T) {
	st, err := store.Open("file:template_auto_no_drift?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	model, _ := st.InferenceModelByModelID(fixture.ModelID)
	req := &dto.DeployRequest{TemplateID: model.ID}
	selection, err := (New(Deps{Store: st})).applyTemplateDefaults(req, templateHardware{GPU: chipmatch.GPU{Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Deviated {
		t.Fatalf("automatic selection drift=%v", selection.Diff)
	}
}

func TestFlatTemplatePreservesExplicitImageOverride(t *testing.T) {
	fixture := deploymentFlatFixture()
	st, err := store.Open("file:flat_template_explicit_image?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	model, _ := st.InferenceModelByModelID(fixture.ModelID)
	req := dto.DeployRequest{TemplateID: model.ID, TemplateSpecName: "fp8-balanced", Image: "custom/vllm:cuda"}
	if _, err := (New(Deps{Store: st})).applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}}); err != nil {
		t.Fatal(err)
	}
	if req.Image != "custom/vllm:cuda" {
		t.Fatalf("image override=%q", req.Image)
	}
}

func TestDigestImageIsAdmittedForRuntimeVerification(t *testing.T) {
	const pinned = "registry.example/vendor/runtime:v1@sha256:0123456789abcdef"
	selection := templateSelection{Spec: &store.InferenceSpecRow{Image: pinned}}
	if rejectsDigestImage(pinned, selection) {
		t.Fatal("reviewed flat spec digest image was rejected")
	}
	if rejectsDigestImage(pinned, templateSelection{}) || rejectsDigestImage("registry.example/vendor/runtime:v2@sha256:fedcba9876543210", selection) {
		t.Fatal("digest image should be admitted and verified against the node-local digest")
	}
	if rejectsDigestImage("registry.example/vendor/runtime:v1", templateSelection{}) {
		t.Fatal("ordinary tagged image was rejected")
	}
}

func TestFlatTemplateUnknownPinnedImageKeepsEngineCommandMode(t *testing.T) {
	overrides, err := RequestContainerOverrides(nil)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := deploy.SelectEngine("sglang")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
		Engine: engine, Params: deploy.Params{ModelPath: "/model", TemplateArgs: []string{"--device", "npu"}},
		ContainerOverrides: overrides,
	})
	if err != nil {
		t.Fatal(err)
	}
	if launch.CommandMode != string(deploy.CommandEngine) || !strings.Contains(strings.Join(append(launch.Entrypoint, launch.Argv...), " "), "--device npu") {
		t.Fatalf("resolved launch=%+v", launch)
	}
}

func TestFlatTemplateComponentsBecomeImmutableModelBindings(t *testing.T) {
	st, err := store.Open("file:flat_deployment_components?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	target := deploymentFlatFixture()
	target.ModelID = "acme/target"
	target.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"Acme/Target-FP8","revision":"target-rev"}]`
	target.Specs[0].Components = `[
		{"name":"target","role":"primary","weights":{"quantization":"fp8","sources":[{"provider":"huggingface","repo_id":"Acme/Target-FP8","revision":"target-rev"}]},"weight_bytes":17179869184,"mount_path":"/models/target"},
		{"name":"draft","role":"speculative_draft","model_id":"acme/draft","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"Acme/Draft","revision":"draft-rev"}]},"weight_bytes":2147483648,"mount_path":"/models/draft"}
	]`
	draft := store.InferenceModel{ModelID: "acme/draft", Source: "upstream", Architecture: "draft", Specs: []store.InferenceSpecRow{{
		Name: "bf16", Engine: "vllm", Image: "image", Quantization: "bf16", WeightBytes: 2 << 30,
		WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Draft","revision":"draft-rev"}]`, GPUs: `{"vendor":"nvidia","cc":">=8.0"}`,
	}}}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{target, draft}); err != nil {
		t.Fatal(err)
	}
	catalogModel, _ := st.InferenceModelByModelID(target.ModelID)
	h := New(Deps{Store: st})
	req := dto.DeployRequest{TemplateID: catalogModel.ID, TemplateSpecName: "fp8-balanced"}
	selection, err := h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	primary, err := h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, catalogModel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if primary.SourceID != "Acme/Target-FP8" || primary.Revision != "target-rev" || len(selection.Components) != 2 {
		t.Fatalf("primary=%+v components=%+v", primary, selection.Components)
	}
	if selection.Weight.Provider != "huggingface" || selection.Weight.RepoID != "Acme/Target-FP8" || selection.Weight.Revision != "target-rev" {
		t.Fatalf("top-level selected weight did not follow the primary component: %+v", selection.Weight)
	}
	for _, component := range selection.Components {
		if component.StoreModelID == 0 {
			t.Fatalf("component did not receive immutable model binding: %+v", component)
		}
		bound, err := st.GetModel(component.StoreModelID)
		if err != nil || bound.SourceID != component.RepoID || bound.Revision != component.Revision {
			t.Fatalf("component=%+v bound=%+v err=%v", component, bound, err)
		}
	}
	snapshot := snapshotTemplateSelection(selection, req.Image)
	if snapshot.WeightRevision != "target-rev" || len(snapshot.Components) != 2 || snapshot.Components[1].MountPath != "/models/draft" || snapshot.Components[1].Revision != "draft-rev" || snapshot.Components[1].WeightBytes != 2<<30 {
		t.Fatalf("snapshot components=%+v", snapshot.Components)
	}
	modelMount, mounts := previewComponentMounts(st, selection.Components, nil)
	if modelMount != "/models/target" || len(mounts) != 1 || mounts[0].Source != "$MODEL_COMPONENT_DRAFT_PATH" || !mounts[0].ReadOnly {
		t.Fatalf("offline component preview mount=%q mounts=%+v", modelMount, mounts)
	}
}

func TestTemplateParserValuesCanBeKeptCustomizedOrDisabled(t *testing.T) {
	spec := &store.InferenceSpecRow{Engine: "vllm", Args: `["--dtype","auto","--reasoning-parser","qwen3","--tool-call-parser=hermes"]`}
	selection := templateSelection{Spec: spec, ExtraArgs: []string{"--dtype", "auto", "--reasoning-parser", "qwen3", "--tool-call-parser=hermes"}}
	req := dto.DeployRequest{
		Engine:      "vllm",
		ExtraArgs:   "--dtype\nauto",
		QuickParams: map[string]any{"reasoning_parser": "qwen3", "tool_call_parser": "hermes"},
	}
	if diff := flatTemplateRequestDiff("vllm", "", "", "", &req, selection); len(diff) != 0 {
		t.Fatalf("unchanged template parser values marked as drift: %v", diff)
	}
	// The parser arguments are dictionary values, so nothing of the spec's args
	// survives as passthrough text once they are materialized.
	launch, err := materializeDeployRequest("vllm", &req, selection, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if launch.Quick.ReasoningParser != "qwen3" || launch.Quick.ToolCallParser != "hermes" || launch.Quick.Dtype != "auto" {
		t.Fatalf("template parser arguments were not delegated to structured values: %+v", launch.Quick)
	}
	if len(launch.TemplateResidual) != 0 || len(launch.UserResidual) != 0 {
		t.Fatalf("template=%v user=%v residual must be empty", launch.TemplateResidual, launch.UserResidual)
	}
	req.QuickParams["reasoning_parser"] = ""
	if diff := flatTemplateRequestDiff("vllm", "", "", "", &req, selection); !reflect.DeepEqual(diff, []string{"params"}) {
		t.Fatalf("disabling a template parser must be explicit drift: %v", diff)
	}
}

func TestTemplateCacheReportFlagMatchingQuickParamIsNotDrift(t *testing.T) {
	spec := &store.InferenceSpecRow{Engine: "vllm", Args: `["--dtype","auto","--enable-prompt-tokens-details"]`}
	selection := templateSelection{Spec: spec, ExtraArgs: []string{"--dtype", "auto", "--enable-prompt-tokens-details"}}
	req := dto.DeployRequest{
		Engine:      "vllm",
		ExtraArgs:   "--dtype\nauto",
		QuickParams: map[string]any{"enable_cache_report": true},
	}
	if diff := flatTemplateRequestDiff("vllm", "", "", "", &req, selection); len(diff) != 0 {
		t.Fatalf("template cache-report flag matching the quick param marked as drift: %v", diff)
	}
	req.QuickParams["enable_cache_report"] = false
	if diff := flatTemplateRequestDiff("vllm", "", "", "", &req, selection); !reflect.DeepEqual(diff, []string{"params"}) {
		t.Fatalf("overriding the template cache-report flag must be explicit drift: %v", diff)
	}
}
