package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestMaterializeDeployRequestUsesUnsetForPreviewAndPersistenceProjection(t *testing.T) {
	request := &dto.DeployRequest{
		QuickParams:      map[string]any{"max_model_len": 8192},
		QuickParamsUnset: []string{"kv_offloading_size"},
	}
	selection := templateSelection{ExtraArgs: []string{"--kv-offloading-size", "32", "--max-num-seqs", "16"}}
	materialized, err := materializeDeployRequest(deploy.EngineVLLM, request, selection, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if materialized.Quick.KVOffloadingSize != 0 || materialized.Quick.MaxModelLen != 8192 || materialized.Quick.MaxNumSeqs != 16 {
		t.Fatalf("effective preview config = %+v", materialized.Quick)
	}
	persisted := LaunchConfigJSON(materialized.Quick)
	// Unset removed the tier size; what remains is the stated pair plus the
	// dictionary defaults an untouched form shows, and no tombstone data.
	if strings.Contains(persisted, "unset") {
		t.Fatalf("persisted launch config carries tombstone data: %s", persisted)
	}
	var persistedValues map[string]any
	if err := json.Unmarshal([]byte(persisted), &persistedValues); err != nil {
		t.Fatal(err)
	}
	if _, leaked := persistedValues["kv_offloading_size"]; leaked {
		t.Fatalf("unset key persisted: %s", persisted)
	}
	if fmt.Sprint(persistedValues["max_model_len"]) != "8192" || fmt.Sprint(persistedValues["max_num_seqs"]) != "16" {
		t.Fatalf("stated values must persist verbatim: %s", persisted)
	}
	if fmt.Sprint(persistedValues["gpu_memory_utilization"]) != "0.9" {
		t.Fatalf("dictionary defaults must persist for a request: %s", persisted)
	}

	// Both endpoint request DTOs carry the same embedded create body, so the
	// exact tombstone projection survives the evaluate round trip unchanged.
	evaluate := dto.EvaluateRequest{DeployRequest: *request}
	raw, err := json.Marshal(evaluate)
	if err != nil {
		t.Fatal(err)
	}
	var create dto.DeployRequest
	if err := json.Unmarshal(raw, &create); err != nil {
		t.Fatal(err)
	}
	if len(create.QuickParamsUnset) != 1 || create.QuickParamsUnset[0] != "kv_offloading_size" {
		t.Fatalf("evaluate/create request projection = %#v", create.QuickParamsUnset)
	}
}

func TestMaterializeDeployRequestReportsUnsetConflictsAsStableProductErrors(t *testing.T) {
	for name, request := range map[string]*dto.DeployRequest{
		"form": {
			QuickParams:      map[string]any{"kv_offloading_size": 8},
			QuickParamsUnset: []string{"kv_offloading_size"},
		},
		"extra args": {
			QuickParamsUnset: []string{"kv_offloading_size"},
			ExtraArgs:        "--kv-offloading-size 8",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := materializeDeployRequest(deploy.EngineVLLM, request, templateSelection{}, nil, "")
			var product *messages.ProductError
			if !errors.As(err, &product) || product.Code() != deploy.ErrorLaunchConfigUnsetConflict {
				t.Fatalf("error = %v, want %s", err, deploy.ErrorLaunchConfigUnsetConflict)
			}
		})
	}
}
