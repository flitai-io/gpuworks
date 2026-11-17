package planning

import (
	"errors"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func hangOn(uuids ...string) UCXNetlinkHangOf {
	hung := map[string]*proto.UCXNetlinkHang{}
	for _, uuid := range uuids {
		hung[uuid] = &proto.UCXNetlinkHang{Kernel: "5.15.0-203-generic"}
	}
	return func(uuid string) *proto.UCXNetlinkHang { return hung[uuid] }
}

func ucxFacts(networks map[uint]netplan.Network, mode string, hung ...string) UnitSuggestFacts {
	cards := map[uint]int{}
	for id := range networks {
		cards[id] = 8
	}
	facts := choiceFacts(cards, networks)
	m, _ := pdmode.Lookup(mode)
	facts.UCXNetlinkAffects = m.UCXNetlinkAffects()
	facts.UCXNetlinkHang = hangOn(hung...)
	return facts
}

var pdOneEachRoles = []PDRoleNeed{{Role: "prefill", Units: 1, GPUs: 1}, {Role: "decode", Units: 1, GPUs: 1}}

// A node whose kernel hangs UCX is left out of an automatic PD choice when the
// mode moves its KV over UCX on Ethernet, so the units go on the others; with
// no one left, the count and names say why.
func TestChoosePDUnitsLeavesOutNodesThatHangUCXOverEthernet(t *testing.T) {
	facts := ucxFacts(map[uint]netplan.Network{1: ethernet, 2: ethernet, 3: ethernet}, "vllm-nixl", "n-1")
	units, counts, ok := ChoosePDUnits(facts, pdOneEachRoles, nil, store.PlacementSpread, acceptPD)
	if !ok || slices.Contains(pdNodes(units, "prefill"), "n-1") || slices.Contains(pdNodes(units, "decode"), "n-1") {
		t.Fatalf("units = %v ok=%v, want n-1 skipped", units, ok)
	}
	if counts.UCXNetlink != 1 || !slices.Equal(counts.UCXNetlinkNodes, []string{"n-1"}) {
		t.Fatalf("counts = %+v", counts)
	}

	facts = ucxFacts(map[uint]netplan.Network{1: ethernet, 2: ethernet}, "sglang-nixl", "n-1", "n-2")
	_, counts, ok = ChoosePDUnits(facts, pdOneEachRoles, nil, store.PlacementSpread, acceptPD)
	if ok || counts.UCXNetlink != 2 || !slices.Equal(counts.UCXNetlinkNodes, []string{"n-1", "n-2"}) {
		t.Fatalf("ok=%v counts=%+v, want both nodes left out for UCX", ok, counts)
	}
	err := UnitPlacementNone(counts)
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorUnitPlacementNone {
		t.Fatalf("err = %v", err)
	}
	if p := product.Params(); p["ucx_netlink_hang"] != 2 || !slices.Equal(p["ucx_netlink_nodes"].([]string), []string{"n-1", "n-2"}) || p["nodes"] != 2 {
		t.Fatalf("params = %v", p)
	}
}

// The names stay few however many nodes are affected; the count is exact.
func TestUCXNetlinkNamesAreBounded(t *testing.T) {
	networks := map[uint]netplan.Network{}
	var hung []string
	for id := uint(1); id <= 9; id++ {
		networks[id] = ethernet
		hung = append(hung, "n-"+string(rune('0'+id)))
	}
	facts := ucxFacts(networks, "vllm-nixl", hung...)
	_, counts := SuggestUnitGroups(facts)
	if counts.UCXNetlink != 9 || len(counts.UCXNetlinkNodes) != ucxNetlinkNamed {
		t.Fatalf("counts = %+v", counts)
	}
}

// Nothing is left out where the rule does not apply: on a host RDMA network
// (a warning, not a skip), for a mode whose KV does not move over UCX, and
// when no live fact was injected. A refusal-free count says so.
func TestUCXNetlinkLeavesNodesInWhereTheRuleDoesNotApply(t *testing.T) {
	cases := []struct {
		name  string
		facts UnitSuggestFacts
	}{
		{"host RDMA", ucxFacts(map[uint]netplan.Network{1: rdma, 2: rdma}, "vllm-nixl", "n-1", "n-2")},
		{"mode not over UCX", ucxFacts(map[uint]netplan.Network{1: ethernet, 2: ethernet}, "vllm-ppu-mooncake", "n-1", "n-2")},
		{"no live fact", func() UnitSuggestFacts {
			f := ucxFacts(map[uint]netplan.Network{1: ethernet, 2: ethernet}, "vllm-nixl", "n-1", "n-2")
			f.UCXNetlinkHang = nil
			return f
		}()},
		{"not a PD deployment", func() UnitSuggestFacts {
			f := ucxFacts(map[uint]netplan.Network{1: ethernet, 2: ethernet}, "vllm-nixl", "n-1", "n-2")
			f.UCXNetlinkAffects = false
			return f
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups, counts := SuggestUnitGroups(tc.facts)
			if counts.UCXNetlink != 0 || len(groups) != 1 || len(groups[0].Nodes) != 2 {
				t.Fatalf("groups=%d counts=%+v, want both nodes offered", len(groups), counts)
			}
		})
	}
}

// A group on Ethernet loses its affected node while one on RDMA keeps its
// own: the rule follows the network the node's rank would use.
func TestUCXNetlinkFollowsThePerNodeNetwork(t *testing.T) {
	facts := ucxFacts(map[uint]netplan.Network{1: ethernet, 2: ethernet, 3: rdma, 4: rdma}, "vllm-nixl", "n-1", "n-3")
	groups, counts := SuggestUnitGroups(facts)
	got := map[uint][]string{}
	for _, g := range groups {
		got[g.Network.ID] = nodeUUIDs(candidateNodes(g.Nodes))
	}
	if counts.UCXNetlink != 1 || len(got[1]) != 1 || got[1][0] != "n-2" || len(got[2]) != 2 {
		t.Fatalf("groups = %v counts=%+v", got, counts)
	}
}

func candidateNodes(cs []PoolCandidate) []*store.Node {
	var out []*store.Node
	for _, c := range cs {
		out = append(out, c.Node)
	}
	return out
}

// A failover or re-pick takes its candidates from the same facts: a replacement
// is never an affected node.
func TestReplacementCandidatesSkipNodesThatHangUCX(t *testing.T) {
	facts := ucxFacts(map[uint]netplan.Network{1: ethernet, 2: ethernet, 3: ethernet}, "vllm-nixl", "n-2")
	facts.PerRank = 1
	got := nodeUUIDs(ReplacementCandidates(facts, ethernet.ID, NodeArch(&facts.Nodes[0]), nil, false))
	if !slices.Equal(got, []string{"n-1", "n-3"}) {
		t.Fatalf("candidates = %v, want n-2 left out", got)
	}
}

// The deployment's own placement says whether the rule applies: only a PD
// deployment in a mode whose KV moves over UCX carries it into the facts, and
// only with a live fact to read.
func TestLoadUnitSuggestFactsCarriesTheUCXNetlinkRule(t *testing.T) {
	st, err := store.Open("file:unit_suggest_ucx?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	resolve := func([]store.Node) (netplan.Resolution, error) { return netplan.Resolution{}, nil }
	placement := func(mode string) string {
		return `{"pd":{"mode":"` + mode + `","roles":{"prefill":{"units":[{}]},"decode":{"units":[{}]}}}}`
	}
	cases := []struct {
		name      string
		placement string
		hang      UCXNetlinkHangOf
		want      bool
	}{
		{"nixl PD", placement("vllm-nixl"), hangOn("x"), true},
		{"sglang nixl PD", placement("sglang-nixl"), hangOn("x"), true},
		{"mooncake PD", placement("vllm-ppu-mooncake"), hangOn("x"), false},
		{"ordinary deployment", "", hangOn("x"), false},
		{"no live fact", placement("vllm-nixl"), nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep := &store.Deployment{Placement: tc.placement}
			facts, err := LoadUnitSuggestFacts(st, resolve, dep, nodelabel.Requirements{}, nil, func(string) bool { return true }, tc.hang, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			if facts.UCXNetlinkAffects != tc.want {
				t.Fatalf("UCXNetlinkAffects = %v, want %v", facts.UCXNetlinkAffects, tc.want)
			}
		})
	}
}

// The one rule, as the placement that names its nodes asks it.
func TestUCXNetlinkBlocks(t *testing.T) {
	nixl, _ := pdmode.Lookup("vllm-nixl")
	mooncake, _ := pdmode.Lookup("vllm-ppu-mooncake")
	if !UCXNetlinkBlocks(nixl.UCXNetlinkAffects(), ethernet) {
		t.Fatal("NIXL over Ethernet must block")
	}
	if UCXNetlinkBlocks(nixl.UCXNetlinkAffects(), rdma) {
		t.Fatal("NIXL over host RDMA only warns")
	}
	if UCXNetlinkBlocks(mooncake.UCXNetlinkAffects(), ethernet) {
		t.Fatal("a mode not over UCX is never blocked")
	}
}
