package planning

import (
	"encoding/json"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/placement"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestResolveAllocatedLaunchRejectsTemplateNodeLimitBeforeGPUResolution(t *testing.T) {
	req := LaunchRequest{
		Topology:  DistTopology{NNodes: 2},
		Selection: deploy.CatalogSelection{Placement: json.RawMessage(`{"max_nodes":1}`)},
		Node:      &store.Node{},
	}
	// The selected index is absent, so resolving the GPUs first would return
	// a GPU error instead of the catalog's node limit.
	_, err := ResolveAllocatedLaunch(req, []int{0}, 1, parallelism.Spec{}, 0, nil, 0, false)
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorTemplatePlacementNodeLimit {
		t.Fatalf("ResolveAllocatedLaunch = %v, want %s", err, messages.ErrorTemplatePlacementNodeLimit)
	}
}

func TestDeploymentTemplateReadsSnapshotWithoutMaxNodes(t *testing.T) {
	dep := &store.Deployment{
		CatalogSelectionSnapshot: `{"template_id":7,"model_id":"org/model","spec_name":"bf16-balanced","spec_source":"upstream","engine":"vllm","placement":{"language":"starlark","resolver":"def resolve(gpu):\n    return {}"}}`,
	}
	selection, err := DeploymentTemplate(dep)
	if err != nil {
		t.Fatal(err)
	}
	var contract placement.Contract
	if err := json.Unmarshal(selection.Placement, &contract); err != nil {
		t.Fatalf("decode stored placement: %v", err)
	}
	if contract.MaxNodes != 0 || selection.PlacementResolver() != "def resolve(gpu):\n    return {}" {
		t.Fatalf("unexpected placement from old snapshot: %+v", contract)
	}
	if err := selection.ValidatePlacementNodes(5); err != nil {
		t.Fatalf("old snapshot refused a multi-node launch: %v", err)
	}
}
