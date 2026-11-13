package planning

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestTargetPoolRefusalIsLocalAndIndependentOfNodeOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			nodes := []store.Node{
				{UUID: "broken", GPUVendor: "amd", OS: "linux", Arch: "amd64", RuntimeDriver: "host", GPUs: []store.GPU{{Index: 0, Vendor: "amd", Name: "MI300X"}}},
				{UUID: "healthy", GPUVendor: "amd", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{{Index: 0, Vendor: "amd", Name: "MI300X"}}},
			}
			if reverse {
				nodes[0], nodes[1] = nodes[1], nodes[0]
			}
			resolve := func(req LaunchRequest) (*proto.RequestedFeatures, error) {
				if req.Node.UUID == "broken" {
					return nil, errors.New("node-local image entrypoint unavailable")
				}
				return &proto.RequestedFeatures{CommandKnown: true}, nil
			}
			// A failure on one runtime shape cannot abort the remaining pool.
			refusal, err := targetRefusalFor(&store.Deployment{Engine: "vllm"}, nil, &store.Model{Name: "model"}, func(string) bool { return true }, resolve)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for i := range nodes {
				got[nodes[i].UUID] = reasonCode(refusal(&nodes[i]))
			}
			if got["healthy"] != "" || got["broken"] != scheduler.ReasonPlacementRejected {
				t.Fatalf("node refusals=%v", got)
			}
		})
	}
}

func TestTargetPoolResolvesOnlyDistinctHardwareShapes(t *testing.T) {
	calls := 0
	resolve := func(req LaunchRequest) (*proto.RequestedFeatures, error) {
		calls++
		return &proto.RequestedFeatures{CommandKnown: true}, nil
	}
	refusal, err := targetRefusalFor(&store.Deployment{Engine: "vllm"}, nil, &store.Model{Name: "model"}, func(string) bool { return true }, resolve)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		vendor, name := "nvidia", "H100"
		if i%2 == 0 {
			vendor, name = "amd", "MI300X"
		}
		node := &store.Node{UUID: fmt.Sprint(i), GPUVendor: vendor, OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{{Index: i % 7, Vendor: vendor, Name: name}}}
		if got := reasonCode(refusal(node)); got != "" {
			t.Fatalf("node %d refused: %s", i, got)
		}
	}
	if calls != 2 {
		t.Fatalf("resolved %d launches for 200 nodes / two hardware shapes", calls)
	}
}

// A shape shares command facts, never the node-local model/component paths
// that are checked while preparing the concrete launch.
func TestTargetPoolDoesNotCacheComponentPathFailure(t *testing.T) {
	selection := deploy.CatalogSelection{Components: []deploy.ComponentBinding{{Name: "draft", Role: "draft", StoreModelID: 2, MountPath: "/models/draft"}}}
	raw, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{Engine: "vllm", CatalogSelectionSnapshot: string(raw)}
	model := &store.Model{Name: "model"}
	node := &store.Node{UUID: "node", GPUVendor: "nvidia", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100"}}}
	calls := 0
	resolve := func(req LaunchRequest) (*proto.RequestedFeatures, error) {
		calls++
		return previewRequestedFeatures(req)
	}
	refusal, err := targetRefusalFor(dep, nil, model, func(string) bool { return true }, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if got := reasonCode(refusal(node)); got != "" {
		t.Fatalf("pure preview refused: %s", got)
	}
	if _, _, _, err := DeploymentLaunch(dep, nil, node, model, &ModelPaths{Primary: "/primary"}); err == nil {
		t.Fatal("missing component path accepted")
	}
	if _, _, _, err := DeploymentLaunch(dep, nil, node, model, &ModelPaths{Primary: "/primary", Components: map[uint]string{2: "/draft"}}); err != nil {
		t.Fatalf("another node's complete paths rejected: %v", err)
	}
	healthy := *node
	healthy.UUID = "healthy-node"
	if got := reasonCode(refusal(&healthy)); got != "" {
		t.Fatalf("pool preview reused concrete path error: %s", got)
	}
	if calls != 1 {
		t.Fatalf("node-local paths leaked into pure fact cache: resolves=%d", calls)
	}
}

func TestTargetPoolUsesTemplateCardRatherThanInventoryOrder(t *testing.T) {
	raw := `{"gpus":{"vendor":"amd"}}`
	dep := &store.Deployment{Engine: "vllm", CatalogSelectionSnapshot: raw}
	calls := 0
	resolve := func(req LaunchRequest) (*proto.RequestedFeatures, error) {
		calls++
		gpu, ok := req.Node.GPUByIndex(req.Placement.GPUIndexes[0])
		if !ok || req.Node.VendorOf(gpu) != "amd" {
			t.Fatalf("resolved template through wrong card: %+v", gpu)
		}
		return &proto.RequestedFeatures{CommandKnown: true}, nil
	}
	refusal, err := targetRefusalFor(dep, nil, &store.Model{Name: "model"}, func(string) bool { return true }, resolve)
	if err != nil {
		t.Fatal(err)
	}
	cards := []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100", ArchID: "sm_90"}, {Index: 1, Vendor: "amd", Name: "MI300X", ArchID: "gfx942"}}
	for _, reverse := range []bool{false, true} {
		ordered := append([]store.GPU(nil), cards...)
		if reverse {
			ordered[0], ordered[1] = ordered[1], ordered[0]
		}
		node := &store.Node{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: ordered}
		if got := reasonCode(refusal(node)); got != "" {
			t.Fatalf("reverse=%v refusal=%s", reverse, got)
		}
	}
	if calls != 1 {
		t.Fatalf("resolved %d AMD targets for identical unordered shapes", calls)
	}
}

func TestMixedCardFeaturesRemainBoundToTheirActualTargets(t *testing.T) {
	cards := []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100", ArchID: "sm_90"}, {Index: 1, Vendor: "amd", Name: "MI300X", ArchID: "gfx942"}}
	for _, reverse := range []bool{false, true} {
		ordered := append([]store.GPU(nil), cards...)
		if reverse {
			ordered[0], ordered[1] = ordered[1], ordered[0]
		}
		node := &store.Node{OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: ordered}
		resolve := func(req LaunchRequest) (*proto.RequestedFeatures, error) {
			gpu, _ := req.Node.GPUByIndex(req.Placement.GPUIndexes[0])
			return &proto.RequestedFeatures{CommandKnown: true, EngineSleep: node.VendorOf(gpu) == "nvidia"}, nil
		}
		requests, _ := targetRequestsFor(LaunchRequest{Node: node}, nil, targetRequestCache{}, resolve)
		snap := scheduler.NodeSnapshot{GPUs: []scheduler.GPU{{Index: 0}, {Index: 1}}}
		if refusal := constrainTargetGPUs(Input{Node: node, TargetRequests: requests}, &snap); refusal != nil || len(snap.GPUs) != 2 {
			t.Fatalf("reverse=%v cards=%v refusal=%+v", reverse, snap.GPUs, refusal)
		}
	}
}

func TestTargetPoolMemoizesMalformedTemplateByEffectiveShape(t *testing.T) {
	calls := 0
	resolve := func(req LaunchRequest) (*proto.RequestedFeatures, error) {
		calls++
		return previewRequestedFeatures(req)
	}
	// A valid stored override object with an invalid executable argv cannot
	// depend on node-local model paths or any external IO in this preview.
	dep := &store.Deployment{Engine: "vllm", ContainerOverrides: `{"entrypoint_display":"["}`}
	refusal, err := targetRefusalFor(dep, nil, &store.Model{Name: "model"}, func(string) bool { return true }, resolve)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		vendor, name := "nvidia", "H100"
		if i%2 == 0 {
			vendor, name = "amd", "MI300X"
		}
		node := &store.Node{UUID: fmt.Sprint(i), OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{{Index: 0, Vendor: vendor, Name: name}}}
		if got := reasonCode(refusal(node)); got != scheduler.ReasonPlacementRejected {
			t.Fatalf("node %d refusal=%q", i, got)
		}
	}
	if calls != 2 {
		t.Fatalf("malformed template resolved %d times for 200 nodes/two target shapes", calls)
	}
}

// The per-scan verdict cache is keyed by the whole hardware shape: nodes that
// differ only in runtime driver or card architecture are judged separately.
func TestTargetPoolVerdictCacheSeparatesShapes(t *testing.T) {
	dep := &store.Deployment{Engine: "vllm", CatalogSelectionSnapshot: `{"gpus":{"vendor":"amd","arch_id":"gfx942","name":"MI3\\d\\dX"}}`}
	refusal, err := targetRefusalFor(dep, nil, &store.Model{Name: "model"}, func(string) bool { return true }, func(LaunchRequest) (*proto.RequestedFeatures, error) {
		return &proto.RequestedFeatures{CommandKnown: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	node := func(uuid, driver, arch string) *store.Node {
		gpus := make([]store.GPU, 8)
		for i := range gpus {
			gpus[i] = store.GPU{Index: i, Vendor: "amd", Name: "MI325X", ArchID: arch}
		}
		return &store.Node{UUID: uuid, GPUVendor: "amd", OS: "linux", Arch: "amd64", RuntimeDriver: driver, GPUs: gpus}
	}
	// The refused shape comes first so a shape-blind cache would leak it.
	if got := reasonCode(refusal(node("hostproc", "host", "gfx942"))); got != scheduler.ReasonRuntimeUnsupported {
		t.Fatalf("host-process node verdict = %q, want %s", got, scheduler.ReasonRuntimeUnsupported)
	}
	for i := 0; i < 50; i++ {
		if got := reasonCode(refusal(node(fmt.Sprint("ok-", i), "docker", "gfx942"))); got != "" {
			t.Fatalf("compatible node %d refused: %s", i, got)
		}
	}
	if got := reasonCode(refusal(node("hostproc-2", "host", "gfx942"))); got != scheduler.ReasonRuntimeUnsupported {
		t.Fatalf("second host-process node verdict = %q", got)
	}
}

// go test ./internal/server/planning -run '^$' -bench TargetPoolRefusal -benchmem
func BenchmarkTargetPoolRefusal1000Nodes(b *testing.B) {
	dep := &store.Deployment{Engine: "vllm", CatalogSelectionSnapshot: `{"gpus":{"vendor":"amd","arch_id":"gfx942","name":"MI3\\d\\dX"}}`}
	nodes := make([]store.Node, 1000)
	for n := range nodes {
		gpus := make([]store.GPU, 8)
		for i := range gpus {
			gpus[i] = store.GPU{Index: i, Vendor: "amd", Name: "MI325X", ArchID: "gfx942"}
		}
		nodes[n] = store.Node{UUID: fmt.Sprint(n), GPUVendor: "amd", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: gpus}
	}
	resolve := func(LaunchRequest) (*proto.RequestedFeatures, error) {
		return &proto.RequestedFeatures{CommandKnown: true}, nil
	}
	b.ReportAllocs()
	for b.Loop() {
		refusal, err := targetRefusalFor(dep, nil, &store.Model{Name: "model"}, func(string) bool { return true }, resolve)
		if err != nil {
			b.Fatal(err)
		}
		for i := range nodes {
			if reasonCode(refusal(&nodes[i])) != "" {
				b.Fatal("refused")
			}
		}
	}
}

func reasonCode(reason *scheduler.Reason) string {
	if reason == nil {
		return ""
	}
	return reason.Code
}
