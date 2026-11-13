package deployment

import (
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The states a deployment shows while the gateway refuses every request
// because the replicas it needs sleep. They are derived, never stored: an
// instance's reported SleepLevel and the sleep intent of its desired state are
// the only record of sleep, and planning.InstanceAsleep is how the gateway
// judges a request, so what a row shows cannot disagree with what the gateway
// answers.
const (
	// StateSleeping is every replica the gateway needs asleep while
	// at least one engine still reports the level it had before the order: the
	// order is accepted and the gateway already refuses every request, and the
	// engines are still swapping memory out (a level-1 order takes seconds).
	StateSleeping = "sleeping"
	// StateAsleep is every replica the gateway needs asleep with the
	// level its engine reports.
	StateAsleep = "asleep"
)

// replicaStanding is how the gateway judges one replica.
type replicaStanding int

const (
	// replicaNotServing is a replica the gateway routes no request to for a
	// reason other than sleep: no instance, a stopped or failed one, a node
	// that is gone, a multi-node generation missing a rank.
	replicaNotServing replicaStanding = iota
	// replicaAwake is a replica the gateway serves requests from.
	replicaAwake
	// replicaAsleep is a replica the gateway would route to but refuses for
	// sleep: planning.InstanceAsleep, the judgement the router itself makes.
	replicaAsleep
)

// standingOf is the one predicate the ready and the asleep counts come from,
// and it is the gateway's: a replica is reached through its leader, on an
// online node, and a multi-node generation serves only as a whole. A follower
// sleeps with its generation's engine (one process, one sleep order) and is
// never judged on its own.
//
// Sleep is judged first, as the router judges it (route_select records an
// asleep leader before it looks at its node or its other ranks), so a replica
// whose own ranks or node are down is still asleep when the leader asks to
// sleep: the gateway refuses it for sleep either way.
func standingOf(rows Rows, leader *store.Instance, followersComplete bool, incomplete map[string]bool) replicaStanding {
	if leader == nil || leader.State != proto.InstRunning {
		return replicaNotServing
	}
	if planning.InstanceAsleep(leader) {
		return replicaAsleep
	}
	if !InstanceNodeOnline(rows, leader.NodeID) || incomplete[leader.UUID] || !followersComplete {
		return replicaNotServing
	}
	return replicaAwake
}

// ReplicaCounts is what one role's replicas are, as the gateway reads them.
type ReplicaCounts struct {
	Awake  int
	Asleep int
	// Settled is the asleep ones whose engine reports the level it was ordered
	// to; Level is the deepest reported level.
	Settled int
	Level   int
	// Held is the asleep ones an operator's or a schedule's sleep holds
	// (store.Instance.SleepHeld, the leader's): a request does not wake them.
	Held int
}

// Facts is how the gateway reads a deployment's replicas now.
type Facts struct {
	// Ready counts the replicas the gateway would serve a request from: this
	// is ready_replicas.
	Ready int
	// AsleepReplicas counts the replicas the gateway refuses because they
	// sleep, in every role: this is asleep_replicas, and with Ready it adds up
	// to the replicas the gateway reaches at all.
	AsleepReplicas int
	// Settled counts the asleep replicas whose engine reports the level it was
	// ordered to. The rest have been ordered, are already refused, and are
	// still swapping memory out.
	Settled int
	// Level is the deepest sleep level an engine reports.
	Level int
	// HeldReplicas counts the asleep replicas a held sleep keeps asleep: a
	// request wakes none of them.
	HeldReplicas int
	// Roles is the same walk per role, for a PD response's role counts.
	Roles map[string]ReplicaCounts
	// refused is set when the gateway would answer no request at all: some
	// role it needs has no awake replica.
	refused bool
	// sleepRefusal is set when every role that refuses is refused by sleep
	// alone: it has sleeping replicas and no awake one. Only refused and
	// sleepRefusal together make a deployment asleep, which is why a replica
	// down for another reason never reads as asleep.
	sleepRefusal bool
}

// Asleep reports the gateway refusing every request because the replicas it
// needs sleep: the condition the sleeping and asleep states describe. A
// deployment with no serving replica is never asleep; nothing is left to wake.
func (f Facts) Asleep() bool { return f.refused && f.sleepRefusal && f.AsleepReplicas > 0 }

// SleepHold says how the asleep replicas are held (dto.SleepHold*): every
// one by an operator's or a schedule's sleep, some, or none; empty with none
// asleep.
func (f Facts) SleepHold() string {
	switch {
	case f.AsleepReplicas == 0:
		return ""
	case f.HeldReplicas >= f.AsleepReplicas:
		return dto.SleepHoldAll
	case f.HeldReplicas > 0:
		return dto.SleepHoldSome
	}
	return dto.SleepHoldNone
}

// State is the state a fully asleep deployment shows: asleep once every asleep
// engine reports its level, sleeping while any order is still being carried
// out. It is only meaningful when Asleep says so; callers ask for it that way.
func (f Facts) State() string {
	if f.Settled >= f.AsleepReplicas {
		return StateAsleep
	}
	return StateSleeping
}

// FactsOf reads the facts out of the replica match the response
// shows: one walk, so every number a response carries describes the same
// replicas, judged the way the router judges them.
func FactsOf(rows Rows, view ReplicaView, insts []store.Instance) Facts {
	incomplete := planning.GroupsIncomplete(OnlineInstances(rows, insts))
	facts := Facts{Roles: map[string]ReplicaCounts{}}
	refused, sleepCause := false, true
	for _, role := range view.Roles() {
		counts := replicaCountsOf(rows, view, role, incomplete)
		facts.Roles[role] = counts
		facts.Ready += counts.Awake
		facts.AsleepReplicas += counts.Asleep
		facts.Settled += counts.Settled
		facts.HeldReplicas += counts.Held
		facts.Level = max(facts.Level, counts.Level)
		if counts.Awake > 0 || !view.RoleRequired(role, counts) {
			continue
		}
		// The gateway has nothing awake to answer this role: an outage, and an
		// outage caused by sleep only when the role is asleep rather than down.
		refused = true
		if counts.Asleep == 0 {
			sleepCause = false
		}
	}
	facts.refused = refused
	facts.sleepRefusal = refused && sleepCause
	return facts
}

// replicaCountsOf counts the replicas filling one role, over the units the
// placement has and, for an ordinary deployment, the replica that left it.
func replicaCountsOf(rows Rows, view ReplicaView, role string, incomplete map[string]bool) ReplicaCounts {
	var out ReplicaCounts
	judge := func(leader *store.Instance, followersComplete bool) {
		switch standingOf(rows, leader, followersComplete, incomplete) {
		case replicaAwake:
			out.Awake++
		case replicaAsleep:
			out.Asleep++
			out.Level = max(out.Level, leader.SleepLevel)
			if leader.SleepHeld {
				out.Held++
			}
			// Settled means the engine has reached the level it was ordered
			// to, which is not the same as it reporting some level: a change
			// between two levels goes through awake (the engine only sleeps
			// from awake), so a level-2 order on a level-1 engine reports 1
			// for the whole wake-then-offload sequence. Comparing against the
			// order is what makes that transition read "sleeping" instead of
			// byte-identically to the settled state before the click.
			if leader.SleepLevel > 0 && leader.SleepLevel >= leader.SleepIntent() {
				out.Settled++
			}
		}
	}
	for _, slot := range view.Match.Slots {
		if slot.Unit.Role != role {
			continue
		}
		judge(slot.Inst, followersRunning(rows, slot))
	}
	// An ordinary deployment's excess replica still serves: the controller
	// drains it on a later pass and, until it is draining, the gateway routes
	// to it as to any other replica. So a scale-down that leaves the awake
	// replica excess must not read asleep. A PD deployment's excess replica
	// does not: pdRoute.withoutUnroutable drops it from the routing set.
	if role == "" && view.PD == nil {
		for i := range view.Match.Excess {
			judge(&view.Match.Excess[i], true)
		}
	}
	return out
}

// roles are the roles the deployment's replicas are counted by: one empty role
// for an ordinary deployment, the mode's roles for a PD one (the placement's,
// in the mode's order, when the mode cannot be read).
func (v ReplicaView) Roles() []string {
	if v.PD == nil {
		return []string{""}
	}
	if len(v.Mode.Roles) > 0 {
		return v.Mode.Roles
	}
	out := make([]string, 0, len(v.PD.Roles))
	for role := range v.PD.Roles {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

// roleRequired reports whether the gateway needs an awake replica of this role
// to answer a request, which is what makes its sleeping replicas an outage
// instead of a reduction. It mirrors the gateway's own rule: pdRoute.wantsPrefill
// serves without prefill instances when the mode may run empty and every one of
// them sleeps, which is an order carried out rather than a degradation.
func (v ReplicaView) RoleRequired(role string, counts ReplicaCounts) bool {
	if v.PD == nil {
		return role == ""
	}
	if role != pdmode.RolePrefill {
		return true
	}
	if len(v.PD.Units(pdmode.RolePrefill)) == 0 {
		return false
	}
	return !(counts.Awake == 0 && counts.Asleep > 0 && v.Mode.MayBeEmpty(pdmode.RolePrefill))
}

// FactsFor reads the facts for callers that hold no replica view.
func FactsFor(rows Rows, d *store.Deployment, insts []store.Instance) Facts {
	view, err := NewReplicaView(rows, d, insts)
	if err != nil {
		return Facts{}
	}
	return FactsOf(rows, view, insts)
}

// followersRunning reports every follower rank of the replica running on an
// online node: a generation serves as a whole.
func followersRunning(rows Rows, slot planning.UnitSlot) bool {
	for _, follower := range slot.Followers {
		if follower == nil || follower.State != proto.InstRunning || !InstanceNodeOnline(rows, follower.NodeID) {
			return false
		}
	}
	return true
}

// ReplicaView is a deployment's replicas as the gateway and the controller
// match them, over the deployment's own nodes: every reader of a
// deployment's state reads replica membership from here.
type ReplicaView struct {
	Match planning.UnitMatch
	// PD and Mode are the deployment's roles: nil PD for an ordinary
	// deployment, and Mode as the placement names it (its zero value when the
	// gateway does not know the name, which reads as no role being optional).
	PD   *planning.PDPlacement
	Mode pdmode.Mode
}

// NewReplicaView matches the deployment's instances to its placement's units.
func NewReplicaView(rows Rows, d *store.Deployment, insts []store.Instance) (ReplicaView, error) {
	var view ReplicaView
	uuids := map[uint]string{}
	for _, id := range d.NodeIDList() {
		if n := rows.Node(id); n != nil {
			uuids[n.ID] = n.UUID
		}
	}
	pd, err := planning.PDOf(d.Placement)
	if err != nil {
		return view, err
	}
	view.PD = pd
	if pd != nil {
		view.Mode, _ = pdmode.Lookup(pd.Mode)
	}
	view.Match = planning.MatchUnits(planning.UnitsOfDeployment(d, pd, uuids), insts, uuids, d.PlacementStrategy)
	return view, nil
}

// StateDeleting is a deployment whose delete is in flight.
const StateDeleting = "deleting"

// States are every state a deployment shows (EffectiveState): the runtime
// states, the delete in flight, and the two sleep states.
var States = []string{"pending", "pulling", "starting", "running", "offline", "failed", "stopped", StateDeleting, StateSleeping, StateAsleep}

// EffectiveState is the one state a deployment shows, wherever it is shown:
// a delete in flight outranks whatever the runtime reports, and a deployment
// the gateway refuses every request of because the replicas it needs sleep
// is asleep (or sleeping, while the order is still being carried out) rather
// than the running state its engines keep reporting.
//
// A partly slept deployment keeps the runtime state: its awake replicas serve,
// and asleep_replicas says how many do not, so the row shows both instead of
// passing itself off as entirely well or entirely asleep.
func EffectiveState(d *store.Deployment, runtimeState string, facts Facts) string {
	if d.State == store.DeployDeleting {
		return StateDeleting
	}
	if runtimeState == "running" && facts.Asleep() {
		return facts.State()
	}
	return runtimeState
}

// State is EffectiveState for a caller that holds neither the runtime state
// nor the facts.
func State(rows Rows, d *store.Deployment, insts []store.Instance) string {
	runtime := RuntimeState(rows, d, insts, RecordedModelProgress(rows, d))
	return EffectiveState(d, runtime, FactsFor(rows, d, insts))
}
