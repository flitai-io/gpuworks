package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/templateexport"
)

const templateCommit = "48d788eca847d4d7548f375ad03d3c9312f6139e"

type templateFixture struct {
	st         *store.Store
	deployment store.Deployment
	instance   store.Instance
	model      store.Model
	// implicitServedName persists launches the way a server that predates
	// naming alias-less launches, or a host agent without
	// proto.CapLaunchNameVerbatim, left them: no --served-model-name.
	implicitServedName bool
}

// newTemplateFixture stores a running single-node deployment of a model made
// without a template, the way the create path and the agent's reports leave
// it: the model row, a node with two cards, an instance bound to one of them
// by hardware identity, and the model copy that instance reads.
func newTemplateFixture(t *testing.T) *templateFixture {
	t.Helper()
	st, err := store.Open(fmt.Sprintf("file:app_templateexport_%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_")), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db := st.DB()
	f := &templateFixture{st: st}
	f.model = store.Model{
		Source: "huggingface", SourceID: "allenai/OLMo-2-0425-1B-Instruct", Revision: "main", Name: "OLMo 2 1B",
		Precision: "bf16", WeightBytes: 2969854224, ContextLength: 4096,
		Meta:            `{"model_type":"olmo2","num_hidden_layers":16,"hidden_size":2048,"num_attention_heads":16,"num_key_value_heads":16}`,
		Operations:      `[{"type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`,
		InputModalities: `["text"]`,
	}
	mustCreateRow(t, db.Create(&f.model).Error)
	node := store.Node{UUID: "node-1", Name: "gpu3", State: "online", NvidiaDriver: "580.65",
		GPUs: []store.GPU{
			{Index: 0, Ident: "GPU-aaa", UUID: "GPU-aaa", Name: "NVIDIA H20", Vendor: "nvidia", ComputeCapability: "9.0", MemoryTotal: 96 << 30},
			{Index: 1, Ident: "GPU-bbb", UUID: "GPU-bbb", Name: "NVIDIA H20", Vendor: "nvidia", ComputeCapability: "9.0", MemoryTotal: 96 << 30},
		}}
	mustCreateRow(t, db.Create(&node).Error)
	f.deployment = store.Deployment{
		Name: "olmo", ModelID: f.model.ID, NodeID: node.ID, Engine: "vllm", Image: "runner:vllm@sha", State: store.DeployActive,
		QuickParams:        `{"max_model_len":4096,"gpu_memory_utilization":0.9}`,
		ExtraArgs:          "--seed 7",
		ContainerOverrides: `{"env":{"HF_TOKEN":"secret"}}`,
	}
	mustCreateRow(t, db.Create(&f.deployment).Error)
	f.instance = store.Instance{UUID: "inst-1", DeploymentID: f.deployment.ID, NodeID: node.ID, State: proto.InstRunning,
		GPUIdents: `["GPU-aaa"]`, Image: "runner:vllm@sha"}
	mustCreateRow(t, db.Create(&f.instance).Error)
	file := store.ModelFile{ModelID: f.model.ID, NodeID: node.ID, State: "ready", Revision: templateCommit, SizeBytes: 2969854224}
	mustCreateRow(t, db.Create(&file).Error)
	mustCreateRow(t, db.Create(&store.InstanceModelFile{InstanceID: f.instance.ID, ModelID: f.model.ID, ModelFileID: file.ID}).Error)
	f.persistRuntimeLaunch(t)
	return f
}

// Use the same pure post-allocation assembly as the controller, so the
// fixture includes the immutable executable that a real running row stores.
func (f *templateFixture) persistRuntimeLaunch(t *testing.T) {
	f.persistRuntimeLaunchAt(t, 30000)
}

func (f *templateFixture) persistRuntimeLaunchAt(t *testing.T, port int) {
	f.persistAllocatedRuntimeLaunch(t, port, []int{0}, parallelism.Spec{TP: 1, PP: 1, NNodes: 1})
}

func (f *templateFixture) persistAllocatedRuntimeLaunch(t *testing.T, port int, indexes []int, parallel parallelism.Spec) {
	t.Helper()
	dep, err := f.st.GetDeployment(f.deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	node, err := f.st.NodeByID(f.instance.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := f.st.GetModel(f.model.ID)
	if err != nil {
		t.Fatal(err)
	}
	request, _, _, err := planning.DeploymentLaunch(dep, nil, node, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Source.Image = f.instance.Image
	request.ImplicitServedName = f.implicitServedName
	plan, err := planning.ResolveAllocatedLaunch(request, indexes, len(indexes), parallel, port, nil, model.WeightBytes, false)
	if err != nil {
		t.Fatal(err)
	}
	var idents []string
	for _, index := range indexes {
		for _, card := range node.GPUs {
			if card.Index == index {
				idents = append(idents, card.Ident)
				break
			}
		}
	}
	desired := proto.InstanceDesiredState{ResolvedLaunch: plan.Resolved, UUID: f.instance.UUID, DeploymentID: dep.ID, Engine: dep.Engine, Image: f.instance.Image,
		GPUIndexes: indexes, GPUIdents: idents, APIPort: port, VRAMLimitBytes: dep.VRAMLimitBytes, ModelPath: "/models/olmo"}
	raw, err := json.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	f.instance.DesiredStateJSON = string(raw)
	bound, err := json.Marshal(idents)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateRow(t, f.st.DB().Model(&store.Instance{}).Where("id = ?", f.instance.ID).Updates(map[string]any{"spec": string(raw), "gpu_idents": string(bound)}).Error)
}

func mustCreateRow(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadReadsWhatTheRunningInstanceRan(t *testing.T) {
	f := newTemplateFixture(t)
	source, err := loadTemplateSource(f.st, f.deployment.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := source.input
	if in.Image != "runner:vllm@sha" || in.GPUCount != 1 || len(in.GPUs) != 1 || in.GPUs[0].MemoryMiB != 96<<10 || in.GPUs[0].DriverVersion != "580.65" {
		t.Fatalf("image %q, cards %d %+v", in.Image, in.GPUCount, in.GPUs)
	}
	if in.Quick.MaxModelLen != 4096 || !slices.Equal(in.UserArgs, []string{"--seed", "7"}) || in.UserEnv["HF_TOKEN"] != "secret" {
		t.Fatalf("launch layers: quick %+v user %q env %v", in.Quick, in.UserArgs, in.UserEnv)
	}
	if !slices.Equal(in.Model.ResolvedRevisions, []string{templateCommit}) || in.Model.Quantization != "bf16" || len(in.Model.Operations) != 1 {
		t.Fatalf("model facts = %+v", in.Model)
	}
	if op, ok := source.contract.CheckOperation(); !ok || op.Type != inferenceapi.OperationChat {
		t.Fatalf("contract = %+v", source.contract)
	}
	out, err := templateexport.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Model.Specs[0].Weights.Sources[0].Revision != templateCommit {
		t.Fatalf("weights = %+v, want the commit the copy resolved, not the model's branch", out.Model.Specs[0].Weights)
	}
}

func TestLoadRefusesDeploymentsASpecCannotDescribe(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*templateFixture)
		reason string
	}{
		{"stopped", func(f *templateFixture) { f.update(t, "desired_state", store.DeployStopped) }, templateexport.ReasonNotRunning},
		{"no running instance", func(f *templateFixture) {
			mustCreateRow(t, f.st.DB().Model(&store.Instance{}).Where("id = ?", f.instance.ID).Update("state", proto.InstStarting).Error)
		}, templateexport.ReasonNotRunning},
		{"multi-node", func(f *templateFixture) { f.update(t, "n_nodes", 2) }, templateexport.ReasonDistributed},
		{"pd", func(f *templateFixture) { f.update(t, "placement", `{"pd":{"mode":"vllm-nixl","roles":{}}}`) }, templateexport.ReasonPD},
		{"command overridden", func(f *templateFixture) {
			f.update(t, "container_overrides", `{"cmd_display":"[\"python\",\"serve.py\"]"}`)
		}, templateexport.ReasonCommandOverridden},
		{"unbound instance", func(f *templateFixture) {
			mustCreateRow(t, f.st.DB().Model(&store.Instance{}).Where("id = ?", f.instance.ID).Update("gpu_idents", "").Error)
		}, templateexport.ReasonNoGPUFacts},
		{"template gone", func(f *templateFixture) { f.update(t, "template_id", 999) }, templateexport.ReasonTemplateMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTemplateFixture(t)
			tc.mutate(f)
			_, err := loadTemplateSource(f.st, f.deployment.ID, nil)
			var product *messages.ProductError
			if !errors.As(err, &product) || product.Params()["reason"] != tc.reason {
				t.Fatalf("error = %v, want reason %s", err, tc.reason)
			}
		})
	}
}

func TestLoadReportsAMissingDeployment(t *testing.T) {
	f := newTemplateFixture(t)
	if _, err := loadTemplateSource(f.st, f.deployment.ID+100, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error = %v, want store.ErrNotFound", err)
	}
}

func TestLoadStartsFromTheTemplateSpecTheDeploymentNames(t *testing.T) {
	f := newTemplateFixture(t)
	spec := func(name, source, args string) store.InferenceSpecRow {
		return store.InferenceSpecRow{Name: name, Source: source, Engine: "vllm", Image: "runner:vllm", Quantization: "bf16",
			GPUs: `{"vendor":"nvidia"}`, Args: args, Env: `{"TEMPLATE_ENV":"1"}`,
			WeightSources: `[{"provider":"huggingface","repo_id":"allenai/OLMo-2-0425-1B-Instruct","revision":"` + templateCommit + `"}]`}
	}
	if err := f.st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "allenai/olmo-2-0425-1b-instruct", Source: "upstream",
		Specs: []store.InferenceSpecRow{spec("bf16-vllm", "upstream", `["--upstream"]`)},
	}}); err != nil {
		t.Fatal(err)
	}
	// A user spec may share the upstream spec's name; the deployment's
	// recorded source says which one it was made from.
	if err := f.st.ImportUserInferenceCatalog([]store.InferenceModel{{
		ModelID: "allenai/olmo-2-0425-1b-instruct", Source: "user",
		Specs: []store.InferenceSpecRow{spec("bf16-vllm", "user", `["--mine"]`)},
	}}, false); err != nil {
		t.Fatal(err)
	}
	row, err := f.st.InferenceModelByModelID("allenai/olmo-2-0425-1b-instruct")
	if err != nil {
		t.Fatal(err)
	}
	f.update(t, "template_id", row.ID)
	f.update(t, "template_spec_name", "bf16-vllm")
	f.update(t, "template_spec_source", "upstream")
	f.update(t, "template_snapshot", `{"template_id":1,"spec_name":"bf16-vllm","spec_source":"upstream","quantization":"bf16","weight_provider":"huggingface","weight_repo_id":"allenai/OLMo-2-0425-1B-Instruct","weight_revision":"`+templateCommit+`","extra_args":["--upstream"],"env":{"TEMPLATE_ENV":"1"},"api":{"operations":[{"type":"openai.embeddings"}]}}`)
	mustCreateRow(t, f.st.DB().Model(&store.Model{}).Where("id = ?", f.model.ID).Update("operations", `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]`).Error)
	f.persistRuntimeLaunch(t)

	source, err := loadTemplateSource(f.st, f.deployment.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source.input.TemplateSpec == nil || source.input.Template == nil {
		t.Fatal("no template loaded")
	}
	if !slices.Equal(source.input.TemplateSpec.Args, []string{"--upstream"}) || !slices.Equal(source.input.TemplateArgs, []string{"--upstream"}) {
		t.Fatalf("spec args %q template args %q, want the upstream spec", source.input.TemplateSpec.Args, source.input.TemplateArgs)
	}
	if source.input.TemplateEnv["TEMPLATE_ENV"] != "1" {
		t.Fatalf("template env = %v", source.input.TemplateEnv)
	}
	if op, ok := source.contract.CheckOperation(); !ok || op.Type != inferenceapi.OperationEmbeddings || op.Endpoint == "" {
		t.Fatalf("contract = %+v, want the snapshot's normalized contract", source.contract)
	}

	f.update(t, "template_spec_source", "user")
	f.update(t, "template_snapshot", `{"template_id":1,"spec_name":"bf16-vllm","spec_source":"user","quantization":"bf16","weight_provider":"huggingface","weight_repo_id":"allenai/OLMo-2-0425-1B-Instruct","weight_revision":"`+templateCommit+`","extra_args":["--mine"],"env":{"TEMPLATE_ENV":"1"}}`)
	f.persistRuntimeLaunch(t)
	if source, err = loadTemplateSource(f.st, f.deployment.ID, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(source.input.TemplateSpec.Args, []string{"--mine"}) {
		t.Fatalf("spec args %q, want the user spec of the same name", source.input.TemplateSpec.Args)
	}
}

func (f *templateFixture) update(t *testing.T, column string, value any) {
	t.Helper()
	mustCreateRow(t, f.st.DB().Model(&store.Deployment{}).Where("id = ?", f.deployment.ID).Update(column, value).Error)
}

// The exporter end to end on stored rows: it tests the chat operation through
// the probe with the health check's body, leaves the deployment's own env out
// and answers the wire DTO.
func TestCatalogTemplatesExportsAStoredDeployment(t *testing.T) {
	f := newTemplateFixture(t)
	var sent []string
	exporter := catalogTemplates{store: f.st, probe: func(_ context.Context, id uint, _ string, path string, payload map[string]any) templateexport.ProbeReply {
		sent = append(sent, fmt.Sprintf("%d %s max_tokens=%v", id, path, payload["max_tokens"]))
		return templateexport.ProbeReply{HTTPStatus: 200, InstanceUUID: f.instance.UUID}
	}}
	out, err := exporter.Export(context.Background(), f.deployment.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{fmt.Sprintf("%d /v1/chat/completions max_tokens=1", f.deployment.ID)}; !slices.Equal(sent, want) {
		t.Fatalf("probes = %q, want %q", sent, want)
	}
	if out.ModelID != "allenai/olmo-2-0425-1b-instruct" || out.SpecName != fmt.Sprintf("vllm-1xnvidia-h20-d%d", f.deployment.ID) || !out.Saveable {
		t.Fatalf("export = %+v", out)
	}
	if !slices.Equal(out.ExcludedEnv, []string{"HF_TOKEN"}) || strings.Contains(out.YAML, "secret") {
		t.Fatalf("excluded env = %q; yaml leaks the token: %v", out.ExcludedEnv, strings.Contains(out.YAML, "secret"))
	}
	if len(out.Checks) != 1 || out.Checks[0].Status != proto.CheckPass || out.Checks[0].Code != messages.CheckTemplateOperationAnswered || out.Checks[0].Params.HTTPStatus != 200 || len(out.Notes) == 0 || out.Notes[0].Code != templateexport.NoteVerifiedOn {
		t.Fatalf("checks %+v notes %+v", out.Checks, out.Notes)
	}
	if _, err := inferencecatalog.ParseYAML([]byte(out.YAML)); err != nil {
		t.Fatalf("yaml does not import: %v", err)
	}

	f.update(t, "desired_state", store.DeployStopped)
	sent = nil
	if _, err := exporter.Export(context.Background(), f.deployment.ID, nil); err == nil || len(sent) != 0 {
		t.Fatalf("stopped deployment: err %v, probes %q", err, sent)
	}
}
