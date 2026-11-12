package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// pdTierBody is a PD draft that enables a 19 GiB CPU KV tier. KV offload
// belongs to the prefill role only, so only its instances keep the tier in
// /dev/shm.
func pdTierBody(modelID uint, overrides string) string {
	return fmt.Sprintf(`{"name":"pd","model_id":%d,"engine":"vllm","image":"vllm/vllm-openai:v0.25.1","quick_params":{"max_model_len":4096,"kv_offloading_size":19}%s,"placement":{"pd":%s}}`, modelID, overrides, pdOneEach)
}

// The preview states each instance's own /dev/shm: the prefill instances carry
// the engine default plus the tier, the decode instance the plain default.
func TestEvaluatePDShmSizeFollowsEachRolesTier(t *testing.T) {
	h, modelID, _ := pdAPIFixture(t)
	status, raw := evaluateForFieldErrors(t, h, pdTierBody(modelID, ""))
	if status != http.StatusOK || string(raw["ok"]) != "true" {
		t.Fatalf("evaluate = %d %s", status, raw)
	}
	var resp dto.EvaluateResponse
	encoded, _ := json.Marshal(raw)
	if err := json.Unmarshal(encoded, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.PD == nil || len(resp.PD.Roles) != 2 || resp.PD.Roles[0].Role != "decode" || resp.PD.Roles[1].Role != "prefill" {
		t.Fatalf("pd = %+v", resp.PD)
	}
	for _, unit := range resp.PD.Roles[0].Units {
		if unit.ShmBytes != 16<<30 {
			t.Fatalf("decode shm = %d, want the plain 16 GiB default", unit.ShmBytes)
		}
	}
	if len(resp.PD.Roles[1].Units) != 2 {
		t.Fatalf("prefill units = %+v", resp.PD.Roles[1].Units)
	}
	for _, unit := range resp.PD.Roles[1].Units {
		if unit.ShmBytes != 35<<30 {
			t.Fatalf("prefill shm = %d, want 16 GiB + 19 GiB", unit.ShmBytes)
		}
	}
	// Literal wire values: byte counts under the shm_bytes key.
	var wire struct {
		Roles []struct {
			Units []map[string]json.RawMessage `json:"units"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(raw["pd"], &wire); err != nil {
		t.Fatal(err)
	}
	if got := string(wire.Roles[0].Units[0]["shm_bytes"]); got != "17179869184" {
		t.Fatalf("decode wire shm_bytes = %s", got)
	}
	if got := string(wire.Roles[1].Units[0]["shm_bytes"]); got != "37580963840" {
		t.Fatalf("prefill wire shm_bytes = %s", got)
	}
}

// A size that fits the decode role but not the prefill role's tier is refused
// before anything runs, attributed to the shared memory field.
func TestEvaluatePDRefusesShmBelowThePrefillTier(t *testing.T) {
	h, modelID, _ := pdAPIFixture(t)
	status, raw := evaluateForFieldErrors(t, h, pdTierBody(modelID, `,"container_overrides":{"shm_bytes":8589934592}`))
	if status != http.StatusOK || string(raw["ok"]) != "false" {
		t.Fatalf("evaluate = %d %s", status, raw)
	}
	errs := fieldErrorsOf(t, raw)
	if len(errs) != 1 || errs[0].Field != deployment.FieldContainerShm || errs[0].Code != messages.ErrorShmBelowKVCPUTier {
		t.Fatalf("errors = %+v", errs)
	}
	if fmt.Sprint(errs[0].Params["shm_gib"]) != "8" || fmt.Sprint(errs[0].Params["tier_gib"]) != "19" {
		t.Fatalf("params = %+v", errs[0].Params)
	}
}
