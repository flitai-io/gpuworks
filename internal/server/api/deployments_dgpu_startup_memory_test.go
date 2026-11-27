package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAscendDGPUStartupMemoryWarning(t *testing.T) {
	const repo = "Qwen/Qwen3-0.6B"
	builtin, spec := builtinDGPUStartupSpec(t, strings.ToLower(repo))
	for _, tc := range []struct {
		name, placement, quick, extra, command string
		quota                                  int
		busy, warn, direct                     bool
	}{
		{name: "direct_create", placement: "manual", quick: `{"gpu_memory_utilization":0.90}`, quota: 8, warn: true, direct: true},
		{name: "manual_quick", placement: "manual", quick: `{"gpu_memory_utilization":0.90}`, quota: 8, warn: true},
		{name: "auto_extra", placement: "auto", quick: "{}", extra: "--mem-fraction-static=0.90", quota: 8, warn: true},
		{name: "manual_safe", placement: "manual", quick: `{"gpu_memory_utilization":0.88}`, quota: 8},
		{name: "auto_default", placement: "auto", quick: "{}", quota: 8},
		{name: "whole_card", placement: "manual", quick: `{"gpu_memory_utilization":0.90}`},
		{name: "custom_graph", placement: "manual", quick: `{"gpu_memory_utilization":0.90}`, extra: "--cuda-graph-max-bs=256", quota: 8, warn: true},
		{name: "no_graph", placement: "manual", quick: `{"gpu_memory_utilization":0.90}`, extra: "--disable-cuda-graph", quota: 8},
		{name: "manual_script", placement: "manual", quick: "{}", command: sglangOneCardScript + " --dtype bfloat16 --mem-fraction-static=0.90", quota: 8, warn: true},
		{name: "busy_node", placement: "manual", quick: `{"gpu_memory_utilization":0.90}`, quota: 8, busy: true, warn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, holderModel := partitionPoolFixture(t, map[string]string{"pool-ascend": accelerator.BackendAscendLogical})
			st := h.d.Store
			cardNode, err := st.NodeWithGPUs("pool-ascend")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.DB().Model(&store.GPU{}).Where("node_id = ?", cardNode.ID).Update("name", "Ascend 910B4").Error; err != nil {
				t.Fatal(err)
			}
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: repo, Name: repo, Arch: "qwen3", WeightBytes: spec.WeightBytes})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.ReconcileInferenceCatalog([]store.InferenceModel{builtin}); err != nil {
				t.Fatal(err)
			}
			template, err := st.InferenceModelByModelID(builtin.ModelID)
			if err != nil {
				t.Fatal(err)
			}
			wantDeployments := 0
			if tc.busy {
				holder, err := st.CreateDeployment(&store.Deployment{Name: "whole-card", ModelID: holderModel, NodeID: cardNode.ID, Engine: "sglang", Replicas: 1})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.CreateInstance(&store.Instance{UUID: "whole-card-0", DeploymentID: holder.ID, NodeID: cardNode.ID, State: proto.InstRunning, GPUIndexes: `[0]`}); err != nil {
					t.Fatal(err)
				}
				wantDeployments++
			}
			node := ""
			if tc.placement == "manual" {
				node = "pool-ascend"
			}
			command := "{}"
			if tc.command != "" {
				command = fmt.Sprintf(`{"command_mode":"manual","cmd_display":%q}`, tc.command)
			}
			body := fmt.Sprintf(`{"name":%q,"model_id":%d,"template_id":%d,"template_spec_source":%q,"template_spec_name":%q,"engine":"sglang","image":%q,"node_uuid":%q,"placement_mode":%q,"target_vendor":"ascend","placement":{"tp":1},"vram_limit_gib":%d,"quick_params":%s,"extra_args":%q,"container_overrides":%s}`,
				tc.name, model.ID, template.ID, spec.Source, spec.Name, spec.Image, node, tc.placement, tc.quota, tc.quick, tc.extra, command)

			if !tc.direct {
				wire := evaluateWire(t, h, body)
				assertDGPUStartupWarning(t, wire["warnings"], tc.warn)
				if string(wire["ok"]) != fmt.Sprint(!tc.busy) || len(wire["errors"]) != 0 {
					t.Fatalf("evaluate = %s, errors = %s, plan = %s; busy = %v", wire["ok"], wire["errors"], wire["plan"], tc.busy)
				}
			}
			created := httptest.NewRecorder()
			h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if created.Code != http.StatusCreated {
				t.Fatalf("create = %d %s, want accepted", created.Code, created.Body)
			}
			var response map[string]json.RawMessage
			if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			assertDGPUStartupWarning(t, response["warnings"], tc.warn)
			if rows, err := st.ListDeployments(); err != nil || len(rows) != wantDeployments+1 {
				t.Fatalf("created request did not persist a deployment: %v %v", rows, err)
			}
		})
	}
}

func assertDGPUStartupWarning(t *testing.T, body json.RawMessage, want bool) {
	t.Helper()
	var warnings []messages.Warning
	if len(body) > 0 {
		if err := json.Unmarshal(body, &warnings); err != nil {
			t.Fatal(err)
		}
	}
	warnings = slices.DeleteFunc(warnings, func(w messages.Warning) bool {
		return w.Code != messages.WarningDGPUStartupMemory
	})
	if !want {
		if len(warnings) != 0 {
			t.Fatalf("unexpected dGPU warnings: %v", warnings)
		}
		return
	}
	got, err := json.Marshal(warnings)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON(t, string(got), `[{"code":"DGPU_STARTUP_MEMORY","params":{"quota_gib":8,"utilization":0.9,"max_utilization":0.88}}]`)
}

func builtinDGPUStartupSpec(t *testing.T, modelID string) (store.InferenceModel, store.InferenceSpecRow) {
	t.Helper()
	catalog, err := seed.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(catalog.Models, func(model inferencecatalog.InferenceModel) bool { return model.ModelID == modelID })
	if index < 0 {
		t.Fatalf("built-in catalog has no model %s", modelID)
	}
	rows, err := inferencecatalog.ToFlatStoreModels(inferencecatalog.FlatCatalog{Models: catalog.Models[index : index+1]})
	if err != nil {
		t.Fatal(err)
	}
	model := rows[0]
	gpu := chipmatch.ReportedGPU("Ascend 910B4", "ascend", "910B4", "")
	var matching []store.InferenceSpecRow
	for _, spec := range model.Specs {
		if spec.Engine != "sglang" || spec.Quantization != "bf16" || spec.Scenario != "balanced" {
			continue
		}
		predicate, err := chipmatch.DecodePredicate(spec.GPUs)
		if err != nil {
			t.Fatal(err)
		}
		if chipmatch.ValidateSpec(gpu, chipmatch.Spec{Name: spec.Name, GPUs: predicate}, nil) == nil {
			matching = append(matching, spec)
		}
	}
	if len(matching) != 1 {
		t.Fatalf("found %d built-in BF16 SGLang balanced specs for %s on 910B4", len(matching), modelID)
	}
	return model, matching[0]
}
