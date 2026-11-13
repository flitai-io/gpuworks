package deployment

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An unpinned create has no runtime to judge: the runtime-dependent rules wait
// for the controller, which checks the node it picks.
func TestCreatePreflightWithoutANodeSkipsRuntimeRules(t *testing.T) {
	engine, err := deploy.SelectEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	base := deploy.ResolveLaunchInput{
		Engine: engine, Image: "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3", PreStartArgv: []string{"prepare-tool"},
		Params:          deploy.Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1},
		AllowUnresolved: true,
	}
	imageFor := func(*store.Node) string { return base.Image }
	if err := preflightCreateRuntimes(base, nil, nil, "/model", nil, nil, nil, imageFor); err != nil {
		t.Fatalf("unpinned create was refused: %v", err)
	}
	pinned := &store.Node{UUID: "tpl-node", RuntimeDriver: "udocker"}
	err = preflightCreateRuntimes(base, nil, nil, "/model", pinned, nil, nil, imageFor)
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorAscendV1CompatPreStart {
		t.Fatalf("pinned udocker create = %v, want %s", err, messages.ErrorAscendV1CompatPreStart)
	}
	// The request's own image field is empty: the node's resolved image is what
	// decides.
	base.Image = ""
	err = preflightCreateRuntimes(base, nil, nil, "/model", pinned, nil, nil, func(*store.Node) string {
		return "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3"
	})
	if !errors.As(err, &product) || product.Code() != messages.ErrorAscendV1CompatPreStart {
		t.Fatalf("create with a resolved compat image = %v, want %s", err, messages.ErrorAscendV1CompatPreStart)
	}
}
