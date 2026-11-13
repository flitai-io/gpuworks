package planning

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func launchKVFixture(t *testing.T) (*store.Store, *store.Model, *store.InferenceModel, *store.InferenceModel) {
	t.Helper()
	st, err := store.Open("file:planning_launch_kv_selection?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	spec := func(name, engine string) store.InferenceSpecRow {
		return store.InferenceSpecRow{
			Name: name, Engine: engine, Image: "registry.example/app:v1", Quantization: "bf16",
			WeightBytes: 1 << 30, RuntimeWeightBytes: 1 << 30,
			WeightSources: "[{\"provider\":\"huggingface\",\"repo_id\":\"org/base\"}]",
			GPUs:          "{\"vendor\":\"nvidia\",\"min_count\":1}", Args: "[]", Env: "{}",
		}
	}
	models := []store.InferenceModel{
		{
			ModelID: "org/base", Source: "upstream",
			KVBytesPerToken: "{\"vllm\":12288,\"sglang\":65536,\"unused_removed_marker\":777}",
			Specs:           []store.InferenceSpecRow{spec("base-vllm", "vllm")},
		},
		{
			ModelID: "org/selected", Source: "upstream",
			KVBytesPerToken: "{\"vllm\":49152,\"sglang\":12288}",
			Specs:           []store.InferenceSpecRow{spec("selected-vllm", "vllm"), spec("selected-sglang", "sglang")},
		},
		{
			ModelID: "org/no-vllm-kv", Source: "upstream",
			KVBytesPerToken: "{\"sglang\":2048}",
			Specs:           []store.InferenceSpecRow{spec("selected-no-vllm-kv", "vllm")},
		},
	}
	if err := st.ReconcileInferenceCatalog(models); err != nil {
		t.Fatal(err)
	}
	selected, err := st.InferenceModelByModelID("org/selected")
	if err != nil {
		t.Fatal(err)
	}
	noVLLMKV, err := st.InferenceModelByModelID("org/no-vllm-kv")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "org/base", Name: "fixture", WeightBytes: 1 << 30, Arch: "qwen3",
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, model, selected, noVLLMKV
}

func selectionForKV(model *store.InferenceModel, engine string) deploy.CatalogSelection {
	return deploy.CatalogSelection{
		TemplateID: model.ID, ModelID: model.ModelID, Engine: engine, SpecName: "selected-" + engine,
		WeightBytes: 1 << 30, RuntimeWeightBytes: 1 << 30,
	}
}

func TestLaunchInputUsesSelectedCatalogKVByIDAndKeepsFallback(t *testing.T) {
	st, model, selected, noVLLMKV := launchKVFixture(t)
	node := &store.Node{GPUVendor: "nvidia"}

	for _, tc := range []struct {
		engine string
		want   int64
	}{
		{engine: "vllm", want: 49152},
		{engine: "sglang", want: 12288},
	} {
		input, err := launchInput(st, LaunchRequest{
			Engine: tc.engine, Model: model, Node: node, Selection: selectionForKV(selected, tc.engine),
			Topology: DistTopology{NNodes: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		if input.KVBytesPerToken != tc.want {
			t.Fatalf("selected catalog %s KV = %d, want %d", tc.engine, input.KVBytesPerToken, tc.want)
		}
	}

	for _, tc := range []struct {
		name      string
		selection deploy.CatalogSelection
		want      int64
	}{
		{name: "no snapshot selection falls back to source id", want: 12288},
		{name: "zero template id falls back to source id", selection: deploy.CatalogSelection{ModelID: selected.ModelID}, want: 12288},
		{name: "unknown template id falls back to source id", selection: deploy.CatalogSelection{TemplateID: selected.ID + 100000, ModelID: selected.ModelID}, want: 12288},
		{name: "mismatched model identity falls back to source id", selection: deploy.CatalogSelection{TemplateID: selected.ID, ModelID: "org/wrong"}, want: 12288},
		{name: "selected row missing engine key stays unknown", selection: selectionForKV(noVLLMKV, "vllm"), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := launchInput(st, LaunchRequest{
				Engine: "vllm", Model: model, Node: node, Selection: tc.selection,
				Topology: DistTopology{NNodes: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			if input.KVBytesPerToken != tc.want {
				t.Fatalf("KVBytesPerToken = %d, want %d", input.KVBytesPerToken, tc.want)
			}
		})
	}
}

func TestLaunchInputSelectedCatalogKVLookupIsSingleIDQuery(t *testing.T) {
	st, model, selected, _ := launchKVFixture(t)
	var modelQueries, specQueries []string
	err := st.DB().Callback().Query().After("gorm:query").Register("planning:selected_catalog_kv_by_id", func(tx *gorm.DB) {
		switch tx.Statement.Table {
		case "inference_models":
			modelQueries = append(modelQueries, strings.ToLower(tx.Statement.SQL.String()))
		case "inference_specs":
			specQueries = append(specQueries, strings.ToLower(tx.Statement.SQL.String()))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = launchInput(st, LaunchRequest{
		Engine: "vllm", Model: model, Node: &store.Node{GPUVendor: "nvidia"},
		Selection: selectionForKV(selected, "vllm"), Topology: DistTopology{NNodes: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(modelQueries) != 1 || len(specQueries) != 0 {
		t.Fatalf("selected catalog lookups = model rows %v, spec rows %v; want one model ID query and no spec query", modelQueries, specQueries)
	}
	query := strings.ToLower(modelQueries[0])
	normalized := strings.NewReplacer(" ", "", "\t", "", "\"", "", string(rune(96)), "").Replace(query)
	if !strings.Contains(normalized, "whereinference_models.id=") || strings.Contains(query, "inference_specs") {
		t.Fatalf("selected catalog query must fetch one row by ID, got %q", modelQueries[0])
	}
}

func TestLaunchInputWithNilStoreDoesNotResolveSelectedCatalog(t *testing.T) {
	_, model, selected, _ := launchKVFixture(t)
	input, err := launchInput(nil, LaunchRequest{
		Engine: "vllm", Model: model, Node: &store.Node{GPUVendor: "nvidia"},
		Selection: selectionForKV(selected, "vllm"), Topology: DistTopology{NNodes: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if input.KVBytesPerToken != 0 {
		t.Fatalf("nil-store KVBytesPerToken = %d, want unknown 0", input.KVBytesPerToken)
	}
}

func TestPlanLaunchUsesSelectedCatalogKVForPreviewAndController(t *testing.T) {
	st, model, selected, _ := launchKVFixture(t)
	node, err := st.RegisterNode("selected-kv-plan", store.HashToken("token"), "selected-kv-plan")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUVendor = "nvidia"
	node.GPUs = []store.GPU{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 80 << 30}}

	dep := &store.Deployment{ID: 5, Image: "registry.example/app:v1"}
	source := LaunchSourceFromDeployment(dep)
	request := LaunchRequest{
		Engine: "vllm", Model: model, Node: node, Selection: selectionForKV(selected, "vllm"),
		Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1, Role: "leader"},
		ModelMount: deploy.DefaultModelMount, ServedName: "selected-kv", Source: source,
	}
	controller := request
	controller.APIPort = 31000
	preview := request
	preview.Preview = true
	preview.Source = LaunchSourceFromRequest(dep.Image, source.Overrides)

	controllerPlan, err := PlanLaunch(context.Background(), st, storeNetworks(st), controller)
	if err != nil {
		t.Fatal(err)
	}
	previewPlan, err := PlanLaunch(context.Background(), st, storeNetworks(st), preview)
	if err != nil {
		t.Fatal(err)
	}
	if !controllerPlan.Output.Result.OK || !previewPlan.Output.Result.OK {
		t.Fatalf("controller=%+v preview=%+v", controllerPlan.Output.Result, previewPlan.Output.Result)
	}
	if controllerPlan.Input.KVBytesPerToken != 49152 || previewPlan.Input.KVBytesPerToken != 49152 {
		t.Fatalf("controller KV=%d preview KV=%d, want selected catalog 49152", controllerPlan.Input.KVBytesPerToken, previewPlan.Input.KVBytesPerToken)
	}
	controllerInput := controllerPlan.Input
	// A stored controller launch identifies the deployment for ledger exclusion;
	// a new draft has no persisted identity. Compare the shared planning facts.
	controllerInput.DeploymentID = previewPlan.Input.DeploymentID
	if !reflect.DeepEqual(controllerInput, previewPlan.Input) {
		t.Fatalf("controller and preview inputs differ: controller=%+v preview=%+v", controllerPlan.Input, previewPlan.Input)
	}
}
