package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestTargetAdmissionAllowsExplicitModelIndependentDraft(t *testing.T) {
	node := &store.Node{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100"}}}
	dep := &store.Deployment{Engine: "vllm", ServedName: "custom", Image: "example/custom:v1"}
	admission := TargetRefusalFor(dep, nil, func() (*store.Model, error) { return nil, nil }, nil)
	if refusal := admission.Refusal(node); refusal != nil || admission.Err() != nil {
		t.Fatalf("model-independent draft refusal=%+v err=%v", refusal, admission.Err())
	}
	dep.ModelID = 999
	admission = TargetRefusalFor(dep, nil, func() (*store.Model, error) { return nil, nil }, nil)
	admission.Refusal(node)
	if admission.Err() == nil {
		t.Fatal("missing referenced global model was accepted")
	}
}
