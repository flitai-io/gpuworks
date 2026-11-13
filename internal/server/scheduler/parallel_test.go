package scheduler

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/parallelism"
)

// A stated parallelism fixes the group on this node to its per-node share and
// spreads the weights over every GPU of the deployment.
func TestScheduleStatedParallelism(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 48, true, 0), gpu(1, 48, true, 0)},
	}
	// 64 GiB of weights cannot sit on one 48 GiB GPU, but TP 2 across two
	// one-GPU nodes holds half of them per GPU.
	req := Request{WeightBytes: 64 * giB, HeadCount: 32, GPUMemUtil: 0.9, Parallel: parallelism.Spec{TP: 2, NNodes: 2}}
	res := Schedule(req, node)
	if !res.OK || res.Plan.TP != 1 || len(res.Plan.GPUIndexes) != 1 {
		t.Fatalf("TP 2 over two nodes = %+v %v", res.Plan, res.Message)
	}
	if single := Schedule(Request{WeightBytes: 64 * giB, HeadCount: 32, GPUMemUtil: 0.9, AllowedTPs: []int{1}}, node); single.OK {
		t.Fatal("the same weights on one GPU alone must not fit")
	}

	// TP 2 × PP 2 on two nodes is two GPUs per node; AllowedTPs is ignored.
	req = Request{WeightBytes: 64 * giB, HeadCount: 32, GPUMemUtil: 0.9, AllowedTPs: []int{1}, Parallel: parallelism.Spec{TP: 2, PP: 2, NNodes: 2}}
	if res := Schedule(req, node); !res.OK || res.Plan.TP != 2 {
		t.Fatalf("TP 2 × PP 2 over two nodes = %+v %v", res.Plan, res.Message)
	}

	// The heads are split over the deployment's TP, not this node's share.
	req = Request{WeightBytes: 8 * giB, HeadCount: 12, GPUMemUtil: 0.9, Parallel: parallelism.Spec{TP: 8, NNodes: 8}}
	res = Schedule(req, node)
	if res.OK || res.Message.Code != ReasonTPHeadCountNotDivisible || res.Message.Params["tp"] != 8 {
		t.Fatalf("12 heads over TP 8 = %+v", res)
	}

	// More GPUs per node than the node has.
	if res := Schedule(Request{WeightBytes: 8 * giB, HeadCount: 32, GPUMemUtil: 0.9, Parallel: parallelism.Spec{TP: 8, NNodes: 2}}, node); res.OK {
		t.Fatalf("four GPUs per node on a two-GPU node = %+v", res.Plan)
	}
	// A spec its nodes cannot share leaves nothing to place.
	if res := Schedule(Request{WeightBytes: 8 * giB, HeadCount: 32, GPUMemUtil: 0.9, Parallel: parallelism.Spec{TP: 3, NNodes: 2}}, node); res.OK {
		t.Fatalf("indivisible spec = %+v", res.Plan)
	}
}

// A stated PP with the TP left to the scheduler sizes the group in multiples
// of PP: a small model gets PP GPUs (TP 1 per stage), never fewer GPUs than
// it has stages, and the head count is checked against the per-stage TP.
func TestScheduleStatedPPWithoutTP(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 48, true, 0), gpu(1, 48, true, 0), gpu(2, 48, true, 0), gpu(3, 48, true, 0)},
	}
	req := Request{WeightBytes: 2 * giB, HeadCount: 32, GPUMemUtil: 0.9, Parallel: parallelism.Spec{PP: 2}}
	res := Schedule(req, node)
	if !res.OK || len(res.Plan.GPUIndexes) != 2 {
		t.Fatalf("PP 2 with TP unstated = %+v %v, want a two-GPU group", res.Plan, res.Message)
	}
	if got := req.Parallel.Assigned(len(res.Plan.GPUIndexes)); got.TP != 1 || got.PP != 2 {
		t.Fatalf("assigned parallelism = %+v, want TP 1 × PP 2", got)
	}

	// 3 heads divide a per-stage TP of 1 (group 2) but not the group itself.
	req = Request{WeightBytes: 2 * giB, HeadCount: 3, GPUMemUtil: 0.9, Parallel: parallelism.Spec{PP: 2}}
	if res := Schedule(req, node); !res.OK || len(res.Plan.GPUIndexes) != 2 {
		t.Fatalf("3 heads over PP 2 × TP 1 = %+v %v", res.Plan, res.Message)
	}

	// More stages than the node has GPUs leaves nothing to place.
	if res := Schedule(Request{WeightBytes: 2 * giB, HeadCount: 32, GPUMemUtil: 0.9, Parallel: parallelism.Spec{PP: 8}}, node); res.OK {
		t.Fatalf("PP 8 on a four-GPU node placed %+v", res.Plan)
	}
}

// A reason names the GPU group by its size and the deployment's own TP and
// PP: TP 2 × PP 2 needs four GPUs, and must never read as "tp=4".
func TestScheduleReasonsSeparateGroupSizeFromTP(t *testing.T) {
	node := NodeSnapshot{
		UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50,
		GPUs: []GPU{gpu(0, 48, true, 0), gpu(1, 48, true, 0)},
	}
	res := Schedule(Request{WeightBytes: 2 * giB, HeadCount: 32, GPUMemUtil: 0.9, Parallel: parallelism.Spec{TP: 2, PP: 2}}, node)
	if res.OK || res.Message.Code != ReasonInsufficientEmptyGPUs {
		t.Fatalf("TP 2 × PP 2 on two GPUs = %+v", res)
	}
	if p := res.Message.Params; p["gpus"] != 4 || p["tp"] != 2 || p["pp"] != 2 || p["available"] != 2 {
		t.Fatalf("params = %+v, want gpus 4, tp 2, pp 2, available 2", p)
	}

	res = Schedule(Request{WeightBytes: 2 * giB, HeadCount: 32, GPUMemUtil: 0.9, Parallel: parallelism.Spec{PP: 2}}, node)
	if !res.OK {
		t.Fatalf("PP 2 on two GPUs = %+v", res)
	}
	if p := res.Plan.Reasons[0].Params; p["gpus"] != 2 || p["tp"] != 1 || p["pp"] != 2 {
		t.Fatalf("topology params = %+v, want gpus 2, tp 1, pp 2", p)
	}
}

func TestScheduleAllocatesDistinctGPUsForDataParallel(t *testing.T) {
	node := NodeSnapshot{UUID: "n1", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: []GPU{gpu(0, 48, true, 0), gpu(1, 48, true, 0), gpu(2, 48, true, 0), gpu(3, 48, true, 0)}}
	for _, tc := range []struct {
		spec parallelism.Spec
		want int
	}{
		{parallelism.Spec{TP: 1, DP: 2}, 2},
		{parallelism.Spec{TP: 2, DP: 2}, 4},
		{parallelism.Spec{TP: 2, DP: 2, NNodes: 2}, 2},
	} {
		req := Request{WeightBytes: 2 * giB, HeadCount: 32, GPUMemUtil: 0.9, Parallel: tc.spec}
		res := Schedule(req, node)
		if !res.OK || len(res.Plan.GPUIndexes) != tc.want {
			t.Fatalf("%+v: %+v %v", tc.spec, res.Plan, res.Message)
		}
		if perGPUNeed(req, tc.want) != perGPUNeed(Request{WeightBytes: req.WeightBytes, GPUMemUtil: req.GPUMemUtil, Parallel: parallelism.Spec{TP: tc.spec.TP, PP: tc.spec.PP, NNodes: tc.spec.NNodes}}, tc.want) {
			t.Fatalf("DP diluted one replica's weights: %+v", tc.spec)
		}
	}
}
