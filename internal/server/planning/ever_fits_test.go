package planning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
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
	_, never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, reqs, build)
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
	if _, never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, reqs, build); err != nil || never != nil || built != 3 {
		t.Fatalf("with a big node in the pool: never = %+v err = %v built %d", never, err, built)
	}

	_, never, err = EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nodelabel.Requirements{"pool": {"nowhere"}}, build)
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
	_, never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nil, build)
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
	if _, never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nil, badConfig); err == nil || never != nil {
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
	_, never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nil, build)
	if err != nil || never == nil || never.Reason.Code != "insufficient_empty_gpus" {
		t.Fatalf("one card: never=%+v err=%v", never, err)
	}
	node.GPUs = append(node.GPUs, store.GPU{Index: 1, Name: "NVIDIA GPU", MemoryTotal: 16 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"})
	_, never, err = EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nil, build)
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
		if _, never, err := EverFits(context.Background(), st, storeNetworks(st), order, NodeFacts{}, nil, build); err != nil || never != nil {
			t.Fatalf("order %s, %s: never=%+v err=%v; want the four-card node to fit", order[0].DisplayName(), order[1].DisplayName(), never, err)
		}
	}
	_, never, err := EverFits(context.Background(), st, storeNetworks(st), []*store.Node{small}, NodeFacts{}, nil, build)
	var unresolved *LaunchResolveError
	if never != nil || !errors.As(err, &unresolved) {
		t.Fatalf("only the two-card node: never=%+v err=%v; want its resolve error", never, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := EverFits(ctx, st, storeNetworks(st), []*store.Node{small, large}, NodeFacts{}, nil, build); !errors.Is(err, context.Canceled) {
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
		if _, never, err := EverFits(context.Background(), st, storeNetworks(st), order, NodeFacts{}, nil, build); err != nil || never != nil {
			t.Fatalf("order %s, %s: never=%+v err=%v; want the dGPU-ready node to fit", order[0].DisplayName(), order[1].DisplayName(), never, err)
		}
	}
	if _, never, err := EverFits(context.Background(), st, storeNetworks(st), []*store.Node{plain}, NodeFacts{}, nil, build); err != nil || never == nil {
		t.Fatalf("only the plain node: never=%+v err=%v; want a refusal", never, err)
	}
}

// NeverFitting answers node by node what EverFits answers for a pool: every
// node of a shape that could not run the launch unloaded carries that shape's
// refusal, planned once per shape; a node that fits is absent. A shape whose
// launch cannot be resolved is a rejected placement when another shape fits,
// and is left out when none does, where the error may be the request's own.
func TestNeverFittingAnswersEachNodeOncePerShape(t *testing.T) {
	st, err := store.Open("file:planning_never_fitting?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/never-fitting", Name: "never-fitting", WeightBytes: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	node := func(name, card string, cards int, memory int64) *store.Node {
		registered, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		registered.GPUVendor, registered.Arch = "nvidia", "amd64"
		for index := 0; index < cards; index++ {
			registered.GPUs = append(registered.GPUs, store.GPU{Index: index, Name: card, MemoryTotal: memory, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"})
		}
		return registered
	}
	smallA, smallB, big := node("small-a", "NVIDIA A10", 1, 16<<30), node("small-b", "NVIDIA A10", 1, 16<<30), node("big", "NVIDIA H100", 1, 80<<30)
	built := 0
	build := func(node *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		built++
		return LaunchRequest{
			Engine: "vllm", Model: model, Node: node, Topology: DistTopology{NNodes: 1, Role: "standalone"},
			Source: LaunchSourceFromDeployment(&store.Deployment{Image: "registry.example/vllm:v1"}), ServedName: "never-fitting",
		}, deploy.CatalogSelection{}, nil
	}
	unloaded := func(build func(*store.Node) (LaunchRequest, deploy.CatalogSelection, error)) func(*store.Node) (*scheduler.Reason, error) {
		return func(node *store.Node) (*scheduler.Reason, error) {
			req, layer, err := build(node)
			if err != nil {
				return nil, err
			}
			plan, refusal, err := PlanTemplateLaunch(context.Background(), st, storeNetworks(st), node, req.UnloadedPreview(), layer)
			return UnloadedRefusal(node, req, plan, refusal, err)
		}
	}
	never, fits, err := NeverFitting(context.Background(), storeNetworks(st), []*store.Node{smallA, big, smallB}, nil, unloaded(build))
	if err != nil || !fits {
		t.Fatalf("fits=%v err=%v", fits, err)
	}
	if len(never) != 2 || never[smallA.UUID].Code != "no_fit" || never[smallB.UUID].Code != "no_fit" || built != 2 {
		t.Fatalf("never = %+v after %d plans, want both 16 GiB nodes refused no_fit after 2 plans", never, built)
	}
	if _, refused := never[big.UUID]; refused {
		t.Fatalf("the 80 GiB node fits a 20 GiB model: %+v", never[big.UUID])
	}

	// Two data-parallel workers of two pipeline stages need four cards. The
	// two-card node cannot hold them: that is its refusal, in the scheduler's
	// words, with or without a node that can. The launch the preview still
	// assembles for it, on one card nobody chose, is not what it is told.
	twoCards, fourCards := node("two-cards", "NVIDIA H100", 2, 80<<30), node("four-cards", "NVIDIA H100", 4, 80<<30)
	parallel := func(node *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		return LaunchRequest{Engine: "vllm", Model: model, Node: node, Placement: Placement{PP: 2},
			Selection:  deploy.CatalogSelection{ExtraArgs: []string{"--data-parallel-size", "2"}},
			Topology:   DistTopology{NNodes: 1, Role: "standalone"},
			Source:     LaunchSourceFromDeployment(&store.Deployment{Image: "registry.example/vllm:v1"}),
			ServedName: "never-fitting"}, deploy.CatalogSelection{}, nil
	}
	for _, pool := range [][]*store.Node{{twoCards, fourCards}, {twoCards}} {
		never, _, err = NeverFitting(context.Background(), storeNetworks(st), pool, nil, unloaded(parallel))
		if _, detailed := never[twoCards.UUID].Params["detail"]; err != nil || len(never) != 1 || never[twoCards.UUID].Code != "no_fit" || detailed {
			t.Fatalf("pool of %d: never = %+v err=%v, want only the two-card node refused no_fit", len(pool), never, err)
		}
	}

	// A launch that cannot be resolved on cards the node does give is the
	// planner's error. Such a kind is a rejected placement, worded as a create
	// words it, when another kind fits; when none does the error may be the
	// request's own, and no node is turned down for it.
	stated := func(node *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		req, layer, err := build(node)
		req.Placement, req.ExtraArgs = Placement{TP: 1}, []string{"--tensor-parallel-size", "2"}
		return req, layer, err
	}
	var resolveErr *LaunchResolveError
	if _, err := unloaded(stated)(big); !errors.As(err, &resolveErr) {
		t.Fatalf("a command stating two cards on a placement of one: err=%v, want the resolve error", err)
	}
	unresolvedOn := func(target *store.Node) func(*store.Node) (*scheduler.Reason, error) {
		return func(node *store.Node) (*scheduler.Reason, error) {
			if node.UUID == target.UUID {
				return unloaded(stated)(node)
			}
			return unloaded(build)(node)
		}
	}
	never, _, err = NeverFitting(context.Background(), storeNetworks(st), []*store.Node{fourCards, big}, nil, unresolvedOn(fourCards))
	detail, _ := never[fourCards.UUID].Params["detail"].(string)
	if err != nil || len(never) != 1 || never[fourCards.UUID].Code != "placement_rejected" || !strings.HasPrefix(detail, "resolve launch: vllm topology: ") {
		t.Fatalf("never = %+v err=%v, want only the unresolvable node rejected with the error as a create words it", never, err)
	}
	if never, fits, err = NeverFitting(context.Background(), storeNetworks(st), []*store.Node{fourCards}, nil, unresolvedOn(fourCards)); err != nil || fits || len(never) != 0 {
		t.Fatalf("only the unresolvable kind: never=%+v fits=%v err=%v; want no node turned down", never, fits, err)
	}

	// A kind whose plan fails for another reason (the image may not be
	// pulled there, a store read) is left unjudged; the others are answered,
	// and the failure is reported once.
	failing := errors.New("image may not be pulled on this node")
	never, fits, err = NeverFitting(context.Background(), storeNetworks(st), []*store.Node{big, smallA, smallB}, nil, func(node *store.Node) (*scheduler.Reason, error) {
		if node.UUID == big.UUID {
			return nil, failing
		}
		return unloaded(build)(node)
	})
	if !errors.Is(err, failing) || fits || len(never) != 2 || never[smallA.UUID].Code != "no_fit" {
		t.Fatalf("one kind unplannable: never=%+v fits=%v err=%v; want the 16 GiB nodes still refused and the failure reported", never, fits, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if never, _, err := NeverFitting(ctx, storeNetworks(st), []*store.Node{smallA, big}, nil, unloaded(build)); !errors.Is(err, context.Canceled) || never != nil {
		t.Fatalf("canceled context: never=%+v err=%v", never, err)
	}
}

// Two nodes of one hardware shape may differ in what else an unloaded plan
// reads of them: the cache cap in effect on each. own names that, so a node
// is answered with a node that agrees in it, in whatever order the nodes
// come: a model larger than one cap is refused on exactly the nodes with that
// cap, at one plan per cap.
func TestNeverFittingAnswersANodeByItsOwnStorage(t *testing.T) {
	st, err := store.Open("file:planning_never_fitting_storage?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	caps := map[string]string{}
	node := func(name, limit string) *store.Node {
		registered, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		registered.GPUVendor, registered.Arch = "nvidia", "amd64"
		registered.GPUs = []store.GPU{{Index: 0, Name: "NVIDIA A100", MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
		caps[registered.UUID] = limit
		return registered
	}
	smallCap, largeCap, alsoSmallCap := node("small-cap", "small"), node("large-cap", "large"), node("also-small-cap", "small")
	for _, order := range [][]*store.Node{{largeCap, smallCap, alsoSmallCap}, {smallCap, largeCap, alsoSmallCap}, {smallCap, alsoSmallCap, largeCap}} {
		asked := 0
		never, fits, err := NeverFitting(context.Background(), storeNetworks(st), order, func(n *store.Node) string { return caps[n.UUID] }, func(n *store.Node) (*scheduler.Reason, error) {
			asked++
			if caps[n.UUID] == "large" {
				return nil, nil
			}
			return &scheduler.Reason{Code: scheduler.ReasonModelExceedsCacheLimit}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, refused := never[largeCap.UUID]; refused || !fits || len(never) != 2 || asked != 2 ||
			never[smallCap.UUID].Code != scheduler.ReasonModelExceedsCacheLimit || never[alsoSmallCap.UUID].Code != scheduler.ReasonModelExceedsCacheLimit {
			t.Fatalf("order %s: never = %+v fits=%v after %d plans, want the two small-cap nodes refused after 2 plans", order[0].UUID, never, fits, asked)
		}
	}
}

// However many kinds of node a fleet has, one answer costs at most
// NeverFittingBudget plans; the nodes of the kinds not asked stay on offer.
func TestNeverFittingAsksNoMoreKindsThanItsBudget(t *testing.T) {
	st, err := store.Open("file:planning_never_fitting_budget?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var nodes []*store.Node
	for i := 0; i < NeverFittingBudget+9; i++ {
		name := fmt.Sprintf("kind-%02d", i)
		registered, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		registered.GPUVendor, registered.Arch = "nvidia", "amd64"
		registered.GPUs = []store.GPU{{Index: 0, Name: "NVIDIA A100", MemoryTotal: int64(i+1) << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
		nodes = append(nodes, registered)
	}
	asked := 0
	never, fits, err := NeverFitting(context.Background(), storeNetworks(st), nodes, nil, func(*store.Node) (*scheduler.Reason, error) {
		asked++
		return &scheduler.Reason{Code: scheduler.ReasonNoFit}, nil
	})
	if err != nil || fits || asked != NeverFittingBudget || len(never) != NeverFittingBudget {
		t.Fatalf("asked %d kinds and refused %d nodes (fits=%v err=%v), want %d of each", asked, len(never), fits, err, NeverFittingBudget)
	}
	if _, refused := never[nodes[len(nodes)-1].UUID]; refused {
		t.Fatal("a node of a kind beyond the budget was turned down without being asked")
	}
}
