package deploy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestManualModelDeploymentImageResolutionIsVendorGeneric(t *testing.T) {
	tests := []struct {
		name         string
		vendor       string
		gpu          string
		framework    string
		catalogImage string
		override     string
		wantImage    string
		wantBlocked  bool
	}{
		{name: "hygon-k100-catalog", vendor: "hygon", gpu: "Hygon K100_AI", framework: "dtk", catalogImage: "example/hygon-vllm:stable", wantImage: "example/hygon-vllm:stable"},
		{name: "hygon-bw1100-same-vendor-catalog", vendor: "hygon", gpu: "Hygon BW1100", framework: "dtk", catalogImage: "example/hygon-vllm:stable", wantImage: "example/hygon-vllm:stable"},
		{name: "ascend-910c-explicit-private-image", vendor: "ascend", gpu: "Ascend 910C", framework: "cann", override: "private.example/ascend-vllm:custom", wantImage: "private.example/ascend-vllm:custom"},
		// A domestic accelerator never falls back to an engine default image.
		{name: "hygon-without-catalog-or-override", vendor: "hygon", gpu: "Hygon BW1000", framework: "dtk", wantBlocked: true},
	}
	for _, fixture := range tests {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			env := testenv.New(t)
			node := env.AddNode(fixture.name,
				testenv.WithGPUList(testenv.GPUSpec{Model: fixture.gpu, Vendor: fixture.vendor, VRAMMB: 65536}),
				testenv.WithAccelerator(fixture.vendor, fixture.framework, "1.0"),
				testenv.WithDriverName("docker"), testenv.WithoutAcceleratorRuntimeInjection(),
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
			// Each case states the whole catalog it resolves against; the
			// embedded system images are arch-specific and would make the
			// outcome depend on the host running the test.
			for _, statement := range []string{
				"DELETE FROM image_versions WHERE source = 'official'",
				"DELETE FROM image_series WHERE source = 'official'",
			} {
				if err := env.Store().DB().Exec(statement).Error; err != nil {
					t.Fatal(err)
				}
			}
			nodes, err := env.Store().ListNodes()
			if err != nil || len(nodes) != 1 {
				t.Fatalf("nodes=%+v err=%v", nodes, err)
			}
			model, err := env.Store().UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "gpuworks/manual-" + fixture.name, Revision: "main", Name: fixture.name,
				Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 20,
				Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":4096}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			manualDir := testenv.ModelDir(t, "manual")
			if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: manualDir}); err != nil {
				t.Fatal(err)
			}
			if fixture.catalogImage != "" {
				if _, err := env.Store().UpsertImage(&store.Image{
					Name: "vendor runtime", Engine: "vllm", Framework: fixture.framework, FrameworkVersion: "1.0",
					Arch: "multi", ImageRef: fixture.catalogImage, Source: "official", Enabled: true,
				}); err != nil {
					t.Fatal(err)
				}
			}

			request := map[string]any{
				"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": fixture.name,
				"engine": "vllm", "quick_params": map[string]any{}, "placement": map[string]any{},
			}
			if fixture.override != "" {
				request["image"] = fixture.override
			}
			body, _ := json.Marshal(request)
			response, err := env.API(t).PostJSON("/api/v1/deployments", body)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if fixture.wantBlocked {
				raw, _ := io.ReadAll(response.Body)
				if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "IMAGE_CATALOG_NO_MATCH_DOMESTIC") {
					t.Fatalf("status=%d body=%s, want the domestic no-match refusal", response.StatusCode, raw)
				}
				return
			}
			if response.StatusCode != http.StatusCreated {
				raw, _ := io.ReadAll(response.Body)
				t.Fatalf("status=%d body=%s", response.StatusCode, raw)
			}
			wantImage := fixture.wantImage
			select {
			case workload := <-runs:
				if workload.Image != wantImage || !containsRuntimeMount(workload.Mounts, manualDir, "/model", true) {
					t.Fatalf("workload=%+v want image=%q", workload, wantImage)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("manual deployment did not reach agent")
			}
		})
	}
}
