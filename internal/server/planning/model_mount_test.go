package planning

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A model an extra mount row names is a dependency of the deployment like a
// template component: the controller makes it readable on the node, and the
// launch carries the directory it found there.
func TestDeploymentModelMountIsADependencyAndLaunchesFromItsNodePath(t *testing.T) {
	dep := &store.Deployment{ID: 3, Engine: "vllm", ModelID: 1,
		ContainerOverrides: `{"extra_mounts":[{"model_id":7,"container_path":"/draft"},{"model_id":1,"container_path":"/again"},{"host_path":"/srv","container_path":"/srv"}]}`}
	ids, err := DeploymentModelIDs(dep)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []uint{1, 7}) {
		t.Fatalf("DeploymentModelIDs = %v, want the deployment's model then the mounted one", ids)
	}

	model := &store.Model{Name: "model"}
	node := &store.Node{UUID: "node", GPUVendor: "nvidia", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100"}}}
	if _, _, _, err := DeploymentLaunch(dep, nil, node, model, &ModelPaths{Primary: "/primary", Components: map[uint]string{1: "/primary"}}); err == nil || !strings.Contains(err.Error(), "mounts model 7") {
		t.Fatalf("launch without the mounted model's path: err=%v", err)
	}
	req, _, _, err := DeploymentLaunch(dep, nil, node, model, &ModelPaths{Primary: "/primary", Components: map[uint]string{1: "/primary", 7: "/models/draft"}})
	if err != nil {
		t.Fatal(err)
	}
	if req.ModelPaths[7] != "/models/draft" || req.ModelPaths[1] != "/primary" {
		t.Fatalf("ModelPaths = %v", req.ModelPaths)
	}
	// A preview has no node paths: the request carries none and the rows
	// resolve to placeholders downstream.
	preview, _, _, err := DeploymentLaunch(dep, nil, node, model, nil)
	if err != nil || preview.ModelPaths != nil {
		t.Fatalf("preview ModelPaths=%v err=%v", preview.ModelPaths, err)
	}
}
