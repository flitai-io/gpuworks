package planning

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func suggestNode(id uint, vendor, cc string, cards int) store.Node {
	node := store.Node{ID: id, UUID: fmt.Sprintf("n-%d", id), GPUVendor: vendor}
	for i := 0; i < cards; i++ {
		node.GPUs = append(node.GPUs, store.GPU{Index: i, Vendor: vendor, ComputeCapability: cc})
	}
	return node
}

// Nodes group by the compute network they are on and their GPU
// architecture; a group too small, or whose every node already leads a
// replica, is not offered; the group with the model already on most nodes
// comes first.
func TestSuggestUnitGroups(t *testing.T) {
	ib := netplan.Network{ID: 7, Name: "ib"}
	other := netplan.Network{ID: 8, Name: "other"}
	nodes := []store.Node{
		suggestNode(1, "nvidia", "8.0", 2), // ib, ampere
		suggestNode(2, "nvidia", "8.0", 2), // ib, ampere
		suggestNode(3, "nvidia", "9.0", 2), // ib, hopper: another architecture
		suggestNode(4, "nvidia", "8.0", 2), // other network, ampere
		suggestNode(5, "nvidia", "8.0", 2), // other network, ampere, model present
		suggestNode(6, "nvidia", "8.0", 1), // ib, ampere, too few cards
		suggestNode(7, "nvidia", "8.0", 2), // no compute network
	}
	resolved := netplan.Resolution{}
	for _, n := range []struct {
		id  uint
		net netplan.Network
	}{{1, ib}, {2, ib}, {3, ib}, {4, other}, {5, other}, {6, ib}} {
		resolved = append(resolved, netplan.Resolved{NodeID: n.id, Role: netplan.RoleCompute, Network: n.net, IP: "10.0.0.1", Source: netplan.SourceNode})
	}
	facts := UnitSuggestFacts{
		Nodes: nodes, Facts: NodeFacts{Networks: resolved}, Ranks: 2, PerRank: 2,
		Files: []store.ModelFile{{NodeID: 5, State: "present"}},
	}
	groups, counts := SuggestUnitGroups(facts)
	if len(groups) != 2 || groups[0].Network.ID != 8 || groups[1].Network.ID != 7 || groups[1].Arch != "nvidia/ampere" {
		t.Fatalf("groups = %+v", groups)
	}
	if counts.Busy != 1 || counts.NoNetwork != 1 || counts.Largest != 1 {
		t.Fatalf("counts = %+v, want n-6 busy, n-7 without a network, n-3 alone in its group", counts)
	}
	unit, ok := UnitFromGroup(groups[0], 2, nil, nil)
	if !ok || unit[0].UUID != "n-5" || unit[1].UUID != "n-4" {
		t.Fatalf("unit = %v, want led from n-5 where the model is", unit)
	}

	// n-5 already leads a replica: n-4 leads, n-5 follows.
	facts.Leaders = map[string]bool{"n-5": true}
	groups, _ = SuggestUnitGroups(facts)
	if unit, ok = UnitFromGroup(groups[0], 2, facts.Leaders, nil); !ok || unit[0].UUID != "n-4" || unit[1].UUID != "n-5" {
		t.Fatalf("unit = %v", unit)
	}
	// Both lead already: the group cannot carry another replica.
	facts.Leaders = map[string]bool{"n-4": true, "n-5": true}
	groups, _ = SuggestUnitGroups(facts)
	if len(groups) != 1 || groups[0].Network.ID != 7 {
		t.Fatalf("groups = %+v, want only ib's", groups)
	}
	// Skipping a refused node leaves too few.
	if _, ok := UnitFromGroup(groups[0], 2, nil, map[string]bool{"n-1": true}); ok {
		t.Fatal("a group of two with one skipped still offered a replica")
	}
	// Cards the operator already listed elsewhere are taken.
	facts.Leaders, facts.Claimed = nil, map[string]int{"n-1": 2}
	groups, counts = SuggestUnitGroups(facts)
	if counts.Busy != 2 {
		t.Fatalf("claimed cards not counted: %+v", counts)
	}
	for _, group := range groups {
		if group.Network.ID == 7 {
			t.Fatalf("ib offered with n-1's cards claimed: %+v", group)
		}
	}
}

func TestAMDUnitGroupsUseReportedArchitecture(t *testing.T) {
	for _, arch := range []string{"gfx1100", "gfx942", ""} {
		node := suggestNode(1, "amd", "", 1)
		node.GPUs[0].ArchID = arch
		if got := nodeArch(&node); got != "amd/"+arch {
			t.Errorf("nodeArch with %q = %q, want amd/%s", arch, got, arch)
		}
	}
}
