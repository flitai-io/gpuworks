package planning

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

func gpuCandidatesFixture(t *testing.T, state, phase, policy string) (*store.Store, Input, *store.Deployment) {
	t.Helper()
	st, err := store.Open("file:choices_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("choices-node", store.HashToken("token"), "choices-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{
		{Index: 0, UUID: "choices-gpu-0", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 64 << 30},
		{Index: 1, UUID: "choices-gpu-1", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 64 << 30},
	}
	dep := &store.Deployment{Name: "holder", ServedName: "holder", ModelID: 1, NodeID: node.ID, Engine: "vllm", Sleepable: true, State: "active"}
	p, violation := yieldpolicy.Parse(policy, yieldpolicy.Overrides{})
	if violation != nil {
		t.Fatal(violation)
	}
	dep.SetPolicy(p)
	dep, err = st.CreateDeployment(dep)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.CreateInstance(&store.Instance{UUID: "holder-instance", DeploymentID: dep.ID, NodeID: node.ID, State: state, Phase: phase, GPUIndexes: "[0]", DesiredStateJSON: sleepSpec("holder-instance", 0)})
	if err != nil {
		t.Fatal(err)
	}
	return st, Input{Model: &store.Model{WeightBytes: 1 << 30}, Node: node, Engine: "vllm", Quick: deploy.LaunchConfig{GPUMemUtil: 0.9}, YieldPolicy: yieldpolicy.Never}, dep
}

func TestGPUCandidatesFollowInstanceLedgerStates(t *testing.T) {
	for _, tc := range []struct {
		state, phase string
		available    bool
	}{
		{proto.InstPending, "", false}, {proto.InstPulling, "", false}, {proto.InstStarting, "", false},
		{proto.InstOptimizing, "", false}, {proto.InstRunning, "", false},
		{proto.InstFailed, proto.PhaseSleep, false}, {proto.InstFailed, "pull", true}, {proto.InstStopped, "", true},
	} {
		t.Run(tc.state+tc.phase, func(t *testing.T) {
			st, in, _ := gpuCandidatesFixture(t, tc.state, tc.phase, yieldpolicy.Never)
			in.GPUIndexes = []int{0}
			out := in.Evaluate(st)
			if out.GPUCandidates == nil || candidateAvailable(t, out.GPUCandidates, 0) != tc.available || !candidateAvailable(t, out.GPUCandidates, 1) {
				t.Fatalf("choices=%+v, occupied card selectable=%v", out.GPUCandidates, tc.available)
			}
		})
	}
}

func TestGPUCandidatesEditAndReleaseUseCurrentLedger(t *testing.T) {
	st, in, dep := gpuCandidatesFixture(t, proto.InstRunning, "", yieldpolicy.Never)
	if candidateAvailable(t, in.Evaluate(st).GPUCandidates, 0) {
		t.Fatal("occupied card offered to a newcomer")
	}
	in.ExcludeDeploymentID = dep.ID
	if !candidateAvailable(t, in.Evaluate(st).GPUCandidates, 0) {
		t.Fatal("edit was charged its own replaced instance")
	}
	in.ExcludeDeploymentID = 0
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "holder-instance", State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	if !candidateAvailable(t, in.Evaluate(st).GPUCandidates, 0) {
		t.Fatal("released card did not become selectable")
	}
}

func TestGPUCandidatesDistinguishWholeAllocationFromSharedClaims(t *testing.T) {
	for _, tc := range []struct {
		name, desired, state string
		copies               int
		exclude              bool
		want                 string
	}{
		{"whole allocation", `{}`, proto.InstRunning, 1, false, "gpu_whole_allocated"},
		{"pending whole allocation", `{}`, proto.InstPending, 1, false, "gpu_whole_allocated"},
		{"dGPU partial", `{"vram_limit_bytes":17179869184,"partition_backend":"nvidia-dgpu","engine":"vllm"}`, proto.InstRunning, 1, false, "no_free_gpu"},
		{"shared full capacity", `{"memory_claim_bytes":68719476736}`, proto.InstRunning, 1, false, "no_free_gpu"},
		{"shared claims fill capacity", `{"memory_claim_bytes":34359738368}`, proto.InstRunning, 2, false, "no_free_gpu"},
		{"released whole allocation", `{}`, proto.InstStopped, 1, false, ""},
		{"edit excludes whole allocation", `{}`, proto.InstRunning, 1, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, in, dep := gpuCandidatesFixture(t, tc.state, "", yieldpolicy.Never)
			if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", "holder-instance").Update("DesiredStateJSON", tc.desired).Error; err != nil {
				t.Fatal(err)
			}
			for i := 1; i < tc.copies; i++ {
				if _, err := st.CreateInstance(&store.Instance{UUID: fmt.Sprintf("shared-%d", i), DeploymentID: dep.ID, NodeID: in.Node.ID, State: tc.state, GPUIndexes: "[0]", DesiredStateJSON: tc.desired}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.exclude {
				in.ExcludeDeploymentID = dep.ID
			}
			out := in.Evaluate(st)
			if len(out.GPUCandidates) != 2 {
				t.Fatalf("candidates = %+v", out.GPUCandidates)
			}
			candidate := out.GPUCandidates[0]
			code := ""
			if candidate.Reason != nil {
				code = candidate.Reason.Code
			}
			if code != tc.want || candidate.Available != (tc.want == "") || !out.GPUCandidates[1].Available {
				t.Fatalf("reason = %q, want %q; candidates = %+v", code, tc.want, out.GPUCandidates)
			}
		})
	}
}

func TestGPUCandidatesIncludeInventoryOnlyCardsWithoutSchedulingThem(t *testing.T) {
	st, in, _ := gpuCandidatesFixture(t, proto.InstRunning, "", yieldpolicy.Never)
	in.Node.GPUs = append(in.Node.GPUs,
		store.GPU{Index: 3, UUID: "passed-through", Ownership: accelerator.CardPassedThrough},
		store.GPU{Index: -1, UUID: "unbound", Ownership: accelerator.CardWithoutDriver},
	)
	out := in.Evaluate(st)
	if len(out.GPUCandidates) != 4 || candidateAvailable(t, out.GPUCandidates, 3) || candidateAvailable(t, out.GPUCandidates, -1) {
		t.Fatalf("inventory cards missing or available: %+v", out.GPUCandidates)
	}
	if !out.Result.OK || len(out.Result.Plan.GPUIndexes) != 1 || out.Result.Plan.GPUIndexes[0] != 1 {
		t.Fatalf("inventory-only cards changed allocation: %+v", out.Result)
	}
}

func TestGPUCandidatesReportAwakeOccupancyWithoutChangingYield(t *testing.T) {
	for _, owner := range []string{yieldpolicy.Never, yieldpolicy.Idle, yieldpolicy.TimeSlice, yieldpolicy.Anytime} {
		for _, newcomer := range []string{yieldpolicy.Never, yieldpolicy.Idle, yieldpolicy.TimeSlice, yieldpolicy.Anytime} {
			t.Run(owner+"/"+newcomer, func(t *testing.T) {
				st, in, _ := gpuCandidatesFixture(t, proto.InstRunning, "", owner)
				in.YieldPolicy = newcomer
				out := in.Evaluate(st)
				want := false // An awake holder remains occupied even when a new launch may wait.
				if !out.Result.OK || !candidateAvailable(t, out.GPUCandidates, 1) || candidateAvailable(t, out.GPUCandidates, 0) != want {
					t.Fatalf("output=%+v choices=%+v", out.Result, out.GPUCandidates)
				}
				in.GPUIndexes = []int{0}
				pinned := in.Evaluate(st)
				mayWait := owner != yieldpolicy.Never && newcomer != yieldpolicy.Never
				if pinned.Result.OK || (pinned.Yield != nil) != mayWait {
					t.Fatalf("occupied card scheduling changed: result=%+v yield=%+v", pinned.Result, pinned.Yield)
				}
			})
		}
	}
}

func TestGPUCandidatesDoNotYieldToSiblingReplicas(t *testing.T) {
	st, in, dep := gpuCandidatesFixture(t, proto.InstRunning, "", yieldpolicy.Anytime)
	in.YieldPolicy = yieldpolicy.Idle
	in.DeploymentID = dep.ID
	if candidateAvailable(t, in.Evaluate(st).GPUCandidates, 0) {
		t.Fatal("a sibling replica was offered its own deployment's card")
	}
}

func TestGPUCandidatesPreserveTargetValidationAndKeepAlternatives(t *testing.T) {
	for _, selected := range [][]int{{0}, {1}, {0, 1}, {99}} {
		t.Run(fmt.Sprint(selected), func(t *testing.T) {
			st, in, _ := gpuCandidatesFixture(t, proto.InstStopped, "", yieldpolicy.Never)
			in.GPUIndexes = selected
			in.ValidateGPU = func(gpu store.GPU) error {
				if gpu.Index == 0 {
					return errors.New("template requires the other card")
				}
				return nil
			}
			out := in.Evaluate(st)
			valid := len(selected) == 1 && selected[0] == 1
			if out.Result.OK != valid {
				t.Fatalf("selection %v bypassed validation: %+v", selected, out.Result)
			}
			if valid {
				if len(out.GPUCandidates) != 2 || !candidateAvailable(t, out.GPUCandidates, 0) || !candidateAvailable(t, out.GPUCandidates, 1) {
					t.Fatalf("selected range hid other resources: %+v", out.GPUCandidates)
				}
			} else if out.GPUCandidates != nil {
				t.Fatal("early target refusal should not reorder the evaluation to compute hints")
			}
		})
	}
}

func TestGPUCandidatesRespectSleepingReservations(t *testing.T) {
	for _, owner := range []string{yieldpolicy.Never, yieldpolicy.Idle, yieldpolicy.TimeSlice, yieldpolicy.Anytime} {
		for _, newcomer := range []string{yieldpolicy.Never, yieldpolicy.Idle, yieldpolicy.TimeSlice, yieldpolicy.Anytime} {
			for _, level := range []int{1, 2} {
				for _, held := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/level-%d/held-%v", owner, newcomer, level, held), func(t *testing.T) {
						st, in, dep := gpuCandidatesFixture(t, proto.InstRunning, "", owner)
						in.YieldPolicy = newcomer
						if err := st.OrderInstancesSleepHeld(dep.ID, []string{"holder-instance"}, level, proto.SleepModeWait, held); err != nil {
							t.Fatal(err)
						}
						if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "holder-instance", State: proto.InstRunning, SleepLevel: level}); err != nil {
							t.Fatal(err)
						}
						out := in.Evaluate(st)
						want := held || owner == yieldpolicy.Never || newcomer != yieldpolicy.Never
						if out.GPUCandidates == nil || candidateAvailable(t, out.GPUCandidates, 0) != want || !candidateAvailable(t, out.GPUCandidates, 1) {
							t.Fatalf("choices=%+v, want sleeping card selectable=%v", out.GPUCandidates, want)
						}
					})
				}
			}
		}
	}
}

func TestGPUCandidatesDoNotReserveAFreeCard(t *testing.T) {
	st, in, dep := gpuCandidatesFixture(t, proto.InstStopped, "", yieldpolicy.Never)
	in.GPUIndexes = []int{0}
	if preview := in.Evaluate(st); !candidateAvailable(t, preview.GPUCandidates, 0) || !preview.Result.OK {
		t.Fatalf("free card refused: %+v", preview)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "new-holder", DeploymentID: dep.ID, NodeID: in.Node.ID, State: proto.InstPending, GPUIndexes: "[0]"}); err != nil {
		t.Fatal(err)
	}
	if actual := in.Evaluate(st); actual.Result.OK {
		t.Fatal("a later real plan admitted a card occupied after the preview")
	}
	in.Unloaded = true
	if unloaded := in.Evaluate(st); !candidateAvailable(t, unloaded.GPUCandidates, 0) || !unloaded.Result.OK {
		t.Fatal("structural unloaded evaluation must continue to ignore present occupancy")
	}
}

func candidateAvailable(t *testing.T, items []scheduler.GPUCandidate, index int) bool {
	t.Helper()
	for _, item := range items {
		if item.Index != index {
			continue
		}
		if item.Available == (item.Reason != nil) {
			t.Fatalf("availability/reason mismatch: %+v", item)
		}
		return item.Available
	}
	t.Fatalf("missing card %d in %+v", index, items)
	return false
}
