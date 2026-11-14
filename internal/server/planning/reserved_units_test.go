package planning

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// reservedNode is a node of two 48 GiB cards with a request-wakeable sharing
// deployment (idle) asleep on card 0, and a never deployment of the shape
// given to place beside it.
func reservedNode(t *testing.T, nnodes int, pd bool) (*store.Store, *store.Node, *store.Deployment, *store.Deployment) {
	t.Helper()
	st, err := store.Open("file:planning_reserved_units_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("reserved-node", store.HashToken("token"), "reserved-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{
		{NodeID: node.ID, Index: 0, UUID: "gpu-0", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 48 << 30},
		{NodeID: node.ID, Index: 1, UUID: "gpu-1", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 48 << 30},
	}
	for i := range node.GPUs {
		if err := st.DB().Create(&node.GPUs[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	model, err := st.UpsertModel(&store.Model{Name: "reserved-model", Source: "huggingface", SourceID: "org/reserved-model", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	chat := &store.Deployment{Name: "chat", ModelID: model.ID, NodeID: node.ID, ServedName: "chat", Engine: "vllm", Sleepable: true, State: "active"}
	chat.SetPolicy(yieldpolicy.Defaults(yieldpolicy.Idle))
	chat, err = st.CreateDeployment(chat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "chat-1", DeploymentID: chat.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: sleepSpec("chat-1", 0)}); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleep(chat.ID, []string{"chat-1"}, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", "chat-1").Update("sleep_level", 1).Error; err != nil {
		t.Fatal(err)
	}
	placement := ""
	if pd {
		placement = `{"pd":{"prefill":{"units":[]},"decode":{"units":[]}}}`
	}
	other, err := st.CreateDeployment(&store.Deployment{Name: "wide", ModelID: model.ID, NodeID: node.ID, ServedName: "wide", Engine: "vllm", State: "active", NNodes: nnodes, Placement: placement})
	if err != nil {
		t.Fatal(err)
	}
	return st, node, chat, other
}

// A multi-node or PD deployment shares no cards (its policy is never), so a
// request-wakeable sleeper keeps its card from it. Choosing nodes for its
// units counts that card as held, by the ledger's own reservation rule,
// rather than offering a node whose free-looking card the launch would be
// refused.
func TestUnitChoiceCountsTheCardsSleepersKeep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nnodes int
		pd     bool
	}{
		{"multi-node", 2, false},
		{"pd", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, node, _, dep := reservedNode(t, tc.nnodes, tc.pd)
			resolve := func([]store.Node) (netplan.Resolution, error) { return netplan.Resolution{}, nil }
			facts, err := LoadUnitSuggestFacts(st, resolve, dep, nodelabel.Requirements{}, nil, func(string) bool { return true }, nil, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			if !facts.Held[node.ID][0] || facts.Held[node.ID][1] {
				t.Fatalf("held = %v, want card 0 (chat keeps it for a request) and not card 1", facts.Held[node.ID])
			}
			if free := FreeHealthyGPUs(node, facts.Held[node.ID]); free != 1 {
				t.Fatalf("free cards = %d, want 1", free)
			}
		})
	}
	// A sharing deployment is not kept from the card: chat gives it up to
	// one it shares with, and a request then waits for that one.
	st, node, _, _ := reservedNode(t, 1, false)
	held, err := HeldGPUs(st, yieldpolicy.Anytime)
	if err != nil || held[node.ID][0] {
		t.Fatalf("held for a sharing launch = %v err=%v, want card 0 free", held[node.ID], err)
	}
	// An operator's sleep reserves nothing.
	if err := st.OrderDeploymentSleep(1, 1, ""); err != nil {
		t.Fatal(err)
	}
	if held, err := HeldGPUs(st, yieldpolicy.Never); err != nil || held[node.ID][0] {
		t.Fatalf("held behind an operator's sleep = %v err=%v", held[node.ID], err)
	}
}

// The ledger's own refusal for a never launch that only the sleeper's card
// would fit names the sleeper, for a rank of a multi-node replica as for any
// launch: the follower's cards are checked like the leader's.
func TestAMultiNodeRankRefusedForASleepersCardNamesIt(t *testing.T) {
	st, node, chat, _ := reservedNode(t, 2, false)
	// Card 1 is taken by an awake never instance: only card 0 could serve.
	if _, err := st.CreateInstance(&store.Instance{UUID: "busy-1", DeploymentID: 99, NodeID: node.ID, State: "running", GPUIndexes: "[1]", NNodes: 1}); err != nil {
		t.Fatal(err)
	}
	req := scheduler.Request{WeightBytes: 8 << 30, HeadCount: 8, GPUMemUtil: 0.9, AllowedTPs: []int{1}}
	view := ledgerView{reserved: reservedAgainst(st, yieldpolicy.Never)}
	if scheduler.Schedule(req, nodeSnapshot(st, node, true, "", view)).OK {
		t.Fatal("the sleeper's card was free to a never launch")
	}
	reserved := launchReserved(st, node, nodeInstances(t, st, node), req, true, "", false, view)
	if reserved == nil || !slices.Equal(reserved.GPUIndexes, []int{0}) || len(reserved.Sleepers) != 1 || reserved.Sleepers[0].DeploymentID != chat.ID {
		t.Fatalf("reserved = %+v, want card 0 kept by chat", reserved)
	}
	reason := ReservedRefusal(node, reserved)
	if reason.Code != scheduler.ReasonReservedForSleepers || jsonText(t, reason.Params) != `{"gpus":[0],"node":"reserved-node","sleepers":[{"deployment":"chat","policy":"idle"}]}` {
		t.Fatalf("refusal = %s %s", reason.Code, jsonText(t, reason.Params))
	}
}
