package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEvaluateGPUCandidatesWireContract(t *testing.T) {
	for _, tc := range []struct {
		items []dto.GPUCandidate
		want  string
	}{
		{[]dto.GPUCandidate{{Index: 0, Available: false, Reason: &dto.ScheduleReason{Code: "no_free_gpu"}}, {Index: 3, Available: true}}, `{"node_uuid":"node","items":[{"index":0,"available":false,"reason":{"code":"no_free_gpu"}},{"index":3,"available":true}]}`},
		{[]dto.GPUCandidate{}, `{"node_uuid":"node","items":[]}`},
	} {
		raw, err := json.Marshal(dto.GPUCandidates{NodeUUID: "node", Items: tc.items})
		if err != nil || string(raw) != tc.want {
			t.Fatalf("got %s want %s: %v", raw, tc.want, err)
		}
		var decoded dto.GPUCandidates
		if err := json.Unmarshal([]byte(tc.want), &decoded); err != nil {
			t.Fatal(err)
		}
		again, err := json.Marshal(decoded)
		if err != nil || string(again) != tc.want {
			t.Fatalf("round trip %s: %v", again, err)
		}
	}
	raw, _ := json.Marshal(dto.EvaluateResponse{})
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, present := wire["gpu_candidates"]; present {
		t.Fatal("unevaluated candidates must be absent")
	}
}

func TestEvaluateGPUCandidatesKeepThePoolSeparateFromThePlan(t *testing.T) {
	r := newAutoRig(t, 1)
	r.occupy(t, "au-1", 0)
	for _, tc := range []struct {
		name, placement string
		ok              bool
		chosen          []int
	}{
		{"occupied only", `{"gpus":[0],"tp":1}`, false, nil},
		{"free only", `{"gpus":[1],"tp":1}`, true, []int{1}},
		{"both with TP1", `{"gpus":[0,1],"tp":1}`, true, []int{1}},
		{"both with TP2", `{"gpus":[0,1],"tp":2}`, false, nil},
		{"automatic card count", `{"gpus":[0,1]}`, true, []int{1}},
		{"no card constraint", `{}`, true, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"au-1","target_vendor":"nvidia","placement_mode":"manual","placement":%s}`, r.modelID, tc.placement)
			status, wire, resp := autoEvaluate(t, r, body)
			var result struct {
				Plan *struct {
					GPUs []int `json:"gpus"`
				} `json:"plan"`
			}
			if err := json.Unmarshal(wire["plan"], &result); err != nil {
				t.Fatal(err)
			}
			var chosen []int
			if result.Plan != nil {
				chosen = result.Plan.GPUs
			}
			if status != http.StatusOK || resp.OK != tc.ok || !slices.Equal(chosen, tc.chosen) {
				t.Fatalf("response=%s", jsonOf(t, wire))
			}
			if string(wire["gpu_candidates"]) != `{"node_uuid":"au-1","items":[{"index":0,"available":false,"reason":{"code":"gpu_whole_allocated"}},{"index":1,"available":true}]}` {
				t.Fatalf("candidates=%s", wire["gpu_candidates"])
			}
		})
	}
	deps, err := r.st.ListDeployments()
	if err != nil || len(deps) != 1 {
		t.Fatalf("holders=%+v: %v", deps, err)
	}
	// Editing excludes the replaced instance; both cards remain candidates,
	// even though a TP=1 plan only uses one.
	body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"au-1","target_vendor":"nvidia","placement_mode":"manual","placement":{"gpus":[0],"tp":1},"deployment_id":%d}`, r.modelID, deps[0].ID)
	status, wire, resp := autoEvaluate(t, r, body)
	if status != http.StatusOK || !resp.OK || string(wire["gpu_candidates"]) != `{"node_uuid":"au-1","items":[{"index":0,"available":true},{"index":1,"available":true}]}` {
		t.Fatalf("edit=%s", jsonOf(t, wire))
	}
	if err := r.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "hold-au-1", State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	body = fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"au-1","target_vendor":"nvidia","placement_mode":"manual","placement":{"gpus":[0],"tp":1}}`, r.modelID)
	status, wire, resp = autoEvaluate(t, r, body)
	if status != http.StatusOK || !resp.OK || string(wire["gpu_candidates"]) != `{"node_uuid":"au-1","items":[{"index":0,"available":true},{"index":1,"available":true}]}` {
		t.Fatalf("released=%s", jsonOf(t, wire))
	}
}

func TestEvaluateGPUCandidatesFollowWholeAndSharedAllocations(t *testing.T) {
	for _, allocated := range []int{32, 48} {
		t.Run(fmt.Sprint(allocated), func(t *testing.T) {
			h, modelID := partitionPoolFixture(t, map[string]string{"choices-node": accelerator.BackendNVIDIALogical})
			node, err := h.d.Store.NodeByUUID("choices-node")
			if err != nil {
				t.Fatal(err)
			}
			dep, err := h.d.Store.CreateDeployment(&store.Deployment{Name: "holder", ServedName: "holder", Engine: "vllm", NodeID: node.ID, ModelID: modelID, State: "active"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.d.Store.CreateInstance(&store.Instance{UUID: "holder", NodeID: node.ID, DeploymentID: dep.ID, State: proto.InstRunning, GPUIndexes: "[0]", DesiredStateJSON: fmt.Sprintf(`{"engine":"vllm","memory_claim_bytes":%d}`, int64(allocated)<<30)})
			if err != nil {
				t.Fatal(err)
			}
			for _, quota := range []int{0, 16} {
				body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"choices-node","target_vendor":"nvidia","placement_mode":"manual","vram_limit_gib":%d}`, modelID, quota)
				wire := evaluateWire(t, h, body)
				var choices dto.GPUCandidates
				if err := json.Unmarshal(wire["gpu_candidates"], &choices); err != nil {
					t.Fatalf("choices=%s errors=%s: %v", wire["gpu_candidates"], wire["errors"], err)
				}
				want := quota > 0 && allocated == 32
				if choices.NodeUUID != "choices-node" || len(choices.Items) != 1 || choices.Items[0].Available != want || (choices.Items[0].Reason == nil) != want {
					t.Fatalf("quota=%d allocated=%d choices=%+v", quota, allocated, choices)
				}
			}
		})
	}
}

func TestEvaluateGPUCandidatesAreOptionalFeedback(t *testing.T) {
	r := newAutoRig(t, 1)
	for _, tc := range []struct {
		name, extra string
		wantOK      bool
	}{
		{"automatic placement", `,"placement_mode":"auto"`, true},
		{"unrelated parameter error", `,"placement_mode":"manual","start_timeout_sec":-1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"au-1","target_vendor":"nvidia"%s}`, r.modelID, tc.extra)
			status, wire, response := autoEvaluate(t, r, body)
			if status != http.StatusOK || response.OK != tc.wantOK {
				t.Fatalf("status=%d body=%s", status, jsonOf(t, wire))
			}
			if _, present := wire["gpu_candidates"]; present {
				t.Fatal("automatic placement or early error should not require resource feedback")
			}
		})
	}
}
