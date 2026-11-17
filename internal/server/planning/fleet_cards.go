package planning

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// FleetOccupant is one instance holding a card: awake with its claim, or
// asleep with its residue (GPUOccupant, without the whole row).
type FleetOccupant struct {
	DeploymentID uint
	Asleep       bool
	Instance     string
	SleepLevel   int
	ClaimBytes   int64
	ResidueBytes int64
}

// CardState is what a card is, for anything that shows a card: the first of
// CardStates that holds. It is derived here once, so the overview's counts,
// its cards and the board cannot tell a card apart.
type CardState string

const (
	CardOffline     CardState = "offline"      // its node is not online
	CardUnhealthy   CardState = "unhealthy"    // faulty or unusable for a reason other than being held
	CardHeldOutside CardState = "held_outside" // a process outside the platform uses it, and nothing of ours serves there
	CardAwake       CardState = "awake"        // an instance claims memory on it
	CardAsleep      CardState = "asleep"       // only sleepers' residue is on it
	CardFree        CardState = "free"
)

// CardStates are the states in their precedence.
var CardStates = []CardState{CardOffline, CardUnhealthy, CardHeldOutside, CardAwake, CardAsleep, CardFree}

// State is the card's state on a node that is online or not.
func (c FleetCard) State(online bool) CardState {
	switch {
	case !online:
		return CardOffline
	case c.Unhealthy:
		return CardUnhealthy
	case c.HeldOutside && c.AwakeBytes <= 0:
		// A held card our instances serve on - partitions beside the
		// holders (ADR 0068), or work an operator allowed - is serving.
		return CardHeldOutside
	case c.AwakeBytes > 0:
		return CardAwake
	case c.SleepingBytes > 0:
		return CardAsleep
	default:
		return CardFree
	}
}

// FleetCard is one card as the ledger charges it, without the detail of who
// holds it: the figures GPUAllocations gives and which deployments hold it.
// Switching says some instance on the card is between states - starting,
// going to sleep, waking, draining.
type FleetCard struct {
	Index         int
	Name          string
	MemoryBytes   int64
	AwakeBytes    int64
	SleepingBytes int64
	HeldOutside   bool
	// Unhealthy: the card cannot take work for a reason other than being
	// held by a process outside the platform (its health, its ownership).
	Unhealthy bool
	Switching bool
	Occupants []FleetOccupant
}

// FleetDeployment is what the overview names a deployment by.
type FleetDeployment struct {
	Name string
	// YieldPolicy is the deployment's yield policy.
	YieldPolicy string
}

// FleetLedger is every node's cards, by node ID, in the order the node lists
// them, and the deployments by ID.
type FleetLedger struct {
	Cards       map[uint][]FleetCard
	Deployments map[uint]FleetDeployment
}

// FleetCards derives the ledger of all the given nodes in one pass: the live
// instances, deployments and the models of the sleepers are each read once
// for the fleet, not once per node as GPUAllocations and GPUOccupants do. The
// figures are the same ones, because the same arithmetic charges them
// (allocateInstances, occupantsOf).
func FleetCards(st *store.Store, nodes []store.Node) (*FleetLedger, error) {
	instances, err := st.LiveInstances()
	if err != nil {
		return nil, err
	}
	deployments, err := st.LedgerDeployments()
	if err != nil {
		return nil, err
	}
	charge, err := newFleetCharge(st, instances, deployments)
	if err != nil {
		return nil, err
	}
	out := &FleetLedger{Cards: make(map[uint][]FleetCard, len(nodes)), Deployments: make(map[uint]FleetDeployment, len(deployments))}
	for i := range deployments {
		d := &deployments[i]
		out.Deployments[d.ID] = FleetDeployment{Name: d.Name, YieldPolicy: d.Policy().Yield}
	}
	for i := range nodes {
		node := &nodes[i]
		out.Cards[node.ID] = nodeCards(st, node, charge.byNode[node.ID], charge.residue, charge.sleeper, charge.reported)
	}
	return out, nil
}

// FleetAllocations is GPUAllocations for every given node at once, by node ID,
// from instances and deployments the caller already holds (any state: the
// ledger itself skips the ones that hold no claim) instead of one read of
// the node's instances per node. Only the models of the sleepers are read,
// once. The figures are the per-node ledger's, charged by the same
// arithmetic (allocateInstances).
//
// The deployments are full rows: a memory-limited slice's claim reads its
// deployment's gpu_mem_util (QuickParams) from them rather than one row at a
// time.
func FleetAllocations(st *store.Store, nodes []store.Node, instances []store.Instance, deployments []store.Deployment) (map[uint]map[int]GPUAllocation, error) {
	charge, err := newFleetCharge(st, instances, deployments)
	if err != nil {
		return nil, err
	}
	utils := make(map[uint]float64, len(deployments))
	for i := range deployments {
		var quick deploy.LaunchConfig
		if json.Unmarshal([]byte(deployments[i].QuickParams), &quick) == nil {
			utils[deployments[i].ID] = quick.GPUMemUtil
		} else {
			utils[deployments[i].ID] = 0
		}
	}
	charge.residue.gpuMemUtil = func(id uint) (float64, bool) {
		util, ok := utils[id]
		return util, ok
	}
	out := make(map[uint]map[int]GPUAllocation, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		out[node.ID] = allocateInstances(st, node, charge.byNode[node.ID], ledgerView{}, charge.residue, charge.sleeper)
	}
	return out, nil
}

// fleetCharge is what charging the whole fleet's instances shares between
// nodes: the instances by node, how a sleeper is told and what its residue
// is sized from, all derived from rows read once for the fleet.
type fleetCharge struct {
	byNode   map[uint][]store.Instance
	residue  *residues
	sleeper  func(*store.Instance) bool
	reported func(*store.Instance) int
}

// newFleetCharge prepares the charge of instances against the fleet. The
// deployments need only carry their ID and model.
func newFleetCharge(st *store.Store, instances []store.Instance, deployments []store.Deployment) (*fleetCharge, error) {
	modelOf := make(map[uint]uint, len(deployments))
	for i := range deployments {
		modelOf[deployments[i].ID] = deployments[i].ModelID
	}

	byNode := make(map[uint][]store.Instance)
	// A follower of a multi-node replica sleeps with its leader, which is
	// another row of the same deployment: those deployments' rows are kept
	// together to look the leader up.
	byDeployment := map[uint][]store.Instance{}
	for i := range instances {
		in := &instances[i]
		byNode[in.NodeID] = append(byNode[in.NodeID], *in)
		if in.DistGroup != "" {
			byDeployment[in.DeploymentID] = append(byDeployment[in.DeploymentID], *in)
		}
	}
	reported := func(in *store.Instance) int {
		if !in.Headless || in.DistGroup == "" {
			return in.SleepLevel
		}
		return ReportedSleepLevelAmong(byDeployment[in.DeploymentID], in)
	}
	sleeper := func(in *store.Instance) bool {
		return in.State == proto.InstRunning && InstanceSleepIntent(in) > 0 && reported(in) > 0
	}

	// Only sleepers are charged a residue, which is sized from their model.
	wanted := map[uint]bool{}
	for i := range instances {
		if in := &instances[i]; sleeper(in) {
			wanted[modelOf[in.DeploymentID]] = true
		}
	}
	ids := make([]uint, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	models, err := st.LedgerModels(ids)
	if err != nil {
		return nil, err
	}
	byModel := make(map[uint]*store.Model, len(models))
	for i := range models {
		byModel[models[i].ID] = &models[i]
	}
	residue := newResidues(st)
	residue.known = func(deploymentID uint) float64 { return modelParams(byModel[modelOf[deploymentID]]) }
	return &fleetCharge{byNode: byNode, residue: residue, sleeper: sleeper, reported: reported}, nil
}

// NodeCards is one node's cards, as FleetCards gives them, from that node's
// instances alone: what a view of one node reads.
func NodeCards(st *store.Store, node *store.Node) ([]FleetCard, error) {
	rows, err := st.InstancesForNode(node.ID)
	if err != nil {
		return nil, err
	}
	reported := func(in *store.Instance) int { return ReportedSleepLevel(st, in) }
	sleeper := func(in *store.Instance) bool { return chargesResidue(st, in) }
	return nodeCards(st, node, rows, newResidues(st), sleeper, reported), nil
}

// nodeCards charges a node's instances (rows) to its cards - the arithmetic
// of the ledger, allocateInstances and occupantsOf - with what a card shows
// besides.
func nodeCards(st *store.Store, node *store.Node, rows []store.Instance, residue *residues, sleeper func(*store.Instance) bool, reported func(*store.Instance) int) []FleetCard {
	allocations := allocateInstances(st, node, rows, ledgerView{}, residue, sleeper)
	occupants := occupantsOf(st, node, rows, residue, sleeper)
	cards := make([]FleetCard, 0, len(node.GPUs))
	for _, gpu := range node.OwnedGPUs() {
		if gpu.Index < 0 {
			continue // a card the vendor tool does not list holds nothing schedulable
		}
		alloc := allocations[gpu.Index]
		card := FleetCard{
			Index: gpu.Index, Name: gpu.Name, MemoryBytes: gpu.MemoryTotal,
			AwakeBytes: alloc.AllocatedBytes, SleepingBytes: alloc.SleepingBytes, HeldOutside: gpu.HeldOutsidePlatform(),
			Unhealthy: !gpu.CanTakeWork() && !gpu.HeldOutsidePlatform(),
		}
		for _, o := range occupants[gpu.Index] {
			card.Occupants = append(card.Occupants, FleetOccupant{
				DeploymentID: o.Instance.DeploymentID, Asleep: o.Asleep, Instance: o.Instance.UUID,
				SleepLevel: o.Instance.SleepLevel, ClaimBytes: o.ClaimBytes, ResidueBytes: o.ResidueBytes,
			})
			if inFlux(&o.Instance, reported) {
				card.Switching = true
			}
		}
		cards = append(cards, card)
	}
	return cards
}

// inFlux reports whether an instance is between states: not running, or
// running with a sleep order its engine has not yet carried out (or a wake
// it has not finished).
func inFlux(in *store.Instance, reported func(*store.Instance) int) bool {
	if in.State != proto.InstRunning {
		return true
	}
	return (InstanceSleepIntent(in) > 0) != (reported(in) > 0)
}
