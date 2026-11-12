package planning

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PlanLaunch is the one assembly both callers use, so its estimate input has to
// carry the catalog facts whoever asks: the spec's weight, the loader layout's
// resident weight, the measured KV cost and the node's cache record. The
// controller used to omit the last three, which is why the breakdown it
// persisted could disagree with what the form had shown.
func TestPlanLaunchSpendsTheSameCatalogFactsForEveryCaller(t *testing.T) {
	st, err := store.Open("file:planning_plan_launch?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "org/fixture-8b", Source: "upstream", Architecture: "qwen3",
		KVBytesPerToken: `{"vllm":262144}`,
		Specs: []store.InferenceSpecRow{{
			Name: "balanced", Engine: "vllm", Image: "registry.example/app:v1", Quantization: "bf16",
			WeightBytes: 8 << 30, RuntimeWeightBytes: 6 << 30,
			WeightSources: `[{"provider":"huggingface","repo_id":"org/fixture-8b"}]`,
			GPUs:          `{"vendor":"nvidia","min_count":1}`,
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("plan-launch-node", store.HashToken("token"), "plan-launch-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/fixture-8b", Name: "fixture", WeightBytes: 0, Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := deploy.DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"balanced","spec_source":"upstream","weight_bytes":8589934592,"runtime_weight_bytes":6442450944,"gpus":{"vendor":"nvidia","min_count":1},"extra_args":["--dtype","bfloat16"]}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanLaunch(context.Background(), st, storeNetworks(st), LaunchRequest{
		Engine: "vllm", Model: model, Node: node, Selection: selection,
		Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1},
		Source:     LaunchSourceFromDeployment(&store.Deployment{ID: 5, Image: "registry.example/app:v1"}),
		ModelMount: deploy.DefaultModelMount, ServedName: "fixture", APIPort: 30000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Output.Result.OK || plan.Output.Result.Plan == nil {
		t.Fatalf("plan did not fit: %+v", plan.Output.Result)
	}
	// The catalog's measured KV cost reaches the breakdown: a token capacity is
	// only claimed with it.
	if plan.Output.Breakdown.EstimatedKVTokens <= 0 || plan.Output.Breakdown.KVEstimateConfidence != "measured" {
		t.Fatalf("breakdown = %+v", plan.Output.Breakdown)
	}
	if plan.Params.TensorParallel != 1 || plan.Params.AssignedGPUs != 1 || plan.Params.ModelPath != deploy.DefaultModelMount {
		t.Fatalf("params = %+v", plan.Params)
	}
	if plan.Resolved.ModelMount != deploy.DefaultModelMount || len(plan.Resolved.Argv) == 0 {
		t.Fatalf("resolved = %+v", plan.Resolved)
	}
	if !strings.Contains(strings.Join(plan.Resolved.Argv, " "), "--dtype bfloat16") {
		t.Fatalf("template arguments did not reach the command: %v", plan.Resolved.Argv)
	}
	// An assignment the node cannot satisfy is a plan that does not fit, not an
	// error and never a launch on some other card: the caller reports it.
	unfittable, err := PlanLaunch(context.Background(), st, storeNetworks(st), LaunchRequest{
		Engine: "vllm", Model: model, Node: node, Selection: selection,
		Placement: Placement{GPUIndexes: []int{3}}, Topology: DistTopology{NNodes: 1},
		Source: LaunchSourceFromDeployment(&store.Deployment{ID: 5, Image: "registry.example/app:v1"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if unfittable.Output.Result.OK || unfittable.Output.Result.Plan != nil || len(unfittable.Resolved.Argv) != 0 {
		t.Fatalf("unfittable assignment produced a launch: %+v", unfittable.Output.Result)
	}
}

// The acceptance the plan asks for: one deployment row, planned once the way the
// controller plans it and once the way the preview does, has to come out the
// same - same estimate input, same params but for the port a preview cannot
// reserve and the GPU count only a schedule can settle.
func TestPlanLaunchAgreesBetweenTheControllerAndThePreview(t *testing.T) {
	st, err := store.Open("file:planning_plan_launch_parity?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/fixture-8b", Name: "fixture", WeightBytes: 4 << 30, Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("parity-node", store.HashToken("token"), "parity-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	selection, err := deploy.DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"balanced","spec_source":"upstream","weight_bytes":4294967296,"runtime_weight_bytes":4294967296,"extra_args":["--dtype","bfloat16"]}`))
	if err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{ID: 11, ModelID: model.ID, NodeID: node.ID, ServedName: "fixture", Engine: "vllm", Image: "registry.example/app:v1", ContainerOverrides: `{"env":{"USER":"1"}}`}
	request := LaunchRequest{
		Engine: "vllm", Model: model, Node: node, Selection: selection,
		Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1, Role: "leader"},
		Quick: deploy.LaunchConfig{MaxModelLen: 4096}, ExtraArgs: []string{"--enable-metrics"},
		ModelMount: deploy.DefaultModelMount, ServedName: "fixture",
	}
	controller := request
	controller.APIPort = 31000
	controller.Source = LaunchSourceFromDeployment(dep)
	controllerPlan, err := PlanLaunch(context.Background(), st, storeNetworks(st), controller)
	if err != nil {
		t.Fatal(err)
	}
	preview := request
	preview.Preview = true
	preview.Source = LaunchSourceFromRequest(dep.Image, LaunchSourceFromDeployment(dep).Overrides)
	previewPlan, err := PlanLaunch(context.Background(), st, storeNetworks(st), preview)
	if err != nil {
		t.Fatal(err)
	}
	if !controllerPlan.Output.Result.OK || !previewPlan.Output.Result.OK {
		t.Fatalf("controller=%+v preview=%+v", controllerPlan.Output.Result, previewPlan.Output.Result)
	}
	// Only the controller's launch names its deployment: a preview's has
	// none yet, or an edit's is excluded whole (ExcludeDeploymentID).
	if controllerPlan.Input.DeploymentID != dep.ID || previewPlan.Input.DeploymentID != 0 {
		t.Fatalf("deployment ids = %d, %d", controllerPlan.Input.DeploymentID, previewPlan.Input.DeploymentID)
	}
	previewPlan.Input.DeploymentID = dep.ID
	if !reflect.DeepEqual(controllerPlan.Input, previewPlan.Input) {
		t.Fatalf("inputs differ:\n controller %+v\n preview    %+v", controllerPlan.Input, previewPlan.Input)
	}
	controllerPlan.Params.APIPort, previewPlan.Params.APIPort = 0, 0
	controllerPlan.Params.AssignedGPUs, previewPlan.Params.AssignedGPUs = 0, 0
	if !reflect.DeepEqual(controllerPlan.Params, previewPlan.Params) {
		t.Fatalf("params differ:\n controller %+v\n preview    %+v", controllerPlan.Params, previewPlan.Params)
	}
	if controllerPlan.Source.Image != previewPlan.Source.Image || !reflect.DeepEqual(controllerPlan.Source.Overrides, previewPlan.Source.Overrides) {
		t.Fatalf("sources differ: %+v vs %+v", controllerPlan.Source, previewPlan.Source)
	}
}

// The runveil thread caps come from the node the launch is planned on, so the
// preview the form shows and the launch the controller sends carry the same
// keys, and a key the deployment removed is gone from both.
func TestPlanLaunchCapsSGLangThreadsOnRunveilNodes(t *testing.T) {
	st, err := store.Open("file:planning_plan_launch_runveil?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/fixture-8b", Name: "fixture", WeightBytes: 4 << 30, Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("runveil-node", store.HashToken("token"), "runveil-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0"}}
	selection, err := deploy.DecodeCatalogSelection([]byte(`{"engine":"sglang","spec_name":"balanced","spec_source":"upstream","weight_bytes":4294967296,"runtime_weight_bytes":4294967296}`))
	if err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{ID: 12, ModelID: model.ID, NodeID: node.ID, ServedName: "fixture", Engine: "sglang", Image: "registry.example/sglang:v1", ContainerOverrides: `{"env_unset":["TORCH_NUM_THREADS"]}`}
	request := LaunchRequest{
		Engine: "sglang", Model: model, Node: node, Selection: selection,
		Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1, Role: "leader"},
		ModelMount: deploy.DefaultModelMount, ServedName: "fixture",
	}
	for _, tc := range []struct {
		runtime string
		want    string
	}{{"udocker", "4"}, {"docker", ""}} {
		node.RuntimeDriver = tc.runtime
		controller := request
		controller.APIPort = 31000
		controller.Source = LaunchSourceFromDeployment(dep)
		preview := request
		preview.Preview = true
		preview.Source = LaunchSourceFromRequest(dep.Image, LaunchSourceFromDeployment(dep).Overrides)
		for name, req := range map[string]LaunchRequest{"controller": controller, "preview": preview} {
			plan, err := PlanLaunch(context.Background(), st, storeNetworks(st), req)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Resolved.Env["OMP_NUM_THREADS"]; got != tc.want {
				t.Fatalf("%s on %s: OMP_NUM_THREADS = %q, want %q (env %v)", name, tc.runtime, got, tc.want, plan.Resolved.Env)
			}
			if _, kept := plan.Resolved.Env["TORCH_NUM_THREADS"]; kept {
				t.Fatalf("%s on %s: removed TORCH_NUM_THREADS is still set", name, tc.runtime)
			}
		}
	}
}

func TestLaunchInputBuildsCommandFeaturesOnlyForOwnedAMD(t *testing.T) {
	selection := deploy.CatalogSelection{
		Entrypoint: []string{"sglang", "serve"},
		ExtraArgs:  []string{"--from-template"},
	}
	manual := deploy.ContainerOverrides{
		CommandMode: string(deploy.CommandManual),
		CmdDisplay:  `["sglang","serve","--enable-sleep-mode=true"]`,
	}
	for _, tc := range []struct {
		name string
		node *store.Node
		want bool
	}{
		{name: "nvidia", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "nvidia"}}}},
		{name: "huawei", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "ascend"}}}},
		{name: "apple", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "apple"}}}},
		{name: "amd", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "amd"}}, GPUVendor: "amd"}, want: true},
		{name: "mixed includes owned amd", node: &store.Node{GPUs: []store.GPU{{Index: 0, Vendor: "nvidia"}, {Index: 1, Vendor: "amd"}}}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := launchInput(nil, LaunchRequest{
				Engine: "sglang", Node: tc.node, Selection: selection,
				Source:    LaunchSourceFromRequest("image", manual),
				ExtraArgs: []string{"--from-request"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if input.RequestedFeatures == nil || !input.RequestedFeatures.CommandKnown {
				t.Fatalf("missing resolved snapshot: %+v", input.RequestedFeatures)
			}

		})
	}
}

func TestLaunchParamsSupplyManagementInterfaceForKunlunMultiGPU(t *testing.T) {
	node := &store.Node{
		GPUVendor: "kunlunxin", NetworkInventory: `{"management":{"interface":"br_eno1_31","ip":"172.25.17.112"}}`,
		GPUs: []store.GPU{{Index: 0, Vendor: "kunlunxin"}, {Index: 1, Vendor: "kunlunxin"}},
	}
	params := launchParams(LaunchRequest{Node: node, Topology: DistTopology{NNodes: 1}}, node.GPUs[0], 2, parallelism.Spec{TP: 2}, 30000)
	if params.ComputeNetwork.Type != "host_ethernet" || params.ComputeNetwork.Interface != "br_eno1_31" {
		t.Fatalf("multi-GPU compute network = %+v", params.ComputeNetwork)
	}
	for _, parallel := range []parallelism.Spec{{TP: 1, PP: 2}, {TP: 1, DP: 2}} {
		params = launchParams(LaunchRequest{Node: node, Topology: DistTopology{NNodes: 1}}, node.GPUs[0], 0, parallel, 30000)
		if params.ComputeNetwork.Type != "host_ethernet" || params.ComputeNetwork.Interface != "br_eno1_31" {
			t.Fatalf("parallel %+v compute network = %+v", parallel, params.ComputeNetwork)
		}
	}
	params = launchParams(LaunchRequest{Node: node, Topology: DistTopology{NNodes: 1}}, node.GPUs[0], 1, parallelism.Spec{TP: 1}, 30000)
	if params.ComputeNetwork.Type != "" {
		t.Fatalf("single-card network changed = %+v", params.ComputeNetwork)
	}
	selected := deploy.ComputeNetwork{Type: "host_rdma", Interface: "bond0"}
	params = launchParams(LaunchRequest{Node: node, Topology: DistTopology{NNodes: 1, Network: selected}}, node.GPUs[0], 2, parallelism.Spec{TP: 2}, 30000)
	if !reflect.DeepEqual(params.ComputeNetwork, selected) {
		t.Fatalf("selected network replaced = %+v", params.ComputeNetwork)
	}
	nvidia := &store.Node{GPUVendor: "nvidia", NetworkInventory: node.NetworkInventory, GPUs: []store.GPU{{Index: 0, Vendor: "nvidia"}, {Index: 1, Vendor: "nvidia"}}}
	params = launchParams(LaunchRequest{Node: nvidia, Topology: DistTopology{NNodes: 1}}, nvidia.GPUs[0], 2, parallelism.Spec{TP: 2}, 30000)
	if params.ComputeNetwork.Type != "" {
		t.Fatalf("NVIDIA network changed = %+v", params.ComputeNetwork)
	}
}

// storeNetworks is the network service over every network in st, as the app
// wires it.
func storeNetworks(st *store.Store) Networks {
	return network.New(st, network.StoreCatalog{Store: st})
}

// The PD launch says whether the model has recurrent layers, read from its
// config.json (Qwen3.5 nests layer_types under text_config), so the engine
// builder can turn on what the mode needs for them. The rank's PD launch,
// shared with the request, is not written.
func TestLaunchParamsMarksRecurrentLayersOnPD(t *testing.T) {
	node := &store.Node{GPUVendor: "hygon", GPUs: []store.GPU{{Index: 0, Vendor: "hygon"}}}
	hybrid := &store.Model{Meta: `{"architectures":["Qwen3_5ForConditionalGeneration"],"model_type":"qwen3_5",
		"text_config":{"model_type":"qwen3_5_text","num_hidden_layers":4,"hidden_size":1024,"num_attention_heads":8,
		"num_key_value_heads":2,"head_dim":256,"layer_types":["linear_attention","linear_attention","linear_attention","full_attention"]}}`}
	dense := &store.Model{Meta: `{"model_type":"qwen3","num_hidden_layers":28,"hidden_size":1024,"num_attention_heads":16,"num_key_value_heads":8,"head_dim":128}`}
	for _, c := range []struct {
		name  string
		model *store.Model
		want  bool
	}{
		{"gated delta net hybrid", hybrid, true},
		{"full attention", dense, false},
		{"no model record", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			shared := &deploy.PDLaunch{Mode: "vllm-mooncake", Role: "decode"}
			req := LaunchRequest{Node: node, Model: c.model, Topology: DistTopology{NNodes: 1}, PD: shared}
			params := launchParams(req, node.GPUs[0], 1, parallelism.Spec{TP: 1}, 30000)
			if params.PD == nil || params.PD.RecurrentLayers != c.want {
				t.Fatalf("PD launch = %+v, want RecurrentLayers %v", params.PD, c.want)
			}
			if shared.RecurrentLayers {
				t.Fatal("the request's shared PD launch was written")
			}
		})
	}
	params := launchParams(LaunchRequest{Node: node, Model: hybrid, Topology: DistTopology{NNodes: 1}}, node.GPUs[0], 1, parallelism.Spec{TP: 1}, 30000)
	if params.PD != nil {
		t.Fatalf("a launch without PD gained one: %+v", params.PD)
	}
}
