package controller

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ucxSpares is a PD (vllm-nixl, over the management Ethernet) deployment with
// two spare nodes of the decode node's kind; the first, the one the choice
// would take (the lower id), reports a kernel that hangs UCX when hang is set.
type ucxSpares struct {
	f           *pdFixture
	first, last *store.Node
}

func newUCXSpares(t *testing.T, name string, hang bool) ucxSpares {
	t.Helper()
	f := newPDFixture(t, name, twoPrefillOneDecode)
	s := ucxSpares{f: f, first: addPDSpareAt(t, f, name+"-n3", "10.0.0.3"), last: addPDSpareAt(t, f, name+"-n4", "10.0.0.4")}
	if hang {
		f.hub.ucxHang = map[string]*proto.UCXNetlinkHang{s.first.UUID: {Kernel: "5.15.0-203-generic"}}
	}
	return s
}

func (s ucxSpares) decodeNodes(t *testing.T) []string {
	t.Helper()
	dep, err := s.f.st.GetDeployment(s.f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	pd, err := planning.PDOf(dep.Placement)
	if err != nil || pd == nil {
		t.Fatalf("placement = %s err=%v", dep.Placement, err)
	}
	var out []string
	for _, unit := range pd.Units("decode") {
		out = append(out, unit.NodeUUID)
	}
	return out
}

// takeDecodeCard puts another deployment's instance on the decode node's card,
// so the platform must re-pick it.
func (s ucxSpares) takeDecodeCard(t *testing.T) {
	t.Helper()
	decode := s.f.nodes["n2"]
	other, err := createTestDeployment(t, s.f.st, &store.Deployment{Name: "ucx-other", ModelID: s.f.dep.ModelID, NodeID: decode.ID, ServedName: "ucx-other", Engine: "vllm", State: "active", Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.f.st.CreateInstance(&store.Instance{UUID: "ucx-other-inst", DeploymentID: other.ID, NodeID: decode.ID, State: proto.InstRunning, GPUIndexes: "[0]"}); err != nil {
		t.Fatal(err)
	}
}

// loseDecode takes the decode node offline for longer than the replacement wait.
func (s ucxSpares) loseDecode(t *testing.T) {
	t.Helper()
	decode := s.f.nodes["n2"]
	s.f.hub.online[decode.UUID] = false
	if err := s.f.st.DB().Model(&store.Node{}).Where("id = ?", decode.ID).Updates(map[string]any{"state": store.NodeStateOffline, "last_seen_at": time.Now().Add(-11 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
}

// The platform re-picking an automatically placed PD unit chooses among the
// nodes that can run the engine: without the fact the lower-id spare is taken,
// with it the other, and with only an affected spare the unit stays put.
func TestAnAutomaticPDRepickSkipsANodeThatHangsUCX(t *testing.T) {
	for _, tc := range []struct {
		name string
		hang bool
		want func(ucxSpares) string
	}{
		{"control: no node reports the hang", false, func(s ucxSpares) string { return s.first.UUID }},
		{"the lower-id spare hangs UCX", true, func(s ucxSpares) string { return s.last.UUID }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newUCXSpares(t, "ucx-repick-"+map[bool]string{true: "hang", false: "ok"}[tc.hang], tc.hang)
			if err := s.f.st.SetDeploymentUnitsAuto(s.f.dep.ID, true, ""); err != nil {
				t.Fatal(err)
			}
			s.takeDecodeCard(t)
			s.f.reconcile(t)
			s.f.reconcile(t)
			if got := s.decodeNodes(t); len(got) != 1 || got[0] != tc.want(s) {
				t.Fatalf("decode = %v, want on %s", got, tc.want(s))
			}
		})
	}

	t.Run("only an affected spare is left", func(t *testing.T) {
		f := newPDFixture(t, "ucx-repick-none", twoPrefillOneDecode)
		spare := addPDSpare(t, f, "ucx-repick-none-n3")
		f.hub.ucxHang = map[string]*proto.UCXNetlinkHang{spare.UUID: {Kernel: "5.15.0-203-generic"}}
		s := ucxSpares{f: f, first: spare}
		if err := f.st.SetDeploymentUnitsAuto(f.dep.ID, true, ""); err != nil {
			t.Fatal(err)
		}
		s.takeDecodeCard(t)
		f.reconcile(t)
		f.reconcile(t)
		if got := s.decodeNodes(t); len(got) != 1 || got[0] != f.nodes["n2"].UUID {
			t.Fatalf("decode = %v, want it left on %s: the only spare hangs UCX", got, f.nodes["n2"].UUID)
		}
		if events := eventCodes(t, f.st, f.dep.ID, EventUnitPlaced); len(events) != 0 {
			t.Fatalf("UNIT_PLACED = %v, want none", events)
		}
	})
}

// A lost PD unit's replacement is never a node that hangs UCX, and when that
// leaves none the event names the nodes left out and why.
func TestAPDFailoverSkipsANodeThatHangsUCX(t *testing.T) {
	s := newUCXSpares(t, "ucx-failover", true)
	s.f.reconcile(t)
	s.f.reconcile(t)
	s.loseDecode(t)
	s.f.reconcile(t)
	s.f.reconcile(t)
	if got := s.decodeNodes(t); len(got) != 1 || got[0] != s.last.UUID {
		t.Fatalf("decode after the loss = %v, want on %s (the lower-id spare hangs UCX)", got, s.last.UUID)
	}

	f := newPDFixture(t, "ucx-failover-none", twoPrefillOneDecode)
	spare := addPDSpare(t, f, "ucx-failover-none-n3")
	f.hub.ucxHang = map[string]*proto.UCXNetlinkHang{spare.UUID: {Kernel: "5.15.0-203-generic"}}
	g := ucxSpares{f: f, first: spare}
	f.reconcile(t)
	f.reconcile(t)
	g.loseDecode(t)
	f.reconcile(t)
	f.reconcile(t)
	if got := g.decodeNodes(t); len(got) != 1 || got[0] != f.nodes["n2"].UUID {
		t.Fatalf("decode = %v, want it left on the lost node: the only spare hangs UCX", got)
	}
	events := eventCodes(t, f.st, f.dep.ID, EventNodeReplaceNoCandidate)
	if len(events) != 1 || events[0]["ucx_netlink_hang"] != float64(1) || len(events[0]["ucx_netlink_nodes"].([]any)) != 1 || events[0]["ucx_netlink_nodes"].([]any)[0] != spare.DisplayName() {
		t.Fatalf("NODE_REPLACE_NO_CANDIDATE events = %v, want the affected spare named", events)
	}
}

// A drain starts the replacement of a unit on a draining node on a node that
// can run the engine, never one that hangs UCX.
func TestAPDDrainSkipsANodeThatHangsUCX(t *testing.T) {
	s := newUCXSpares(t, "ucx-drain", true)
	s.f.reconcile(t)
	s.f.reconcile(t)
	insts, _ := s.f.st.InstancesForDeployment(s.f.dep.ID)
	for _, inst := range insts {
		if err := s.f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning}); err != nil {
			t.Fatal(err)
		}
	}
	decode := s.f.nodes["n2"]
	if err := s.f.st.DB().Model(&store.Node{}).Where("id = ?", decode.ID).Update("scheduling", store.NodeDraining).Error; err != nil {
		t.Fatal(err)
	}
	s.f.reconcile(t)
	s.f.reconcile(t)
	insts, _ = s.f.st.InstancesForDeployment(s.f.dep.ID)
	onFirst, onLast := 0, 0
	for _, inst := range insts {
		switch inst.NodeID {
		case s.first.ID:
			onFirst++
		case s.last.ID:
			onLast++
		}
	}
	if onFirst != 0 || onLast != 1 {
		t.Fatalf("replacement instances: %d on the node that hangs UCX, %d on the other, want 0 and 1", onFirst, onLast)
	}
}
