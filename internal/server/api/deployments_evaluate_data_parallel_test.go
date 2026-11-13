package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A pinned one-card preview rejects DP2. An unselected preview has no GPU
// allocation to validate; EverFits separately verifies pool admission.
func TestEvaluateRefusesQuickDataParallelBeyondTheAssignedGPUs(t *testing.T) {
	st, err := store.Open("file:evaluate_data_parallel?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/data-parallel", Revision: "main", Name: "data-parallel", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	for _, tc := range []struct {
		name, engine, node, flag string
	}{
		{name: "vLLM on a chosen node", engine: "vllm", node: `"node_uuid":"field-errors-node",`, flag: "--data-parallel-size"},
		{name: "SGLang on a chosen node", engine: "sglang", node: `"node_uuid":"field-errors-node",`, flag: "--dp-size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model_id":%d,%s"engine":%q,"quick_params":{"data_parallel_size":2}}`, model.ID, tc.node, tc.engine)
			status, raw := evaluateForFieldErrors(t, h, body)
			if status != http.StatusOK {
				t.Fatalf("status = %d body = %s", status, raw)
			}
			if string(raw["ok"]) != "false" {
				t.Fatalf("ok = %s, want false (body %s)", raw["ok"], raw)
			}
			errors := fieldErrorsOf(t, raw)
			if len(errors) != 1 {
				t.Fatalf("errors = %+v, want exactly the data-parallel rule", errors)
			}
			got := errors[0]
			if got.Field != "data_parallel_size" || got.Code != ErrorDataParallelGPUCount {
				t.Fatalf("field error = %+v, want data_parallel_size/%s", got, ErrorDataParallelGPUCount)
			}
			for key, want := range map[string]string{"key": "data_parallel_size", "flag": tc.flag, "dp": "2", "tp": "1", "pp": "1", "required": "2", "assigned": "1"} {
				if fmt.Sprint(got.Params[key]) != want {
					t.Fatalf("params[%q] = %v, want %s (all: %+v)", key, got.Params[key], want, got.Params)
				}
			}

			twin := fmt.Sprintf(`{"model_id":%d,%s"engine":%q,"quick_params":{"data_parallel_size":1}}`, model.ID, tc.node, tc.engine)
			status, raw = evaluateForFieldErrors(t, h, twin)
			if status != http.StatusOK || string(raw["ok"]) != "true" {
				t.Fatalf("degree one: status = %d body = %s", status, raw)
			}
			if _, present := raw["errors"]; present {
				t.Fatalf("degree one produced errors: %s", raw["errors"])
			}
		})
	}
}

// An unselected form preview can show a symbolic command, but it must not
// claim an actual plan on a one-card pool. A feasible preview supplies the
// selected node that the create API requires, just as the Web form does.
func TestAutomaticDataParallelPreviewAndCreateAdmission(t *testing.T) {
	for _, cards := range []int{1, 2} {
		t.Run(fmt.Sprintf("cards_%d", cards), func(t *testing.T) {
			st, err := store.Open(fmt.Sprintf("file:auto_dp_%d?mode=memory&cache=shared", cards), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/auto-dp", Revision: "main", Name: "auto-dp", WeightBytes: 1 << 30})
			if err != nil {
				t.Fatal(err)
			}
			h := evaluateFieldErrorFixture(t, st)
			if cards == 2 {
				status := proto.NodeStatus{Sys: proto.SysInfo{Hostname: "field-errors-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"}}
				for i := 0; i < 2; i++ {
					status.GPUs = append(status.GPUs, proto.GPUInfo{Index: i, Name: "NVIDIA GPU", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 16 << 30, Healthy: true})
				}
				if err := st.UpdateNodeStatus("field-errors-node", status); err != nil {
					t.Fatal(err)
				}
			}
			body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","quick_params":{"data_parallel_size":2}}`, model.ID)
			status, raw := evaluateForFieldErrors(t, h, body)
			if status != http.StatusOK {
				t.Fatalf("evaluate status %d: %v", status, raw)
			}
			if cards == 1 {
				if plan := raw["plan"]; len(plan) > 0 && string(plan) != "null" {
					t.Fatalf("preview claimed a plan: %s", plan)
				}
				if selected := raw["selected_node"]; len(selected) > 0 && string(selected) != "null" {
					t.Fatalf("one-card preview selected an infeasible node: %s", selected)
				}
				return
			} else {
				var plan struct {
					Plan *struct {
						Inner *struct {
							GPUIndexes []int `json:"gpus"`
						} `json:"plan"`
					} `json:"plan"`
				}
				packed, _ := json.Marshal(raw)
				if err := json.Unmarshal(packed, &plan); err != nil {
					t.Fatal(err)
				}
				if plan.Plan == nil || plan.Plan.Inner == nil || len(plan.Plan.Inner.GPUIndexes) != 2 {
					t.Fatalf("two-card preview has no two-card plan: %s", packed)
				}
			}
			var selected struct {
				UUID string `json:"uuid"`
			}
			if err := json.Unmarshal(raw["selected_node"], &selected); err != nil || selected.UUID == "" {
				t.Fatalf("two-card preview did not select a node: %s (%v)", raw["selected_node"], err)
			}
			create := httptest.NewRecorder()
			request := fmt.Sprintf(`{"name":"auto-dp-%d","model_id":%d,"engine":"vllm","image":"example/vllm:test","placement_mode":"auto","node_uuid":%q,"quick_params":{"data_parallel_size":2}}`, cards, model.ID, selected.UUID)
			h.createDeployment(create, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(request)))
			if create.Code != http.StatusCreated {
				t.Fatalf("two-card create = %d %s", create.Code, create.Body.String())
			}
		})
	}
}
