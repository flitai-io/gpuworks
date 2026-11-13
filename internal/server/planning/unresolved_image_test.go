package planning

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestUnresolvedImageIsOnlyAllowedForResourcePreview(t *testing.T) {
	st, err := store.Open("file:unresolved_image_preview?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, err := st.RegisterNode("preview", store.HashToken("token"), "preview")
	if err != nil {
		t.Fatal(err)
	}
	node.OS = "linux"
	node.Arch = "amd64"
	node.RuntimeDriver = "docker"
	node.GPUs = []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100", MemoryTotal: 80 << 30}}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/preview", Name: "preview", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	req := LaunchRequest{Preview: true, AllowUnresolvedImage: true, Engine: "vllm", Model: model, Node: node, Placement: Placement{GPUIndexes: []int{0}, TP: 1}, Topology: DistTopology{NNodes: 1}, ModelMount: deploy.DefaultModelMount, ServedName: "preview"}
	plan, err := PlanLaunch(context.Background(), st, storeNetworks(st), req)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Output.Fits() {
		t.Fatalf("resource preview did not fit: %+v", plan.Output.Result)
	}
	if len(plan.ImagePull.Candidates) != 0 || plan.Source.Image != "" {
		t.Fatal("unresolved image preview invented image facts")
	}
	for _, preview := range []bool{true, false} {
		req.Preview = preview
		req.AllowUnresolvedImage = false
		if !preview {
			req.AllowUnresolvedImage = true
		}
		_, err := PlanLaunch(context.Background(), st, storeNetworks(st), req)
		if err == nil || !strings.Contains(err.Error(), "no runtime image") {
			t.Fatalf("preview=%v allowed missing image: %v", preview, err)
		}
	}
}
