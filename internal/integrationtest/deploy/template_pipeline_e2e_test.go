package deploy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestInferenceTemplateDeploymentRejectsInvalidGPUSelectionsBeforeAgentWorkload(t *testing.T) {
	healthy, unhealthy := true, false
	type rejectionCase struct {
		name             string
		gpus             []testenv.GPUSpec
		placement        map[string]any
		wantCreateStatus int
		// previewRejects is a request the evaluate preview refuses as invalid;
		// otherwise it answers ok=false with the symbolic topology.
		previewRejects bool
	}
	cases := []rejectionCase{
		{
			name:             "predicate-does-not-match",
			wantCreateStatus: http.StatusBadRequest,
			previewRejects:   true,
			gpus: []testenv.GPUSpec{
				{Model: "Hygon K500", Vendor: "hygon", VRAMMB: 65536, Healthy: &healthy},
				{Model: "Hygon K500", Vendor: "hygon", VRAMMB: 65536, Healthy: &healthy},
			},
		},
		{
			name:             "insufficient-memory",
			wantCreateStatus: http.StatusBadRequest,
			gpus: []testenv.GPUSpec{
				{Model: "Hygon BW1000", Vendor: "hygon", VRAMMB: 16384, Healthy: &healthy},
				{Model: "Hygon BW1000", Vendor: "hygon", VRAMMB: 16384, Healthy: &healthy},
			},
		},
		{
			name:             "insufficient-card-count",
			wantCreateStatus: http.StatusBadRequest,
			gpus: []testenv.GPUSpec{
				{Model: "Hygon BW1100", Vendor: "hygon", VRAMMB: 65536, Healthy: &healthy},
			},
		},
		{
			name:             "only-unhealthy-cards",
			wantCreateStatus: http.StatusCreated,
			gpus: []testenv.GPUSpec{
				{Model: "Hygon K100_AI", Vendor: "hygon", VRAMMB: 65536, Healthy: &unhealthy},
				{Model: "Hygon BW1000", Vendor: "hygon", VRAMMB: 65536, Healthy: &unhealthy},
			},
		},
		{
			name:             "explicit-selection-too-small",
			wantCreateStatus: http.StatusBadRequest,
			gpus: []testenv.GPUSpec{
				{Model: "Hygon K100", Vendor: "hygon", VRAMMB: 65536, Healthy: &healthy},
				{Model: "Hygon BW1000", Vendor: "hygon", VRAMMB: 65536, Healthy: &healthy},
			},
			placement: map[string]any{"gpus": []int{0}},
		},
	}

	for _, fixture := range cases {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			env := testenv.New(t)
			node := env.AddNode("reject-"+fixture.name,
				testenv.WithGPUList(fixture.gpus...),
				testenv.WithDriverName("fake"),
				testenv.WithoutAcceleratorRuntimeInjection(),
			)
			runs := make(chan runtime.WorkloadSpec, 1)
			node.Driver.AfterRun(func(spec runtime.WorkloadSpec) {
				if strings.HasPrefix(spec.Name, "gpuworks-partition-probe-") {
					return
				}
				select {
				case runs <- spec:
				default:
				}
			})
			env.WaitNodeOnline(t, node.Name)
			nodes, err := env.Store().ListNodes()
			if err != nil || len(nodes) != 1 {
				t.Fatalf("nodes=%+v err=%v", nodes, err)
			}

			modelID := "gpuworks/template-reject-" + fixture.name
			model, err := env.Store().UpsertModel(&store.Model{
				Source: "huggingface", SourceID: modelID, Revision: "main", Name: modelID,
				Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 20,
				Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":4096}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "reject")}); err != nil {
				t.Fatal(err)
			}
			resolver := "def resolve(gpu):\n" +
				"    if gpu.count < 2:\n        return reject(\"at least two GPUs are required\")\n" +
				"    if gpu.min_memory_gib < 32:\n        return reject(\"at least 32 GiB per GPU is required\")\n" +
				"    return {\"GPU_COUNT\": gpu.count, \"TP_SIZE\": gpu.count}\n"
			catalog := store.InferenceModel{
				ModelID: modelID, DisplayTitle: modelID, Source: "upstream", Architecture: "Qwen3ForCausalLM", ContextLength: 4096,
				Specs: []store.InferenceSpecRow{{
					Name: "hygon-general", Scenario: "balanced", Engine: "vllm", Image: "gpuworks/runtime-hygon:test",
					Quantization: "bf16", WeightBytes: 1 << 20,
					WeightSources: fmt.Sprintf(`[{"provider":"huggingface","repo_id":%q,"revision":"main"}]`, modelID),
					GPUs:          `{"vendor":"hygon","name":"(^|[^A-Za-z0-9])(?:K100(?:[_ -]?AI)?|BW(?:[_ -]?[0-9]+)?)([^A-Za-z0-9]|$)"}`,
					Args:          `["--tensor-parallel-size","${TP_SIZE}"]`, Env: `{}`,
					Placement: fmt.Sprintf(`{"language":"starlark","resolver":%q}`, resolver),
				}},
			}
			if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
				t.Fatal(err)
			}
			template, err := env.Store().InferenceModelByModelID(modelID)
			if err != nil {
				t.Fatal(err)
			}
			request := map[string]any{
				"template_id": template.ID, "template_spec_source": "upstream", "template_spec_name": "hygon-general", "node_uuid": nodes[0].UUID,
				"served_name": fixture.name, "placement": fixture.placement,
			}
			body, _ := json.Marshal(request)
			api := env.API(t)
			preview, err := api.PostJSON("/api/v1/evaluate", body)
			if err != nil {
				t.Fatal(err)
			}
			previewRaw, _ := io.ReadAll(preview.Body)
			preview.Body.Close()
			if fixture.previewRejects {
				if preview.StatusCode != http.StatusBadRequest {
					t.Fatalf("preview status=%d body=%s", preview.StatusCode, previewRaw)
				}
			} else {
				var evaluated struct {
					OK      bool     `json:"ok"`
					Command []string `json:"command"`
				}
				if preview.StatusCode != http.StatusOK || json.Unmarshal(previewRaw, &evaluated) != nil || evaluated.OK {
					t.Fatalf("preview status=%d body=%s", preview.StatusCode, previewRaw)
				}
				if command := strings.Join(evaluated.Command, " "); !strings.Contains(command, "$TP_SIZE") {
					t.Fatalf("unschedulable preview lost symbolic topology: %v", evaluated.Command)
				}
			}

			response, err := api.PostJSON("/api/v1/deployments", body)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != fixture.wantCreateStatus {
				t.Fatalf("status=%d body=%s", response.StatusCode, raw)
			}
			// A selection no card of the node could ever satisfy is refused at
			// create; one that only waits for device health is accepted so it
			// can recover. The evaluate response exposes either synchronously,
			// and neither path may send a partial workload to the agent.
			if fixture.wantCreateStatus == http.StatusBadRequest && !fixture.previewRejects && !strings.Contains(string(raw), "DEPLOYMENT_NEVER_FITS") {
				t.Fatalf("never-fitting create body=%s", raw)
			}
			t.Logf("preview=%s create=%s", previewRaw, raw)
			select {
			case workload := <-runs:
				t.Fatalf("rejected selection reached the agent: %+v", workload)
			case <-time.After(500 * time.Millisecond):
			}
			instances, err := env.Store().ListInstances()
			if err != nil || len(instances) != 0 {
				t.Fatalf("rejected selection persisted instances=%+v err=%v", instances, err)
			}
		})
	}
}

// TestInferenceTemplateDeploymentPipeline is the backend acceptance seam from
// a catalog fact to the exact workload observed by the container runtime. GPU
// product compatibility lives only in the fixture predicates; none of the
// production API/controller/scheduler code knows these SKU names.
func TestInferenceTemplateDeploymentPipeline(t *testing.T) {
	env := testenv.New(t)
	healthy, unhealthy := true, false
	type nodeCase struct {
		name       string
		gpus       []testenv.GPUSpec
		wantSpec   string
		wantGPU    []int
		override   bool
		wantReject bool
	}
	cases := []nodeCase{
		{name: "nvidia-turing", gpus: []testenv.GPUSpec{{Model: "NVIDIA GeForce RTX 2080", Vendor: "nvidia", ComputeCapability: "7.5", VRAMMB: 8192, Healthy: &healthy}}, wantSpec: "nvidia-general", wantGPU: []int{0}},
		{name: "ascend-successor", gpus: []testenv.GPUSpec{{Model: "Ascend 910C", Vendor: "ascend", VRAMMB: 65536, Healthy: &healthy}}, wantSpec: "ascend-general", wantGPU: []int{0}, override: true},
		{name: "hygon-successor", gpus: []testenv.GPUSpec{{Model: "Hygon K100", Vendor: "hygon", VRAMMB: 65536, Healthy: &unhealthy}, {Model: "Hygon BW1000", Vendor: "hygon", VRAMMB: 65536, Healthy: &healthy}}, wantSpec: "hygon-general", wantGPU: []int{1}},
		{name: "hygon-next-successor", gpus: []testenv.GPUSpec{{Model: "Hygon BW1100", Vendor: "hygon", VRAMMB: 65536, Healthy: &healthy}}, wantSpec: "hygon-general", wantGPU: []int{0}},
		{name: "hygon-outside-family", gpus: []testenv.GPUSpec{{Model: "Hygon K500", Vendor: "hygon", VRAMMB: 65536, Healthy: &healthy}}, wantReject: true},
	}

	type runningNode struct {
		fixture nodeCase
		node    *testenv.Node
		runs    chan runtime.WorkloadSpec
	}
	var running []runningNode
	for _, fixture := range cases {
		node := env.AddNode(fixture.name, testenv.WithGPUList(fixture.gpus...), testenv.WithDriverName("fake"), testenv.WithoutAcceleratorRuntimeInjection())
		runs := make(chan runtime.WorkloadSpec, 1)
		runChannel := runs
		node.Driver.AfterRun(func(spec runtime.WorkloadSpec) {
			if strings.HasPrefix(spec.Name, "gpuworks-partition-probe-") {
				return
			}
			select {
			case runChannel <- spec:
			default:
			}
		})
		env.WaitNodeOnline(t, fixture.name)
		running = append(running, runningNode{fixture: fixture, node: node, runs: runs})
	}

	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "gpuworks/template-pipeline", Revision: "main", Name: "template-pipeline",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 20,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":4096}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := env.Store().ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	byUUID := make(map[string]store.Node, len(nodes))
	pipelineDir := testenv.ModelDir(t, "template-pipeline")
	for _, node := range nodes {
		byUUID[node.UUID] = node
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: pipelineDir}); err != nil {
			t.Fatal(err)
		}
	}

	const sources = `[{"provider":"huggingface","repo_id":"gpuworks/template-pipeline","revision":"main"}]`
	catalog := store.InferenceModel{ModelID: "gpuworks/template-pipeline", Source: "upstream", Architecture: "Qwen3ForCausalLM", ContextLength: 4096, Specs: []store.InferenceSpecRow{
		{Name: "nvidia-general", Engine: "vllm", Image: "gpuworks/runtime-nvidia:test", Quantization: "bf16", WeightSources: sources, WeightBytes: 1 << 20, GPUs: `{"vendor":"nvidia","cc":">=7.5"}`, Args: `["--max-model-len","1024"]`, Env: `{"CATALOG_LAYER":"nvidia"}`},
		{Name: "ascend-general", Engine: "sglang", Image: "gpuworks/runtime-ascend:test", Quantization: "bf16", WeightSources: sources, WeightBytes: 1 << 20, GPUs: `{"vendor":"ascend","name":"(^|[^A-Za-z0-9])(?:910B4?|910C|950DT|950PR)([^A-Za-z0-9]|$)"}`, Args: `["--context-length","1024"]`, Env: `{"CATALOG_LAYER":"ascend"}`},
		{Name: "hygon-general", Engine: "vllm", Image: "gpuworks/runtime-hygon:test", Quantization: "bf16", WeightSources: sources, WeightBytes: 1 << 20, GPUs: `{"vendor":"hygon","name":"(^|[^A-Za-z0-9])(?:K100(?:[_ -]?AI)?|BW(?:[_ -]?[0-9]+)?)([^A-Za-z0-9]|$)"}`, Args: `["--max-model-len","1024"]`, Env: `{"CATALOG_LAYER":"hygon"}`},
	}}
	if err := env.Store().ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	template, err := env.Store().InferenceModelByModelID(catalog.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	api := env.API(t)

	for _, item := range running {
		item := item
		t.Run(item.fixture.name, func(t *testing.T) {
			port := freeTemplatePipelinePort(t)
			env.SetControllerEnginePorts(port, port)
			node := byUUID[item.node.UUID]
			request := map[string]any{
				"template_id": template.ID, "node_uuid": node.UUID, "served_name": item.fixture.name,
				"quick_params": map[string]any{}, "placement": map[string]any{},
			}
			if item.fixture.override {
				request["extra_args"] = "--context-length 2048"
				request["container_overrides"] = map[string]any{"env": map[string]string{"REQUEST_LAYER": "temporary"}}
			}
			body, _ := json.Marshal(request)
			response, err := api.PostJSON("/api/v1/deployments", body)
			if err != nil {
				t.Fatal(err)
			}
			if item.fixture.wantReject {
				defer response.Body.Close()
				if response.StatusCode != http.StatusBadRequest {
					raw, _ := io.ReadAll(response.Body)
					t.Fatalf("status=%d body=%s", response.StatusCode, raw)
				}
				select {
				case workload := <-item.runs:
					t.Fatalf("rejected GPU received workload: %+v", workload)
				case <-time.After(150 * time.Millisecond):
				}
				return
			}
			if response.StatusCode != http.StatusCreated {
				raw, _ := io.ReadAll(response.Body)
				response.Body.Close()
				t.Fatalf("status=%d body=%s", response.StatusCode, raw)
			}
			var created struct {
				ID                 uint   `json:"id"`
				TemplateSpecSource string `json:"template_spec_source"`
				TemplateSpecName   string `json:"template_spec_name"`
				TemplateDeviated   bool   `json:"template_deviated"`
			}
			if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
				response.Body.Close()
				t.Fatal(err)
			}
			response.Body.Close()
			if created.TemplateSpecSource != "upstream" || created.TemplateSpecName != item.fixture.wantSpec || created.TemplateDeviated != item.fixture.override {
				t.Fatalf("created=%+v", created)
			}
			var workload runtime.WorkloadSpec
			select {
			case workload = <-item.runs:
			case <-time.After(20 * time.Second):
				instances, _ := env.Store().InstancesForDeployment(created.ID)
				t.Fatalf("agent did not execute workload; instances=%+v", instances)
			}
			var instances []store.Instance
			env.Eventually(t, 5*time.Second, func() bool {
				instances, _ = env.Store().InstancesForDeployment(created.ID)
				return len(instances) == 1
			})
			if len(instances) != 1 {
				t.Fatalf("instances=%+v", instances)
			}
			var desired proto.InstanceDesiredState
			if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &desired); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(desired.GPUIndexes, item.fixture.wantGPU) || !reflect.DeepEqual(workload.GPUIndexes, item.fixture.wantGPU) {
				t.Fatalf("GPU desired=%v workload=%v want=%v", desired.GPUIndexes, workload.GPUIndexes, item.fixture.wantGPU)
			}
			// A pre-start script (the platform's SGLang severity block) makes the
			// agent wrap the launch: /bin/sh runs the wrapper and then execs the
			// original entrypoint/argv boundaries, so parity means wrapper shape
			// plus intact boundaries, not a literal entrypoint match.
			if desired.PreStartScript != "" {
				wantCmd := append(append([]string{}, desired.Entrypoint...), desired.Argv...)
				if len(workload.Entrypoint) != 2 || workload.Entrypoint[0] != "/bin/sh" ||
					!reflect.DeepEqual(workload.Cmd, wantCmd) || !reflect.DeepEqual(desired.Env, workload.Env) {
					t.Fatalf("desired/workload drift: desired=%+v workload=%+v", desired.ResolvedLaunch, workload)
				}
				preStartMounted := false
				for _, mount := range workload.Mounts {
					if mount.Target == "/gpuworks/pre-start.sh" && mount.ReadOnly {
						preStartMounted = true
					}
				}
				if !preStartMounted {
					t.Fatalf("pre-start wrapper not mounted read-only: %+v", workload.Mounts)
				}
			} else if !reflect.DeepEqual(desired.Entrypoint, workload.Entrypoint) || !reflect.DeepEqual(desired.Argv, workload.Cmd) || !reflect.DeepEqual(desired.Env, workload.Env) {
				t.Fatalf("desired/workload drift: desired=%+v workload=%+v", desired.ResolvedLaunch, workload)
			}
			if workload.Env["CATALOG_LAYER"] != strings.TrimSuffix(item.fixture.wantSpec, "-general") {
				t.Fatalf("catalog env=%v", workload.Env)
			}
			joined := strings.Join(append(append([]string{}, workload.Entrypoint...), workload.Cmd...), " ")
			wantContext := "1024"
			if item.fixture.override {
				wantContext = "2048"
				if workload.Env["REQUEST_LAYER"] != "temporary" {
					t.Fatalf("temporary env override missing: %v", workload.Env)
				}
			}
			if !strings.Contains(joined, wantContext) || !containsRuntimeMount(workload.Mounts, pipelineDir, desired.ModelMount, true) {
				t.Fatalf("final workload=%+v", workload)
			}
		})
	}
}

func TestMultiComponentTemplatePreviewAndDeploymentUseTheSameLaunch(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("multi-component", testenv.WithGPUList(testenv.GPUSpec{Model: "NVIDIA A100", Vendor: "nvidia", ComputeCapability: "8.0", VRAMMB: 81920}), testenv.WithDriverName("fake"))
	runs := make(chan runtime.WorkloadSpec, 1)
	node.Driver.AfterRun(func(spec runtime.WorkloadSpec) {
		select {
		case runs <- spec:
		default:
		}
	})
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	artifacts := map[string]*store.Model{}
	weightDirs := map[string]string{}
	for _, name := range []string{"target", "draft", "verifier"} {
		artifacts[name], err = env.Store().UpsertModel(&store.Model{
			Source: "huggingface", SourceID: "gpuworks/component-" + name, Revision: "main", Name: name,
			Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 20,
			Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":4096}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		weightDirs[name] = testenv.ModelDir(t, name)
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: artifacts[name].ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: weightDirs[name]}); err != nil {
			t.Fatal(err)
		}
	}
	const draftSpec = `[{"provider":"huggingface","repo_id":"gpuworks/component-draft","revision":"main"}]`
	const verifierSpec = `[{"provider":"huggingface","repo_id":"gpuworks/component-verifier","revision":"main"}]`
	components := `[
		{"name":"target","role":"primary","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"gpuworks/component-target","revision":"main"}]},"weight_bytes":1048576,"mount_path":"/models/target"},
		{"name":"draft","role":"speculative_draft","model_id":"gpuworks/component-draft","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"gpuworks/component-draft","revision":"main"}]},"weight_bytes":1048576,"mount_path":"/models/draft"},
		{"name":"verifier","role":"verifier","model_id":"gpuworks/component-verifier","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"gpuworks/component-verifier","revision":"main"}]},"weight_bytes":1048576,"mount_path":"/models/verifier"}
	]`
	catalog := []store.InferenceModel{
		{ModelID: "gpuworks/component-target", Source: "upstream", Architecture: "Qwen3ForCausalLM", Specs: []store.InferenceSpecRow{{
			Name: "nvidia-speculative", Engine: "vllm", Image: "gpuworks/runtime-component:test", Quantization: "bf16", WeightBytes: 1 << 20,
			WeightSources: `[{"provider":"huggingface","repo_id":"gpuworks/component-target","revision":"main"}]`, GPUs: `{"vendor":"nvidia","cc":">=8.0"}`,
			Args: `["--model","/models/target","--speculative-draft-model","/models/draft","--speculative-verifier","/models/verifier"]`, Env: `{}`, Components: components,
		}}},
		{ModelID: "gpuworks/component-draft", Source: "upstream", Specs: []store.InferenceSpecRow{{Name: "draft", Engine: "vllm", Image: "image", Quantization: "bf16", WeightBytes: 1 << 20, WeightSources: draftSpec, GPUs: `{"vendor":"nvidia","cc":">=8.0"}`, Args: `[]`, Env: `{}`}}},
		{ModelID: "gpuworks/component-verifier", Source: "upstream", Specs: []store.InferenceSpecRow{{Name: "verifier", Engine: "vllm", Image: "image", Quantization: "bf16", WeightBytes: 1 << 20, WeightSources: verifierSpec, GPUs: `{"vendor":"nvidia","cc":">=8.0"}`, Args: `[]`, Env: `{}`}}},
	}
	if err := env.Store().ReconcileInferenceCatalog(catalog); err != nil {
		t.Fatal(err)
	}
	template, err := env.Store().InferenceModelByModelID("gpuworks/component-target")
	if err != nil {
		t.Fatal(err)
	}
	port := freeTemplatePipelinePort(t)
	env.SetControllerEnginePorts(port, port)
	request := map[string]any{"template_id": template.ID, "template_spec_source": "upstream", "template_spec_name": "nvidia-speculative", "node_uuid": nodes[0].UUID, "served_name": "multi-component", "quick_params": map[string]any{}, "placement": map[string]any{}}
	body, _ := json.Marshal(request)
	api := env.API(t)
	previewResponse, err := api.PostJSON("/api/v1/evaluate", body)
	if err != nil {
		t.Fatal(err)
	}
	if previewResponse.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(previewResponse.Body)
		previewResponse.Body.Close()
		t.Fatalf("preview status=%d body=%s", previewResponse.StatusCode, raw)
	}
	var preview struct {
		Command    []string `json:"command"`
		ModelMount string   `json:"model_mount_path"`
		Mounts     []struct {
			Source   string `json:"source"`
			Target   string `json:"target"`
			ReadOnly bool   `json:"read_only"`
		} `json:"mounts"`
	}
	if err := json.NewDecoder(previewResponse.Body).Decode(&preview); err != nil {
		previewResponse.Body.Close()
		t.Fatal(err)
	}
	previewResponse.Body.Close()
	if preview.ModelMount != "/models/target" || !containsPreviewMount(preview.Mounts, weightDirs["draft"], "/models/draft") || !containsPreviewMount(preview.Mounts, weightDirs["verifier"], "/models/verifier") {
		t.Fatalf("preview=%+v", preview)
	}
	response, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create status=%d body=%s", response.StatusCode, raw)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	var workload runtime.WorkloadSpec
	select {
	case workload = <-runs:
	case <-time.After(20 * time.Second):
		t.Fatal("agent did not execute multi-component workload")
	}
	var instances []store.Instance
	env.Eventually(t, 5*time.Second, func() bool {
		instances, _ = env.Store().InstancesForDeployment(created.ID)
		return len(instances) == 1
	})
	var desired proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &desired); err != nil {
		t.Fatal(err)
	}
	if desired.ModelPath != weightDirs["target"] || desired.ModelMount != "/models/target" || !containsRuntimeMount(workload.Mounts, weightDirs["target"], "/models/target", true) || !containsRuntimeMount(workload.Mounts, weightDirs["draft"], "/models/draft", true) || !containsRuntimeMount(workload.Mounts, weightDirs["verifier"], "/models/verifier", true) {
		t.Fatalf("desired=%+v workload=%+v", desired, workload)
	}
	actualCommand := append(append([]string{}, workload.Entrypoint...), workload.Cmd...)
	if !reflect.DeepEqual(commandWithoutEphemeralPort(actualCommand), commandWithoutEphemeralPort(preview.Command)) || !reflect.DeepEqual(desired.Entrypoint, workload.Entrypoint) || !reflect.DeepEqual(desired.Argv, workload.Cmd) {
		t.Fatalf("preview=%v desired=%v/%v workload=%v/%v", preview.Command, desired.Entrypoint, desired.Argv, workload.Entrypoint, workload.Cmd)
	}
}

func commandWithoutEphemeralPort(command []string) []string {
	result := append([]string(nil), command...)
	for index := 0; index+1 < len(result); index++ {
		if result[index] == "--port" {
			result[index+1] = "$PORT"
		}
	}
	return result
}

func containsPreviewMount(mounts []struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}, source, target string) bool {
	for _, mount := range mounts {
		if mount.Source == source && mount.Target == target && mount.ReadOnly {
			return true
		}
	}
	return false
}

func freeTemplatePipelinePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func containsRuntimeMount(mounts []runtime.Mount, source, target string, readOnly bool) bool {
	for _, mount := range mounts {
		if mount.Source == source && mount.Target == target && mount.ReadOnly == readOnly {
			return true
		}
	}
	return false
}
