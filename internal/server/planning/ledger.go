// ledger.go is the derived allocation ledger: what a node's running instances
// have already committed, what a scheduler snapshot of that node looks like,
// and which of those instances could be slept to make room. There is no stored
// allocation table - every figure here is derived from instances, which is what
// keeps it from drifting away from what is actually running.

package planning

import (
	"encoding/json"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// EvictSuggestion names a sleepable instance whose sleep would free GPU memory.
type EvictSuggestion struct {
	InstanceUUID string  `json:"instance_uuid"`
	DeploymentID uint    `json:"deployment_id"`
	Deployment   string  `json:"deployment"`
	GPUIndexes   []int   `json:"gpu_indexes"`
	FreesGiB     float64 `json:"frees_gib"`
}

// suggestEviction finds awake, sleepable instances whose sleep would free
// enough GPU memory for the request. It returns every awake, sleepable instance
// that the current preview can use to make room, or nil if sleeping would not
// help. Instances already asleep are not candidates: their memory is already
// released down to the residue, so sleeping them again frees nothing.
func suggestEviction(st *store.Store, node *store.Node, req scheduler.Request, servingUnavailable bool, view ledgerView) []EvictSuggestion {
	insts, _ := st.InstancesForNode(node.ID)
	_, _, archFamily, _ := nodeGPUProfile(node)
	capacity := map[int]int64{}
	for _, g := range node.OwnedGPUs() {
		capacity[g.Index] = g.MemoryTotal
	}
	evicted := map[string]bool{}
	residue := newResidues(st)
	var sugg []EvictSuggestion
	for i := range insts {
		in := &insts[i]
		if in.State != proto.InstRunning && in.State != proto.InstStarting || view.excludes(in) {
			continue
		}
		dep, err := st.GetDeployment(in.DeploymentID)
		if err != nil || !dep.Sleepable || InstanceAsleep(in) {
			continue
		}
		gis := parseInts(in.GPUIndexes)
		claim := instanceClaim(st, in, archFamily)
		var frees int64
		for _, gi := range gis {
			held := claim
			if held <= 0 {
				held = capacity[gi]
			}
			// Sleeping releases the claim down to the residue the sleeper keeps.
			if released := held - residue.of(in); released > 0 {
				frees += released
			}
		}
		evicted[in.UUID] = true
		sugg = append(sugg, EvictSuggestion{
			InstanceUUID: in.UUID, DeploymentID: dep.ID, Deployment: dep.Name, GPUIndexes: gis,
			FreesGiB: float64(frees) / (1 << 30),
		})
	}
	if len(sugg) == 0 {
		return nil
	}
	// Re-run the scheduler with those instances asleep: their claims become
	// residue, exactly as the ledger will charge them once they have slept.
	view.alsoAsleep = func(in *store.Instance) bool { return evicted[in.UUID] }
	snap := nodeSnapshot(st, node, true, "", view)
	snap.ServingUnavailable = servingUnavailable
	if scheduler.Schedule(req, snap).OK {
		return sugg
	}
	return nil
}

// WakeVerdict is the sleep policy's answer to a wake, as WakeAdmission
// hands it to callers that reach the policy through planning.
type WakeVerdict = sleep.Verdict

// NeedsRoomToWake reports whether waking an instance needs its GPU memory
// back: exactly the sleepers the ledger charges residue for (chargesResidue).
// Any other instance is charged its full claim already - one only ordered to
// sleep, one whose wake is in flight, one whose sleep failed and was rolled
// back - so waking it takes nothing more, and counting its claim again would
// refuse the wake against the instance's own memory.
func NeedsRoomToWake(st *store.Store, in *store.Instance) bool {
	return chargesResidue(st, in)
}

// claimsFor is the GPU memory the sleepers on a node take back when they
// wake (wakeClaims of each).
func claimsFor(st *store.Store, node *store.Node, insts []store.Instance) []sleep.Claim {
	var claims []sleep.Claim
	residue := newResidues(st)
	for i := range insts {
		claims = append(claims, wakeClaims(st, node, &insts[i], residue)...)
	}
	return claims
}

// WakeRefusal is sleepers a wake leaves asleep: a replica's instances on the
// node whose GPUs cannot take them back, with that node's verdict.
type WakeRefusal struct {
	Node      *store.Node
	Instances []store.Instance
	Verdict   WakeVerdict
}

// AdmitWake checks a wake of the named sleepers, the awake claims, sleepers'
// residue and awake occupants of each node weighed by the sleep policy. All
// at once (partial false), each node admits its sleepers together or refuses
// them together. A partial
// wake admits, in order, each replica (a group: a multi-node replica's leader
// and followers wake together, on their own nodes) whose every member still
// fits once the replicas admitted before it are back, and refuses the rest
// whole. Each node's ledger is read once.
func AdmitWake(st *store.Store, groups [][]store.Instance, partial bool) ([]store.Instance, []WakeRefusal, error) {
	nodes := map[uint]*store.Node{}
	states := map[uint][]sleep.GPUState{}
	var order []uint
	nodeOf := func(id uint) (*store.Node, error) {
		if node := nodes[id]; node != nil {
			return node, nil
		}
		node, err := st.NodeByID(id)
		if err != nil {
			return nil, err
		}
		nodes[id], states[id] = node, wakeStates(st, node)
		order = append(order, id)
		return node, nil
	}
	if !partial {
		byNode := map[uint][]store.Instance{}
		var all []store.Instance
		for _, group := range groups {
			for _, in := range group {
				if _, err := nodeOf(in.NodeID); err != nil {
					return nil, nil, err
				}
				byNode[in.NodeID] = append(byNode[in.NodeID], in)
				all = append(all, in)
			}
		}
		var refused []WakeRefusal
		for _, id := range order {
			if verdict := sleep.AdmitWake(states[id], claimsFor(st, nodes[id], byNode[id])); !verdict.OK() {
				refused = append(refused, WakeRefusal{Node: nodes[id], Instances: byNode[id], Verdict: verdict})
			}
		}
		if len(refused) > 0 {
			return nil, refused, nil
		}
		return all, nil, nil
	}
	admittedClaims := map[uint][]sleep.Claim{}
	var admitted []store.Instance
	var refused []WakeRefusal
	for _, group := range groups {
		own := map[uint][]sleep.Claim{}
		var groupOrder []uint
		for i := range group {
			if _, err := nodeOf(group[i].NodeID); err != nil {
				return nil, nil, err
			}
			if _, seen := own[group[i].NodeID]; !seen {
				groupOrder = append(groupOrder, group[i].NodeID)
			}
			own[group[i].NodeID] = append(own[group[i].NodeID], claimsFor(st, nodes[group[i].NodeID], group[i:i+1])...)
		}
		fits := true
		for _, id := range groupOrder {
			claims := append(append([]sleep.Claim{}, admittedClaims[id]...), own[id]...)
			if verdict := sleep.AdmitWake(states[id], claims); !verdict.OK() {
				refused = append(refused, WakeRefusal{Node: nodes[id], Instances: group, Verdict: verdict})
				fits = false
				break
			}
		}
		if !fits {
			continue
		}
		for id, claims := range own {
			admittedClaims[id] = append(admittedClaims[id], claims...)
		}
		admitted = append(admitted, group...)
	}
	return admitted, refused, nil
}

// wakeStates is the node's GPUs as the sleep policy weighs a wake: awake
// claims, sleepers' residue, and who is awake on each.
func wakeStates(st *store.Store, node *store.Node) []sleep.GPUState {
	allocations := GPUAllocations(st, node)
	var states []sleep.GPUState
	for _, g := range node.OwnedGPUs() {
		a := allocations[g.Index]
		state := sleep.GPUState{Index: g.Index, MemoryTotal: g.MemoryTotal, AwakeBytes: a.AllocatedBytes, SleepingBytes: a.SleepingBytes, HeldOutside: g.HeldOutsidePlatform()}
		for i := range a.Instances {
			if chargesResidue(st, &a.Instances[i]) {
				continue
			}
			name := a.Instances[i].UUID
			if dep, err := st.GetDeployment(a.Instances[i].DeploymentID); err == nil {
				name = dep.Name
			}
			state.Occupants = append(state.Occupants, name)
		}
		states = append(states, state)
	}
	return states
}

// wakeClaims is the GPU memory a sleeper takes back when it wakes; none for
// an instance that does not need room (NeedsRoomToWake).
func wakeClaims(st *store.Store, node *store.Node, in *store.Instance, residue *residues) []sleep.Claim {
	if !NeedsRoomToWake(st, in) {
		return nil
	}
	_, _, archFamily, _ := nodeGPUProfile(node)
	capacity := map[int]int64{}
	for _, g := range node.OwnedGPUs() {
		capacity[g.Index] = g.MemoryTotal
	}
	claim := instanceClaim(st, in, archFamily)
	var claims []sleep.Claim
	for _, gi := range parseInts(in.GPUIndexes) {
		bytes := claim
		if bytes <= 0 {
			// A whole-GPU engine reclaims its utilization share of the card.
			bytes = int64(float64(capacity[gi]) * instanceGPUMemUtil(st, in, ledgerFactsOf(in).Engine))
		}
		claims = append(claims, sleep.Claim{GPUIndex: gi, Bytes: bytes, ResidueBytes: residue.of(in)})
	}
	return claims
}

// instanceClaim returns an instance's per-GPU memory claim: a dGPU memory-limited
// instance claims its limit plus what its partition backend lets the engine hold
// outside it (estimate.PartitionClaimBytes); a whole-GPU instance returns 0 (the
// caller treats that as claiming the full GPU).
//
// The claim comes from the same place admission takes it, so a shared GPU is
// not oversubscribed by an overhead the ledger forgot to charge. A backend
// charged the engine's reserve is charged as if the instance captures CUDA
// graphs: the stored spec carries no enforce_eager - only the resolved argv
// does - and a ledger that over-charges costs a co-tenant where one that
// under-charges oversubscribes the card.
//
// The utilization comes from the instance's own deployment configuration when it
// states one. A deployment that stated none - or has since been deleted - is
// charged the engine's own default, which is exactly what its engine is running
// at, read from the dictionary rather than written down here.
func instanceClaim(st *store.Store, inst *store.Instance, archFamily string) int64 {
	spec := ledgerFactsOf(inst)
	if spec.MemoryClaimBytes > 0 {
		// Admitted onto a GPU shared by memory: the claim is the decision.
		return spec.MemoryClaimBytes
	}
	if spec.VRAMLimitBytes <= 0 {
		return 0
	}
	return estimate.PartitionClaimBytes(spec.VRAMLimitBytes, spec.PartitionBackend, func() int64 {
		return estimate.ReserveBytes(engineReserveBase(spec.Engine), archFamily, true, instanceGPUMemUtil(st, inst, spec.Engine))
	})
}

// ledgerFacts are the fields of an instance's desired state (a
// proto.InstanceDesiredState) the ledger charges by. A row without a claim
// reads 0 and is charged its whole GPU, which never oversubscribes it.
type ledgerFacts struct {
	Engine           string `json:"engine"`
	VRAMLimitBytes   int64  `json:"vram_limit_bytes"`
	PartitionBackend string `json:"partition_backend"`
	MemoryClaimBytes int64  `json:"memory_claim_bytes"`
}

func ledgerFactsOf(inst *store.Instance) ledgerFacts {
	var facts ledgerFacts
	if inst.DesiredStateJSON == "" || json.Unmarshal([]byte(inst.DesiredStateJSON), &facts) != nil {
		return ledgerFacts{}
	}
	return facts
}

func instanceGPUMemUtil(st *store.Store, inst *store.Instance, engine string) float64 {
	if st != nil {
		if dep, err := st.GetDeployment(inst.DeploymentID); err == nil {
			var quick deploy.LaunchConfig
			if json.Unmarshal([]byte(dep.QuickParams), &quick) == nil && quick.GPUMemUtil > 0 {
				return quick.GPUMemUtil
			}
		}
	}
	return deploy.DefaultGPUMemUtil(engine)
}

// NodeSnapshot builds the scheduler snapshot: per-GPU free/busy from the derived
// running-instance ledger (whole-GPU model pre-dGPU), topology, and filters.
// Whether the node may serve is a network decision the caller adds
// (Input.ServingUnavailable).
func NodeSnapshot(st *store.Store, node *store.Node, dtypeOK bool, dtypeReason string) scheduler.NodeSnapshot {
	return nodeSnapshot(st, node, dtypeOK, dtypeReason, ledgerView{})
}

// ledgerView adjusts how the ledger reads a node's instances for one preview.
// The zero value reads them as they are.
type ledgerView struct {
	// alsoAsleep treats further instances as asleep, which is how the
	// eviction suggestion previews the ledger after the instances it proposes
	// have slept.
	alsoAsleep func(*store.Instance) bool
	// excludeDeployment leaves that deployment's instances out entirely: an
	// edit of a deployment is previewed as the launch that replaces them, so
	// the memory they hold now is not counted against it (Input.ExcludeDeploymentID).
	excludeDeployment uint
	// reserved says a sleeper keeps its whole claim rather than its residue:
	// it shares its cards by yield policy, but not with the deployment
	// being placed (reservedAgainst).
	reserved func(*store.Instance) bool
}

func (v ledgerView) excludes(in *store.Instance) bool {
	return v.excludeDeployment != 0 && in.DeploymentID == v.excludeDeployment
}

// nodeSnapshot is NodeSnapshot read through view.
func nodeSnapshot(st *store.Store, node *store.Node, dtypeOK bool, dtypeReason string, view ledgerView) scheduler.NodeSnapshot {
	// Derived per-GPU allocation ledger (K8s model, no stored allocated table):
	// a whole-GPU instance claims the full memory; a dGPU memory-limited instance
	// claims its limit + context reserve, so several can share one GPU; a
	// sleeping instance of either kind keeps only its residue.
	allocations := gpuAllocations(st, node, view)
	partitionReports := partitioncap.Decode(node)
	var gpus []scheduler.GPU
	for _, g := range node.OwnedGPUs() {
		a := allocations[g.Index]
		var readyBackends []string
		for _, capability := range partitionReports.ForGPU(&g) {
			if capability.Status == partitioncap.StatusReady {
				readyBackends = append(readyBackends, capability.Backend)
			}
		}
		gpus = append(gpus, scheduler.GPU{
			WholeGPUAllocated: a.WholeGPUAllocated,

			Index: g.Index, MemoryTotal: g.MemoryTotal, AllocatedBytes: a.AllocatedBytes, SleepingBytes: a.SleepingBytes,
			NUMANode: g.NUMANode, Schedulable: g.CanTakeWork(), HeldOutsidePlatform: g.HeldOutsidePlatform(),
			Degraded:          accelerator.HealthDegraded(g.HealthStatus),
			PartitionBackends: readyBackends,
			SharedByMemory:    accelerator.SharedByMemory(node.VendorOf(g)),
		})
	}
	vendor, _, _, _ := nodeGPUProfile(node)
	return scheduler.NodeSnapshot{
		UUID:     node.UUID,
		Vendor:   vendor,
		GPUs:     gpus,
		Topology: st.NodeTopology(node.ID),
		// A node whose runtime cannot hand a container its accelerators is
		// not refused here: the platform cordons it (Node.RuntimeGPUBlocked),
		// because the operator must be able to override that, and a gate
		// in the snapshot they could not.
		DtypeSupported:        dtypeOK,
		DtypeReason:           dtypeReason,
		DiskAvailBytes:        modelDiskAvailable(node),
		RuntimeDiskAvailBytes: runtimeDiskAvailable(node),
	}
}

// modelDiskAvailable reads the agent's statfs snapshot for the filesystem that
// actually holds model weights. A missing or invalid snapshot is deliberately
// reported as unknown (zero), so scheduling does not mistake the node root
// disk total for free model capacity.
func modelDiskAvailable(node *store.Node) int64 {
	if node == nil || strings.TrimSpace(node.PathUsage) == "" {
		return 0
	}
	var paths []proto.PathUsage
	if err := json.Unmarshal([]byte(node.PathUsage), &paths); err != nil {
		return 0
	}
	for _, path := range paths {
		if path.Kind == "models" && path.AvailableBytes > 0 {
			return path.AvailableBytes
		}
	}
	return 0
}

func runtimeDiskAvailable(node *store.Node) int64 {
	if node == nil || strings.TrimSpace(node.PathUsage) == "" {
		return 0
	}
	kind := strings.ToLower(strings.TrimSpace(node.RuntimeDriver))
	// The kind a driver reports its image store under is its own name.
	if kind != "udocker" && kind != "docker" && kind != proto.RuntimeDriverHost {
		return 0
	}
	var paths []proto.PathUsage
	if err := json.Unmarshal([]byte(node.PathUsage), &paths); err != nil {
		return 0
	}
	for _, path := range paths {
		if path.Kind == kind && path.AvailableBytes > 0 {
			return path.AvailableBytes
		}
	}
	return 0
}

// GPUAllocation is the derived scheduler ledger for one physical GPU.
// AllocatedBytes is what awake instances claim; SleepingBytes is the residue
// sleeping instances keep. Both kinds are listed in Instances.
type GPUAllocation struct {
	WholeGPUAllocated bool

	AllocatedBytes int64
	SleepingBytes  int64
	Instances      []store.Instance
}

// GPUAllocations derives per-GPU claims from non-terminal instances. It is the
// shared source for scheduling and the read-only GPU inventory API.
func GPUAllocations(st *store.Store, node *store.Node) map[int]GPUAllocation {
	return gpuAllocations(st, node, ledgerView{})
}

func gpuAllocations(st *store.Store, node *store.Node, view ledgerView) map[int]GPUAllocation {
	if st == nil || node == nil {
		return map[int]GPUAllocation{}
	}
	instances, _ := st.InstancesForNode(node.ID)
	return allocateInstances(st, node, instances, view, newResidues(st), func(in *store.Instance) bool { return chargesResidue(st, in) })
}

// allocateInstances charges a node's instances to its GPUs: the arithmetic
// of the ledger, apart from where the instances come from (one node's read,
// or the fleet's single pass, FleetCards) and how a confirmed sleeper is told
// (sleeper: chargesResidue, or the same test over rows already in hand).
func allocateInstances(st *store.Store, node *store.Node, instances []store.Instance, view ledgerView, residue *residues, sleeper func(*store.Instance) bool) map[int]GPUAllocation {
	result := map[int]GPUAllocation{}
	if node == nil {
		return result
	}
	capacity := map[int]int64{}
	for _, gpu := range node.OwnedGPUs() {
		capacity[gpu.Index] = gpu.MemoryTotal
	}
	// The node is homogeneous, so one architecture describes every claim on it.
	_, _, archFamily, _ := nodeGPUProfile(node)
	for _, instance := range instances {
		if !holdsClaim(&instance) || view.excludes(&instance) {
			continue
		}
		// A wake order is conservative until the agent reports awake: the
		// instance's intent is already zero while the observed level may still
		// be L1/L2, so the full claim remains reserved.
		asleep := (view.alsoAsleep != nil && view.alsoAsleep(&instance)) ||
			(sleeper(&instance) && (view.reserved == nil || !view.reserved(&instance)))
		claim := instanceClaim(st, &instance, archFamily)
		for _, gpuIndex := range parseInts(instance.GPUIndexes) {
			allocation := result[gpuIndex]
			switch {
			case asleep:
				// A sleeper has given the card up down to its residue, whether it
				// held the whole GPU or a memory-limited slice of it.
				allocation.SleepingBytes += residue.of(&instance)
			case claim > 0:
				allocation.AllocatedBytes += claim
			default:
				allocation.AllocatedBytes = capacity[gpuIndex]
				allocation.WholeGPUAllocated = true
			}
			if allocation.AllocatedBytes > capacity[gpuIndex] {
				allocation.AllocatedBytes = capacity[gpuIndex]
			}
			if allocation.AllocatedBytes+allocation.SleepingBytes > capacity[gpuIndex] {
				allocation.SleepingBytes = capacity[gpuIndex] - allocation.AllocatedBytes
			}
			allocation.Instances = append(allocation.Instances, instance)
			result[gpuIndex] = allocation
		}
	}
	return result
}

// chargesResidue is the ledger's single test for a confirmed sleeper: ordered
// to sleep and at a sleep level (ReportedSleepLevel, so a multi-node
// replica's followers give their cards up with their leader). An ordered
// sleep that has not taken effect, a wake in flight, and an uncertain failed
// transition all retain the full GPU claim.
func chargesResidue(st *store.Store, in *store.Instance) bool {
	return in != nil && in.State == proto.InstRunning && InstanceSleepIntent(in) > 0 && ReportedSleepLevel(st, in) > 0
}
