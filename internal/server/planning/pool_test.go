package planning

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func candidate(id uint, replicas, free int, model ModelLocality, sticky bool) PoolCandidate {
	return PoolCandidate{Node: &store.Node{ID: id}, Replicas: replicas, FreeGPUs: free, Model: model, Sticky: sticky}
}

func rankedIDs(ranked []PoolCandidate) []uint {
	out := make([]uint, 0, len(ranked))
	for _, c := range ranked {
		out = append(out, c.Node.ID)
	}
	return out
}

// Spread puts the next replica where the deployment has the fewest; among
// equals, the node that already has the model, then one it ran on, then the
// most room. A node that looks full goes last, not out.
func TestRankPoolSpread(t *testing.T) {
	got := rankedIDs(RankPool([]PoolCandidate{
		candidate(1, 2, 8, ModelPresent, true),  // most replicas already
		candidate(2, 0, 1, ModelAbsent, false),  // no replica, little room
		candidate(3, 0, 4, ModelAbsent, false),  // no replica, more room
		candidate(4, 0, 2, ModelPresent, false), // no replica, has the model
		candidate(5, 0, 2, ModelFetching, true), // fetching beats absent
		candidate(6, 0, 0, ModelPresent, true),  // looks full
	}, store.PlacementSpread, 1))
	if want := []uint{4, 5, 3, 2, 1, 6}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spread order = %v, want %v", got, want)
	}
}

// Binpack fills the node with the least room that still fits.
func TestRankPoolBinpack(t *testing.T) {
	got := rankedIDs(RankPool([]PoolCandidate{
		candidate(1, 0, 8, ModelPresent, false),
		candidate(2, 3, 2, ModelAbsent, false),
		candidate(3, 1, 2, ModelPresent, false),
		candidate(4, 0, 1, ModelPresent, false), // too little room for 2
	}, store.PlacementBinpack, 2))
	if want := []uint{3, 2, 1, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("binpack order = %v, want %v", got, want)
	}
	// A memory-limited replica needs no whole card: nothing is "full".
	got = rankedIDs(RankPool([]PoolCandidate{candidate(1, 0, 3, ModelAbsent, false), candidate(2, 0, 0, ModelAbsent, false)}, store.PlacementBinpack, 0))
	if want := []uint{2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shared binpack order = %v, want %v", got, want)
	}
}

func TestModelLocalityOf(t *testing.T) {
	for state, want := range map[string]ModelLocality{
		proto.MFPresent: ModelPresent, proto.MFRegistered: ModelPresent,
		proto.MFPending: ModelFetching, proto.MFDownloading: ModelFetching, proto.MFSyncing: ModelFetching,
		proto.MFFailed: ModelAbsent, proto.MFAbsent: ModelAbsent, proto.MFInsufficientDisk: ModelAbsent,
	} {
		if got := ModelLocalityOf(&store.ModelFile{State: state}); got != want {
			t.Errorf("%s = %d, want %d", state, got, want)
		}
	}
	if ModelLocalityOf(nil) != ModelAbsent {
		t.Error("no file row")
	}
	// Card 3 is taken, card 5 cannot serve: one free healthy card left. The
	// level says so - a card with none stated has had nothing found about
	// it, which is the opposite of what the zero value used to mean.
	node := &store.Node{GPUs: []store.GPU{{Index: 0}, {Index: 3}, {Index: 5, HealthStatus: accelerator.HealthError}}}
	if got := FreeHealthyGPUs(node, map[int]bool{3: true}); got != 1 {
		t.Errorf("free healthy GPUs = %d, want 1", got)
	}
}

// A scale-down drops the replicas a pool unit no longer claims. Under spread
// the most crowded node gives one up, under binpack the emptiest; a replica
// that is not running yet goes first under either.
func TestMatchUnitsScaleDownKeepsTheStrategyShape(t *testing.T) {
	units := DeploymentUnits(&store.Deployment{Replicas: 2}, nil, nil)
	nodes := map[uint]string{1: "a", 2: "b"}
	insts := []store.Instance{
		{ID: 1, UUID: "a1", NodeID: 1, State: proto.InstRunning},
		{ID: 2, UUID: "a2", NodeID: 1, State: proto.InstRunning},
		{ID: 3, UUID: "b1", NodeID: 2, State: proto.InstRunning},
	}
	excess := func(strategy store.PlacementStrategy, insts []store.Instance) []string {
		var out []string
		for _, inst := range MatchUnits(units, insts, nodes, strategy).Excess {
			out = append(out, inst.UUID)
		}
		return out
	}
	if got := excess(store.PlacementSpread, insts); !reflect.DeepEqual(got, []string{"a2"}) {
		t.Fatalf("spread drops %v, want the newer of the crowded node's two", got)
	}
	if got := excess(store.PlacementBinpack, insts); !reflect.DeepEqual(got, []string{"b1"}) {
		t.Fatalf("binpack drops %v, want the emptiest node's", got)
	}
	starting := append([]store.Instance(nil), insts...)
	starting[0].State = proto.InstStarting
	if got := excess(store.PlacementSpread, starting); !reflect.DeepEqual(got, []string{"a1"}) {
		t.Fatalf("spread drops %v, want the one not running yet", got)
	}
}

// A replica's failed attempts from before pool units - recorded under a key
// that named the deployment's node list - still count against the replica,
// so an upgrade neither forgets nor discards them.
func TestMatchUnitsCountsAttemptsRecordedBeforePoolUnits(t *testing.T) {
	units := DeploymentUnits(&store.Deployment{Replicas: 1}, nil, nil)
	match := MatchUnits(units, []store.Instance{
		{ID: 1, UUID: "old", NodeID: 1, State: proto.InstFailed, ScheduleExpl: `{"unit":{"key":"|node-a[]"}}`},
		{ID: 2, UUID: "pd", NodeID: 1, State: proto.InstFailed, ScheduleExpl: `{"unit":{"key":"prefill|node-a[]"}}`},
	}, map[uint]string{1: "node-a"}, "")
	if got := match.Failed[units[0].Key()]; len(got) != 1 || got[0].UUID != "old" {
		t.Fatalf("failed = %v, want the old attempt counted", match.Failed)
	}
	if len(match.Stale) != 1 || match.Stale[0].UUID != "pd" {
		t.Fatalf("stale = %v, want the other shape's attempt", match.Stale)
	}
}

// Nodes of one hardware shape answer an unloaded plan alike, so the shape
// leaves out everything that is not the hardware: names, ids, health, card
// order.
func TestHardwareShape(t *testing.T) {
	card := func(index int, name string, mem int64, healthy bool) store.GPU {
		level := accelerator.HealthOK
		if !healthy {
			level = accelerator.HealthError
		}
		return store.GPU{Index: index, Name: name, MemoryTotal: mem, ComputeCapability: "8.0", ArchID: "sm_80", HealthStatus: level}
	}
	a := &store.Node{ID: 1, Name: "a", GPUVendor: "NVIDIA", Arch: "amd64", GPUs: []store.GPU{card(0, "NVIDIA A100", 80, true), card(1, "A100", 40, true)}}
	b := &store.Node{ID: 2, Name: "b", GPUVendor: "nvidia", Arch: "amd64", GPUs: []store.GPU{card(0, "A100", 40, false), card(1, "A100", 80, true)}}
	if HardwareShape(a, true) != HardwareShape(b, true) {
		t.Fatalf("same hardware, different shapes:\n%s\n%s", HardwareShape(a, true), HardwareShape(b, true))
	}
	for name, other := range map[string]*store.Node{
		"another card":   {GPUVendor: "nvidia", Arch: "amd64", GPUs: []store.GPU{card(0, "A100", 80, true), card(1, "H100", 40, true)}},
		"more memory":    {GPUVendor: "nvidia", Arch: "amd64", GPUs: []store.GPU{card(0, "A100", 80, true), card(1, "A100", 80, true)}},
		"one card fewer": {GPUVendor: "nvidia", Arch: "amd64", GPUs: []store.GPU{card(0, "A100", 80, true)}},
		"another CPU":    {GPUVendor: "nvidia", Arch: "arm64", GPUs: []store.GPU{card(0, "A100", 80, true), card(1, "A100", 40, true)}},
	} {
		if HardwareShape(other, true) == HardwareShape(a, true) {
			t.Errorf("%s: same shape", name)
		}
	}
	if HardwareShape(a, true) == HardwareShape(a, false) {
		t.Error("a node that may not serve has the shape of one that may")
	}
	mixed := &store.Node{GPUVendor: "nvidia", Arch: "amd64", GPUs: []store.GPU{
		{Index: 0, Name: "H100", MemoryTotal: 80, ComputeCapability: "9.0", ArchID: "sm_90", Vendor: "nvidia"},
		{Index: 1, Name: "P800 PCIe", MemoryTotal: 80, ComputeCapability: "9.0", ArchID: "kl3", Vendor: "kunlunxin"},
	}}
	uniform := &store.Node{GPUVendor: "nvidia", Arch: "amd64", GPUs: []store.GPU{
		{Index: 0, Name: "H100", MemoryTotal: 80, ComputeCapability: "9.0", ArchID: "sm_90", Vendor: "nvidia"},
		{Index: 1, Name: "H100", MemoryTotal: 80, ComputeCapability: "9.0", ArchID: "sm_90", Vendor: "nvidia"},
	}}
	if HardwareShape(mixed, true) == HardwareShape(uniform, true) {
		t.Fatal("per-card vendor differences must distinguish hardware shapes")
	}
}

func TestHardwareShapeIncludesPerCardVendorAndRuntimeFacts(t *testing.T) {
	nvidia := func(os, runtime, cardVendor string) *store.Node {
		return &store.Node{GPUVendor: "", OS: os, RuntimeDriver: runtime, Arch: "amd64", GPUs: []store.GPU{{
			Index: 0, Name: "GPU Model", Vendor: cardVendor, MemoryTotal: 80, ComputeCapability: "9.0", ArchID: "sm_90",
		}}}
	}
	if HardwareShape(nvidia("linux", "docker", "nvidia"), true) == HardwareShape(nvidia("windows", "nerdctl", "ascend"), true) {
		t.Fatal("vendor and runtime differences must distinguish all target shapes")
	}
	amd := func(os, runtime string) *store.Node {
		return &store.Node{OS: os, RuntimeDriver: runtime, Arch: "amd64", GPUs: []store.GPU{{
			Index: 0, Name: "MI300X", Vendor: "amd", MemoryTotal: 192, ArchID: "gfx942",
		}}}
	}
	if HardwareShape(amd("linux", "docker"), true) == HardwareShape(amd("linux", "nerdctl"), true) {
		t.Fatal("AMD runtime differences must remain distinct target shapes")
	}
}

// Every node of the cluster gets exactly one status, the first check it
// fails, so the counts add up to the node count; unmet nodes are counted by
// the requirement they fail; candidates carry their rank.
func TestBuildPoolClassifiesEveryNodeOnce(t *testing.T) {
	gpus := func(n int) []store.GPU {
		var out []store.GPU
		for i := 0; i < n; i++ {
			out = append(out, store.GPU{Index: i, Name: "A100"})
		}
		return out
	}
	nodes := []store.Node{
		{ID: 1, UUID: "room", GPUVendor: "nvidia", Labels: `{"pool":"a"}`, GPUs: gpus(2)},
		{ID: 2, UUID: "busy", GPUVendor: "nvidia", Labels: `{"pool":"a"}`, GPUs: gpus(2)},
		{ID: 3, UUID: "ascend", GPUVendor: "ascend", Labels: `{"pool":"a"}`, GPUs: gpus(2)},
		{ID: 4, UUID: "unlabelled", GPUVendor: "nvidia", GPUs: gpus(2)},
		{ID: 5, UUID: "offline", GPUVendor: "nvidia", Labels: `{"pool":"a"}`, GPUs: gpus(2)},
		// Offline and unmet: the requirement is the first check.
		{ID: 6, UUID: "both", GPUVendor: "ascend", GPUs: gpus(2)},
	}
	pool := BuildPool(PoolFacts{
		Nodes:        nodes,
		Held:         map[uint]map[int]bool{2: {0: true, 1: true}},
		Instances:    []store.Instance{{NodeID: 2, State: proto.InstRunning}},
		Online:       func(uuid string) bool { return uuid != "offline" && uuid != "both" },
		Requirements: map[string][]string{"gpuworks.ai/vendor": {"nvidia"}, "pool": {"a"}},
		Strategy:     store.PlacementSpread, Need: 1,
	})
	status := map[string]PoolStatus{}
	total := 0
	for _, entry := range pool.Entries {
		status[entry.Node.UUID] = entry.Status
		if (entry.Rank > 0) != (entry.Status == PoolRoom || entry.Status == PoolBusy) {
			t.Errorf("%s: rank %d with status %s", entry.Node.UUID, entry.Rank, entry.Status)
		}
	}
	for _, count := range pool.Counts {
		total += count
	}
	want := map[string]PoolStatus{"room": PoolRoom, "busy": PoolBusy, "ascend": PoolUnmet, "unlabelled": PoolUnmet, "offline": PoolOffline, "both": PoolUnmet}
	if !reflect.DeepEqual(status, want) || total != len(nodes) || len(pool.Entries) != len(nodes) {
		t.Fatalf("statuses = %v (total %d), want %v", status, total, want)
	}
	if !reflect.DeepEqual(pool.UnmetByKey, map[string]int{"gpuworks.ai/vendor": 2, "pool": 1}) {
		t.Fatalf("unmet by key = %v", pool.UnmetByKey)
	}
	if got := rankedIDs(pool.Ranked); !reflect.DeepEqual(got, []uint{1, 2}) || pool.Entries[0].Node.UUID != "room" || pool.Entries[0].Rank != 1 {
		t.Fatalf("ranked = %v, first entry %+v", got, pool.Entries[0])
	}
	if pool.UnmetDetail == "" {
		t.Fatal("no detail for the unmet nodes")
	}
}

func TestBuildPoolCountsOnlySelectableFreeGPUs(t *testing.T) {
	node := store.Node{ID: 1, UUID: "pinned", GPUs: []store.GPU{{Index: 3}, {Index: 7}, {Index: 9, HealthStatus: accelerator.HealthError}}}
	for _, tc := range []struct {
		name       string
		selected   []int
		held       map[int]bool
		need, free int
		status     PoolStatus
	}{
		{name: "occupied pin ignores spare card", selected: []int{3}, held: map[int]bool{3: true}, need: 1, free: 0, status: PoolBusy},
		{name: "free sparse index", selected: []int{7}, held: map[int]bool{3: true}, need: 1, free: 1, status: PoolRoom},
		{name: "missing selected card", selected: []int{4}, need: 1, free: 0, status: PoolBusy},
		{name: "unhealthy selected card", selected: []int{9}, need: 1, free: 0, status: PoolBusy},
		{name: "partial parallel group", selected: []int{3, 7}, held: map[int]bool{3: true}, need: 2, free: 1, status: PoolBusy},
		{name: "unpinned replica can use spare card", held: map[int]bool{3: true}, need: 1, free: 1, status: PoolRoom},
		{name: "released sleeping card", selected: []int{3}, need: 1, free: 1, status: PoolRoom},
		{name: "shared card requires full memory trial", selected: []int{3}, held: map[int]bool{3: true}, need: 0, free: 0, status: PoolRoom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := BuildPool(PoolFacts{Nodes: []store.Node{node}, Held: map[uint]map[int]bool{1: tc.held}, Need: tc.need, Placement: Placement{GPUIndexes: tc.selected}})
			if len(pool.Ranked) != 1 || len(pool.Entries) != 1 || pool.Entries[0].FreeGPUs != tc.free || pool.Entries[0].Status != tc.status || pool.Counts[tc.status] != 1 {
				t.Fatalf("pool=%+v entries=%+v", pool, pool.Entries)
			}
		})
	}
}

func TestBuildPoolUsesResolvedSleepTargetAdmission(t *testing.T) {
	kunlun := store.GPU{Index: 0, Name: "P800 PCIe", Vendor: "kunlunxin", ArchID: "kl3"}
	nvidia := store.GPU{Index: 1, Name: "H100", Vendor: "nvidia", ArchID: "sm_90"}
	nodes := []store.Node{
		{ID: 1, UUID: "kunlun", GPUVendor: "kunlunxin", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{kunlun}},
		{ID: 2, UUID: "mixed", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{kunlun, nvidia}},
		{ID: 3, UUID: "amd", GPUVendor: "amd", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUs: []store.GPU{{Index: 0, Name: "MI300X", Vendor: "amd", ArchID: "gfx942"}}},
	}
	admission := TargetRefusalFor(&store.Deployment{Engine: "vllm", Sleepable: true}, nil, func() (*store.Model, error) { return &store.Model{}, nil }, nil)
	pool := BuildPool(PoolFacts{Nodes: nodes, TargetRefusal: admission.Refusal})
	if err := admission.Err(); err != nil {
		t.Fatal(err)
	}
	if len(pool.Ranked) != 2 || pool.UnmetByKey[PoolUnmetAccelerator] != 1 {
		t.Fatalf("pool did not keep compatible cards: %+v", pool)
	}
	for _, entry := range pool.Entries {
		if entry.Node.UUID == "kunlun" && (entry.Status != PoolUnmet || entry.Reason == nil || entry.Reason.Code != "sleep_unsupported_on_node") {
			t.Fatalf("Kunlun pool refusal = %+v", entry)
		}
	}
}
