package planning

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Exercise the public launch contract, including previews used by EverFits.
func TestKunlunLaunchRestrictionsAreRefusals(t *testing.T) {
	st, err := store.Open("file:kunlun-refusals?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, tc := range []struct {
		name, code string
		change     func(*LaunchRequest)
	}{
		{"runtime pending", "runtime_unreported", func(r *LaunchRequest) { r.Node.RuntimeDriver = "" }},
		{"runtime", "runtime_unsupported", func(r *LaunchRequest) { r.Node.RuntimeDriver = "containerd" }},
		{"arch", "runtime_unsupported", func(r *LaunchRequest) { r.Node.Arch = "arm64" }},
		{"OS", "runtime_unsupported", func(r *LaunchRequest) { r.Node.OS = "windows" }},
		{"sleep", "sleep_unsupported_on_node", func(r *LaunchRequest) { r.Sleepable = true }},
		{"PD", "pd_unsupported_on_node", func(r *LaunchRequest) { r.PD = &deploy.PDLaunch{} }},
		{"partition", "partition_unavailable", func(r *LaunchRequest) { r.VRAMLimitBytes = 1 << 30 }},
		{"sleep flag", "sleep_unsupported_on_node", func(r *LaunchRequest) { r.ExtraArgs = []string{"--enable-sleep-mode"} }},
		{"adjacent sleep flags", "sleep_unsupported_on_node", func(r *LaunchRequest) {
			r.ExtraArgs = []string{"--enable-sleep-mode=false", "--enable-sleep-mode"}
		}},
	} {
		for _, preview := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				r := kunlunReviewRequest(t)
				r.Preview = preview
				tc.change(&r)
				plan, reason, err := PlanTemplateLaunch(t.Context(), st, storeNetworks(st), r.Node, r, deploy.CatalogSelection{})
				wantCode := tc.code
				if preview && (wantCode == "sleep_unsupported_on_node" || wantCode == "pd_unsupported_on_node") {
					wantCode = "accelerator_feature_unsupported"
				}
				if err != nil || reason == nil || reason.Code != wantCode || plan.Output.Result.OK {
					t.Fatalf("plan=%+v reason=%+v err=%v", plan.Output.Result, reason, err)
				}
				if tc.code == "runtime_unsupported" {
					if reason.Params["required_os"] != "linux" || reason.Params["required_arch"] != "amd64" || reason.Params["required_runtime"] != "docker/nerdctl/kube" {
						t.Fatal(reason)
					}
				}
			})
		}
	}
}
func kunlunReviewRequest(t *testing.T) LaunchRequest {
	return LaunchRequest{Engine: "vllm", Model: &store.Model{ID: 1, WeightBytes: 1 << 30}, Quick: launchConfig(t, deploy.EngineVLLM),
		Node:   &store.Node{ID: 1, UUID: "kunlun", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUVendor: "kunlunxin", GPUs: []store.GPU{{Index: 0, UUID: "k0", Name: "P800 PCIe", Vendor: "kunlunxin", ArchID: "kl3", MemoryTotal: 96 << 30}}},
		Source: LaunchSourceFromDeployment(&store.Deployment{Image: "registry.example/kunlun:v1"}), Topology: DistTopology{NNodes: 1}, ModelMount: "/model"}
}
func TestKunlunMixedNodeUsesSelectedCardCapabilities(t *testing.T) {
	st, err := store.Open("file:kunlun-mixed?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, reverse := range []bool{false, true} {
		for _, indexes := range [][]int{nil, {3}} {
			r := kunlunReviewRequest(t)
			r.Node.GPUs = append(r.Node.GPUs, store.GPU{Index: 3, UUID: "n3", Vendor: "nvidia", Name: "H100", ArchID: "sm_90", MemoryTotal: 96 << 30})
			if reverse {
				slices.Reverse(r.Node.GPUs)
			}
			r.Sleepable = true
			r.Placement.GPUIndexes = indexes
			plan, reason, err := PlanTemplateLaunch(t.Context(), st, storeNetworks(st), r.Node, r, deploy.CatalogSelection{})
			if err != nil || reason != nil || !plan.Profile.Sleep.Supported || !slices.Equal(plan.Output.Result.Plan.GPUIndexes, []int{3}) {
				t.Fatalf("mixed selected %v: %+v %+v %v", indexes, plan.Output.Result, reason, err)
			}
			// Manual flags must select the same capable cards as the declaration,
			// before scheduling, independent of inventory order.
			r.Sleepable = false
			r.ExtraArgs = []string{"--enable-sleep-mode"}
			plan, reason, err = PlanTemplateLaunch(t.Context(), st, storeNetworks(st), r.Node, r, deploy.CatalogSelection{})
			if err != nil || reason != nil || !slices.Equal(plan.Output.Result.Plan.GPUIndexes, []int{3}) {
				t.Fatalf("manual flag: %+v %+v %v", plan.Output.Result, reason, err)
			}
			r.Placement.GPUIndexes = []int{0}
			_, reason, err = PlanTemplateLaunch(t.Context(), st, storeNetworks(st), r.Node, r, deploy.CatalogSelection{})
			if err != nil || reason == nil || reason.Code != "sleep_unsupported_on_node" {
				t.Fatal(reason, err)
			}
		}
	}
}
func TestKunlunEverFitsSleepIsIndependentOfNodeOrder(t *testing.T) {
	st, err := store.Open("file:kunlun-order?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := kunlunReviewRequest(t)
	nvidia := *base.Node
	nvidia.ID = 2
	nvidia.UUID = "nvidia"
	nvidia.GPUs = []store.GPU{{Index: 0, Vendor: "nvidia", Name: "H100", MemoryTotal: 96 << 30}}
	build := func(n *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		r := base
		r.Node = n
		r.Sleepable = true
		return r, deploy.CatalogSelection{}, nil
	}
	for _, nodes := range [][]*store.Node{{base.Node, &nvidia}, {&nvidia, base.Node}} {
		if never, err := EverFits(t.Context(), st, storeNetworks(st), nodes, NodeFacts{}, nil, build); err != nil || never != nil {
			t.Fatal(never, err)
		}
	}
	if never, err := EverFits(t.Context(), st, storeNetworks(st), []*store.Node{base.Node}, NodeFacts{}, nil, build); err != nil || never == nil || never.Reason.Code != "accelerator_feature_unsupported" {
		t.Fatal(never, err)
	}
}

// Browser regression: a Kunlun template plus a manual sleep flag must not
// select the NVIDIA peer on a mixed node just because that peer supports sleep.
func TestMixedNodeKeepsTemplateContractBeforeCapabilityFiltering(t *testing.T) {
	st, err := store.Open("file:mixed-template-capability?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, reverse := range []bool{false, true} {
		for _, preview := range []bool{false, true} {
			for _, vendor := range []string{"kunlunxin", "nvidia"} {
				r := kunlunReviewRequest(t)
				r.Preview = preview
				r.Node.GPUs = append(r.Node.GPUs, store.GPU{Index: 3, UUID: "n3", Vendor: "nvidia", Name: "H100", ArchID: "sm_90", MemoryTotal: 96 << 30})
				if reverse {
					slices.Reverse(r.Node.GPUs)
				}
				r.Selection.GPUs = []byte(`{"vendor":"` + vendor + `"}`)
				r.ExtraArgs = []string{"--enable-sleep-mode"}
				plan, err := PlanLaunch(t.Context(), st, storeNetworks(st), r)
				if err != nil {
					t.Fatal(err)
				}
				if vendor == "kunlunxin" {
					result := plan.Output.Result
					if result.OK || result.Plan != nil {
						t.Fatalf("reverse=%v preview=%v: Kunlun template selected another chip: %+v", reverse, preview, result)
					}
					if reverse {
						// The first card is H100, which the template rejects before
						// the scheduler can report the Kunlun sleep restriction.
						detail, _ := result.Message.Params["detail"].(string)
						if result.Message.Code != "placement_rejected" || !strings.Contains(detail, "H100") {
							t.Fatalf("reverse=%v preview=%v: wrong template refusal: %+v", reverse, preview, result)
						}
					} else {
						wantCode := "sleep_unsupported_on_node"
						if preview {
							// Preview resolves rejected plans to expose the product refusal.
							wantCode = "accelerator_feature_unsupported"
						}
						if result.Message.Code != wantCode || result.Message.Params["feature"] != "engine_sleep" || result.Message.Params["vendor"] != "Kunlunxin" {
							t.Fatalf("reverse=%v preview=%v: wrong sleep refusal: %+v", reverse, preview, result)
						}
					}
				} else if !plan.Output.Result.OK || !slices.Equal(plan.Output.Result.Plan.GPUIndexes, []int{3}) {
					t.Fatalf("NVIDIA peer rejected: %+v", plan.Output.Result)
				}
				r.ExtraArgs = nil
				plan, err = PlanLaunch(t.Context(), st, storeNetworks(st), r)
				want := 0
				if vendor == "nvidia" {
					want = 3
				}
				if err != nil || !plan.Output.Result.OK || !slices.Equal(plan.Output.Result.Plan.GPUIndexes, []int{want}) {
					t.Fatalf("template selection: %+v %v", plan.Output.Result, err)
				}
			}
		}
	}
}

// Supported whole-card modes must reach placement in both preview and launch.
func TestKunlunSupportedParallelModesReachPlacement(t *testing.T) {
	st, err := store.Open("file:kunlun-parallel-modes?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, tc := range []struct {
		name           string
		pipeline, data int
	}{
		{name: "pipeline parallel", pipeline: 2},
		{name: "engine data parallel", data: 2},
	} {
		for _, preview := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				r := kunlunReviewRequest(t)
				r.Preview = preview
				r.Node.GPUs = append(r.Node.GPUs, store.GPU{Index: 1, UUID: "k1", Name: "P800 PCIe", Vendor: "kunlunxin", ArchID: "kl3", MemoryTotal: 96 << 30})
				r.Placement = Placement{GPUIndexes: []int{0, 1}, TP: 1, PP: tc.pipeline}
				r.Quick.DP = tc.data
				plan, reason, err := PlanTemplateLaunch(t.Context(), st, storeNetworks(st), r.Node, r, deploy.CatalogSelection{})
				if err != nil || reason != nil || !plan.Output.Result.OK || !slices.Equal(plan.Output.Result.Plan.GPUIndexes, []int{0, 1}) {
					t.Fatalf("preview=%v: plan=%+v reason=%+v err=%v", preview, plan.Output.Result, reason, err)
				}
				features := plan.Resolved.RequestedFeatures
				if features == nil || features.PipelineParallel != (tc.pipeline > 1) || features.DataParallel != (tc.data > 1) {
					t.Fatalf("preview=%v: resolved features=%+v", preview, features)
				}
			})
		}
	}
}
