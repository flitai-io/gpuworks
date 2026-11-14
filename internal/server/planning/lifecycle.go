package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// DeploymentMatch is how a deployment's instances fill its replicas, read
// from the store: its units over the nodes it lists, matched to insts.
func DeploymentMatch(st *store.Store, dep *store.Deployment, insts []store.Instance) (UnitMatch, error) {
	uuids := map[uint]string{}
	for _, id := range dep.NodeIDList() {
		if node, err := st.NodeByID(id); err == nil {
			uuids[node.ID] = node.UUID
		}
	}
	pd, err := PDOf(dep.Placement)
	if err != nil {
		return UnitMatch{}, err
	}
	return MatchUnits(UnitsOfDeployment(dep, pd, uuids), insts, uuids, dep.PlacementStrategy), nil
}

// WakeGroups splits named instances by the replica they fill, in the order
// they are named; an instance no replica claims is a group of its own. A
// partial wake admits or refuses a group whole, so a multi-node replica comes
// back with all its ranks.
func WakeGroups(match UnitMatch, named []store.Instance) [][]store.Instance {
	var groups [][]store.Instance
	at := map[*UnitSlot]int{}
	for _, in := range named {
		slot := match.SlotOf(in.UUID)
		if slot == nil {
			groups = append(groups, []store.Instance{in})
			continue
		}
		if index, seen := at[slot]; seen {
			groups[index] = append(groups[index], in)
			continue
		}
		at[slot] = len(groups)
		groups = append(groups, []store.Instance{in})
	}
	return groups
}

// Sleepers are a deployment's instances that hold a sleep intent: the ones a
// deployment-wide wake names.
func Sleepers(insts []store.Instance) []store.Instance {
	var out []store.Instance
	for i := range insts {
		if store.HoldsSleepIntent(&insts[i]) {
			out = append(out, insts[i])
		}
	}
	return out
}

// OrderWake wakes the named instances of a deployment once their GPUs can
// take them back (AdmitWake), grouped by replica. With partial, it wakes the
// groups that fit and returns the rest as refusals; without, one refusal
// keeps them all asleep. whole says the order names every sleeper of the
// deployment: when all of them wake, the deployment-wide sleep order is
// withdrawn with them; otherwise only the woken instances' intents change
// (a wake of some instances withdraws the deployment-wide order too, as
// store.OrderInstancesSleep does). The caller holds the server's order lock
// (lifecycle.Orders.Lock).
func OrderWake(st *store.Store, dep *store.Deployment, match UnitMatch, named []store.Instance, partial, whole bool) ([]store.Instance, []WakeRefusal, error) {
	woken, refused, err := AdmitWake(st, WakeGroups(match, named), partial)
	if err != nil {
		return nil, nil, err
	}
	if len(refused) > 0 && (!partial || len(woken) == 0) {
		return nil, refused, nil
	}
	if whole && len(refused) == 0 {
		err = st.SetDeploymentSleep(dep.ID, 0)
	} else if len(woken) > 0 {
		err = st.OrderInstancesSleep(dep.ID, instanceUUIDs(woken), 0, "")
	}
	if err != nil {
		return nil, nil, err
	}
	return woken, refused, nil
}

func instanceUUIDs(insts []store.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, in := range insts {
		out = append(out, in.UUID)
	}
	return out
}

// YieldTarget is an awake instance on a GPU a refused wake needs, and the
// deployment it serves: put to sleep, it gives the card back.
type YieldTarget struct {
	Deployment *store.Deployment
	Instance   store.Instance
	// Members are the instances of the replica it leads, leader first
	// (ReplicaMembers): a sleep order for it is written on every one.
	Members []store.Instance
}

// YieldBlocker is an occupant a yielding wake cannot put to sleep, and why:
// its deployment cannot sleep, it would leave a PD role empty, or the GPU is
// held by a process outside the platform (Deployment nil).
type YieldBlocker struct {
	Deployment string
	Code       string
}

// YieldTargets are the awake instances occupying the GPUs the refused wakes
// need, each replica once (a multi-node replica sleeps whole through its
// leader), and the occupants that cannot be put to sleep. A wake that yields
// proceeds only when there are no blockers: putting some occupants to sleep
// and not others would take service away without making room. An occupant
// already ordered to sleep is no target: the wake waits for that sleep, and
// it is not ordered or counted as a yield again.
func YieldTargets(st *store.Store, refusals []WakeRefusal) ([]YieldTarget, []YieldBlocker, error) {
	var targets []YieldTarget
	var blockers []YieldBlocker
	seen := map[string]bool{}
	waking := map[string]bool{}
	for _, r := range refusals {
		for _, in := range r.Instances {
			waking[in.UUID] = true
		}
	}
	for _, r := range refusals {
		allocations := GPUAllocations(st, r.Node)
		for _, short := range r.Verdict.Shortfalls {
			if short.HeldOutside {
				blockers = append(blockers, YieldBlocker{Code: "held_outside"})
				continue
			}
			for _, in := range allocations[short.GPUIndex].Instances {
				if waking[in.UUID] || seen[in.UUID] || InstanceSleepIntent(&in) > 0 || !proto.InstanceFillsReplica(in.State) {
					continue
				}
				seen[in.UUID] = true
				dep, err := st.GetDeployment(in.DeploymentID)
				if err != nil {
					return nil, nil, err
				}
				insts, err := st.InstancesForDeployment(dep.ID)
				if err != nil {
					return nil, nil, err
				}
				match, err := DeploymentMatch(st, dep, insts)
				if err != nil {
					return nil, nil, err
				}
				slot := match.SlotOf(in.UUID)
				if slot != nil {
					for _, member := range ReplicaMembers(slot) {
						seen[member.UUID] = true
					}
				}
				inst := in
				if slot != nil {
					inst = *slot.Inst
				}
				if refusal := InstanceSleepRefusal(dep, match, &inst, true); refusal != nil {
					blockers = append(blockers, YieldBlocker{Deployment: dep.Name, Code: refusal.Code})
					continue
				}
				// A target has a slot: an instance without one is refused
				// its sleep (InstanceSleepRefusal) and is a blocker above.
				targets = append(targets, YieldTarget{Deployment: dep, Instance: inst, Members: ReplicaMembers(slot)})
			}
		}
	}
	return targets, blockers, nil
}
