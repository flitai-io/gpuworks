package planning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A pool is judged one node per hardware shape: six small nodes of two card
// models cost two plans and are refused with the first shape's reason; one
// big node among them fits. Nodes the requirements exclude are not counted.
func TestEverFitsPlansOneNodePerHardwareShape(t *testing.T) {
	st, err := store.Open("file:planning_ever_fits?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/ever-fits", Name: "ever-fits", WeightBytes: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*store.Node
	add := func(name, card string, memory int64, labels string) {
		node, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		node.GPUVendor, node.Arch, node.Labels = "nvidia", "amd64", labels
		node.GPUs = []store.GPU{{Index: 0, Name: card, MemoryTotal: memory, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
		nodes = append(nodes, node)
	}
	for i := 0; i < 4; i++ {
		add(fmt.Sprintf("small-a-%d", i), "NVIDIA A10", 16<<30, `{"pool":"p"}`)
	}
	for i := 0; i < 2; i++ {
		add(fmt.Sprintf("small-b-%d", i), "NVIDIA A16", 16<<30, `{"pool":"p"}`)
	}
	add("big-outside", "NVIDIA H100", 80<<30, `{"pool":"other"}`)
	reqs := nodelabel.Requirements{"pool": {"p"}}
	built := 0
	build := func(node *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		built++
		return LaunchRequest{
			Engine: "vllm", Model: model, Node: node, Topology: DistTopology{NNodes: 1, Role: "standalone"},
			Source: LaunchSourceFromDeployment(&store.Deployment{Image: "registry.example/vllm:v1"}), ServedName: "ever-fits",
		}, deploy.CatalogSelection{}, nil
	}
	never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, reqs, build)
	if err != nil {
		t.Fatal(err)
	}
	if never == nil || never.Pool != 6 || never.Shapes != 2 || never.Node != "small-a-0" || never.Reason.Code == "" || built != 2 {
		t.Fatalf("never = %+v built %d, want pool 6, 2 shapes, refused on small-a-0 after 2 plans", never, built)
	}

	// Refused for memory, not for anything about the fixture.
	if never.Reason.Code != "no_fit" {
		t.Fatalf("refusal = %+v, want no_fit", never.Reason)
	}
	add("big-inside", "NVIDIA H100", 80<<30, `{"pool":"p"}`)
	built = 0
	if never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, reqs, build); err != nil || never != nil || built != 3 {
		t.Fatalf("with a big node in the pool: never = %+v err = %v built %d", never, err, built)
	}

	never, err = EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nodelabel.Requirements{"pool": {"nowhere"}}, build)
	if err != nil || never == nil || never.Pool != 0 || never.Shapes != 0 || never.Node != "" {
		t.Fatalf("an empty pool: never = %+v err = %v", never, err)
	}
}

func TestEverFitsSkipsUnsupportedAMDRuntimeAndTriesDocker(t *testing.T) {
	st, err := store.Open("file:planning_amd_runtime?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/amd", Name: "amd", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	card := store.GPU{Index: 1, Name: "AMD Instinct MI300X", MemoryTotal: 96 << 30, Vendor: "amd", ArchID: "gfx942"}
	nodes := []*store.Node{
		{ID: 1, Name: "hostproc", OS: "linux", Arch: "amd64", GPUVendor: "amd", RuntimeDriver: "host", GPUs: []store.GPU{card}},
		{ID: 2, Name: "docker", OS: "linux", Arch: "amd64", GPUVendor: "amd", RuntimeDriver: "docker", GPUs: []store.GPU{card}},
	}
	builds := 0
	build := func(node *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		builds++
		return LaunchRequest{Engine: "vllm", Model: model, Node: node, Placement: Placement{GPUIndexes: []int{1}, TP: 1}, Topology: DistTopology{NNodes: 1}, Source: LaunchSourceFromDeployment(&store.Deployment{Image: "registry.example/amd:v1"}), ServedName: "amd"}, deploy.CatalogSelection{}, nil
	}
	never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nil, build)
	if err != nil || never != nil || builds != 2 {
		t.Fatalf("docker should be tried after the host-process node: never=%+v err=%v builds=%d", never, err, builds)
	}
	if HardwareShape(nodes[0], true) == HardwareShape(nodes[1], true) {
		t.Fatal("different runtimes share one plan shape")
	}
	badConfig := func(node *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		req, layer, err := build(node)
		req.Placement.PP = 2
		return req, layer, err
	}
	if never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nil, badConfig); err == nil || never != nil {
		t.Fatalf("invalid parallelism must propagate after skipping the host-process node: never=%+v err=%v", never, err)
	}
}

func TestEverFitsRequiresEveryDataParallelReplicaCard(t *testing.T) {
	st, err := store.Open("file:ever_fits_dp?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/ever-fits-dp", Name: "ever-fits-dp", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("dp-pool", store.HashToken("token"), "dp-pool")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUVendor, node.Arch = "nvidia", "amd64"
	node.GPUs = []store.GPU{{Index: 0, Name: "NVIDIA GPU", MemoryTotal: 16 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	build := func(node *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		return LaunchRequest{Engine: "vllm", Model: model, Node: node, Placement: Placement{TP: 1}, Quick: deploy.LaunchConfig{DP: 2}, Topology: DistTopology{NNodes: 1, Role: "standalone"}, Source: LaunchSourceFromDeployment(&store.Deployment{Image: "registry.example/vllm:v1"}), ServedName: "ever-fits-dp"}, deploy.CatalogSelection{}, nil
	}
	nodes := []*store.Node{node}
	never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nil, build)
	if err != nil || never == nil || never.Reason.Code != "insufficient_empty_gpus" {
		t.Fatalf("one card: never=%+v err=%v", never, err)
	}
	node.GPUs = append(node.GPUs, store.GPU{Index: 1, Name: "NVIDIA GPU", MemoryTotal: 16 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"})
	never, err = EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nil, build)
	if err != nil || never != nil {
		t.Fatalf("two cards: never=%+v err=%v", never, err)
	}
}

// A node whose launch cannot be resolved cannot run the deployment, but it
// does not veto a pool where another hardware shape fits: a data-parallel
// template on a 2-card node and a 4-card node fits on the 4-card node
// whichever node is listed first. When no shape fits, the resolve error is
// still what the request is refused with.
func TestEverFitsUnresolvableShapeDoesNotVetoThePool(t *testing.T) {
	st, err := store.Open("file:ever_fits_unresolvable?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/ever-fits-unresolvable", Name: "ever-fits-unresolvable", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	node := func(name string, cards int) *store.Node {
		registered, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		registered.GPUVendor, registered.Arch = "nvidia", "amd64"
		for index := 0; index < cards; index++ {
			registered.GPUs = append(registered.GPUs, store.GPU{Index: index, Name: "NVIDIA H100", MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0"})
		}
		return registered
	}
	small, large := node("two-cards", 2), node("four-cards", 4)
	build := func(node *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		return LaunchRequest{Engine: "vllm", Model: model, Node: node, Placement: Placement{PP: 2},
			Selection:  deploy.CatalogSelection{ExtraArgs: []string{"--data-parallel-size", "2"}},
			Topology:   DistTopology{NNodes: 1, Role: "standalone"},
			Source:     LaunchSourceFromDeployment(&store.Deployment{Image: "registry.example/vllm:v1"}),
			ServedName: "ever-fits-unresolvable"}, deploy.CatalogSelection{}, nil
	}
	for _, order := range [][]*store.Node{{small, large}, {large, small}} {
		if never, err := EverFits(context.Background(), st, storeNetworks(st), order, NodeFacts{}, nil, build); err != nil || never != nil {
			t.Fatalf("order %s, %s: never=%+v err=%v; want the four-card node to fit", order[0].DisplayName(), order[1].DisplayName(), never, err)
		}
	}
	never, err := EverFits(context.Background(), st, storeNetworks(st), []*store.Node{small}, NodeFacts{}, nil, build)
	var unresolved *LaunchResolveError
	if never != nil || !errors.As(err, &unresolved) {
		t.Fatalf("only the two-card node: never=%+v err=%v; want its resolve error", never, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := EverFits(ctx, st, storeNetworks(st), []*store.Node{small, large}, NodeFacts{}, nil, build); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: err=%v", err)
	}
}

// Whether a card's dGPU backend is ready is part of the node's hardware shape:
// a memory-limited deployment fits on the ready node of a pool even when an
// otherwise identical node without the backend is listed first.
func TestEverFitsTellsDGPUReadyNodesApart(t *testing.T) {
	st, err := store.Open("file:ever_fits_dgpu_shape?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/ever-fits-dgpu", Name: "ever-fits-dgpu", WeightBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	node := func(name string, ready bool) *store.Node {
		registered, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		registered.GPUVendor, registered.Arch, registered.OS, registered.RuntimeDriver = "nvidia", "amd64", "linux", "docker"
		registered.GPUs = []store.GPU{{Index: 0, Name: "NVIDIA H100", MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0"}}
		if ready {
			index := 0
			assets, _ := json.Marshal([]proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}})
			probes, _ := json.Marshal([]proto.PartitionProbeResult{{Backend: accelerator.BackendNVIDIALogical, GPUIndex: &index, Status: proto.PartitionProbePassed}})
			registered.PartitionAssets, registered.PartitionProbes, registered.PartitionSafetyProtocol = string(assets), string(probes), 1
		}
		return registered
	}
	plain, ready := node("plain", false), node("dgpu-ready", true)
	if HardwareShape(plain, true) == HardwareShape(ready, true) {
		t.Fatal("a dGPU-ready node shares its shape with one whose backend is not ready")
	}
	build := func(node *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		return LaunchRequest{Engine: "vllm", Model: model, Node: node, VRAMLimitBytes: 20 << 30, PartitionBackend: accelerator.BackendNVIDIALogical,
			Topology:   DistTopology{NNodes: 1, Role: "standalone"},
			Source:     LaunchSourceFromDeployment(&store.Deployment{Image: "registry.example/vllm:v1"}),
			ServedName: "ever-fits-dgpu"}, deploy.CatalogSelection{}, nil
	}
	for _, order := range [][]*store.Node{{plain, ready}, {ready, plain}} {
		if never, err := EverFits(context.Background(), st, storeNetworks(st), order, NodeFacts{}, nil, build); err != nil || never != nil {
			t.Fatalf("order %s, %s: never=%+v err=%v; want the dGPU-ready node to fit", order[0].DisplayName(), order[1].DisplayName(), never, err)
		}
	}
	if never, err := EverFits(context.Background(), st, storeNetworks(st), []*store.Node{plain}, NodeFacts{}, nil, build); err != nil || never == nil {
		t.Fatalf("only the plain node: never=%+v err=%v; want a refusal", never, err)
	}
}
