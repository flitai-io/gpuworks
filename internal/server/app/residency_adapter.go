package app

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/lifecycle"
	"gpuworks.ai/gpuworks/internal/server/placewait"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// A yield sleeps at the level the holder's policy prefers: auto is L1 when
// the host can hold the weights, else L2; l1 and l2 are what they say.
// Measured on an L20 (docs/explanation/service-levels.md): the first L1 sleep
// of an engine pins about twice its weights in host memory and keeps them
// pinned even awake, and the agent does that first sleep when the engine
// starts (its prewarm). So an L1 yield takes no host memory that is not
// already held, switches in about 2.5 s, and needs no model file read back,
// where an L2 wake reads the weights again: seconds from page cache, but
// tens of seconds to minutes from disk once pinned memory has squeezed the
// page cache.

// residencyAdapter backs the residency arbiter with the store, the ledger and
// the gateway's held requests and traffic.
type residencyAdapter struct {
	store *store.Store
	proxy *proxy.Proxy
	acct  *proxy.Accountant
	host  planning.HostMemory
	// waits are the deployments whose replica waits to be placed until
	// others give up cards (the controller's); nil when there are none to
	// read.
	waits *placewait.Registry
	// orders is the server's sleep and wake order lock (the controller's).
	orders *lifecycle.Orders
	now    func() time.Time
}

// Waiters are the deployments with requests held for their wake whose sleep a
// request may end.
func (a *residencyAdapter) Waiters() ([]automation.Waiter, error) {
	var out []automation.Waiter
	for _, id := range a.proxy.HeldDeployments() {
		n, since := a.proxy.HeldRequests(id)
		if n == 0 {
			continue
		}
		dep, ok := a.wakeableByRequest(id)
		if !ok {
			continue
		}
		out = append(out, automation.Waiter{DeploymentID: id, Policy: dep.Policy(), Since: since})
	}
	for _, w := range a.placeWaits() {
		dep, err := a.store.GetDeployment(w.DeploymentID)
		if err != nil || dep.State != store.DeployActive {
			continue
		}
		out = append(out, automation.Waiter{DeploymentID: w.DeploymentID, Policy: dep.Policy(), Since: w.Since, Place: true})
	}
	return out, nil
}

func (a *residencyAdapter) placeWaits() []placewait.Waiter {
	if a.waits == nil {
		return nil
	}
	return a.waits.Waiting(a.now())
}

// Settling says the deployment's replica waits to be placed, or an instance
// of it is still starting: asked of the deployment a holder gave its cards
// to, which is not serving yet.
func (a *residencyAdapter) Settling(id uint) bool {
	if a.waits != nil {
		if _, ok := a.waits.Get(id, a.now()); ok {
			return true
		}
	}
	insts, err := a.store.InstancesForDeployment(id)
	if err != nil {
		return false
	}
	return anyStarting(insts)
}

// anyStarting says one of the instances has not started serving yet.
func anyStarting(insts []store.Instance) bool {
	for i := range insts {
		if planning.InstanceStarting(&insts[i]) {
			return true
		}
	}
	return false
}

// placeBlocking is what keeps a replica waiting to be placed: the awake
// holders it waits for, still awake with no sleep ordered, by deployment
// (planning.HoldsCardsAwake: one still starting holds its cards too, and
// reads starting). Sharing deployments are single-node, so an instance is a
// whole replica.
func (a *residencyAdapter) placeBlocking(w placewait.Waiter) (automation.Blocking, error) {
	var out automation.Blocking
	at := map[uint]int{}
	for _, uuid := range w.Holders {
		in, err := a.store.InstanceByUUID(uuid)
		if err != nil || !planning.HoldsCardsAwake(in) || planning.InstanceSleepIntent(in) > 0 {
			continue
		}
		index, ok := at[in.DeploymentID]
		if !ok {
			dep, err := a.store.GetDeployment(in.DeploymentID)
			if err != nil {
				continue
			}
			at[in.DeploymentID] = len(out.Holders)
			out.Holders = append(out.Holders, automation.Holder{DeploymentID: dep.ID, Name: dep.Name, Policy: dep.Policy()})
			index = len(out.Holders) - 1
		}
		out.Holders[index].Instances = append(out.Holders[index].Instances, in.UUID)
		out.Holders[index].Starting = out.Holders[index].Starting || planning.InstanceStarting(in)
	}
	return out, nil
}

// wakeableByRequest is the deployment when it is active and has a sleeper
// (ordered to sleep) a request wakes (store.Deployment.WakesOnRequest): an
// operator's or a schedule's sleepers are not woken for a request or a
// resume, while the platform's sleepers of the same deployment are.
func (a *residencyAdapter) wakeableByRequest(id uint) (*store.Deployment, bool) {
	dep, err := a.store.GetDeployment(id)
	if err != nil || dep.State != store.DeployActive || !dep.Sleepable {
		return nil, false
	}
	insts, err := a.store.InstancesForDeployment(id)
	if err != nil {
		return nil, false
	}
	for i := range insts {
		if store.HoldsSleepIntent(&insts[i]) && planning.InstanceSleepIntent(&insts[i]) > 0 && dep.WakesOnRequest(&insts[i]) {
			return dep, true
		}
	}
	return nil, false
}

// Wakeable says the deployment is active and has a sleeper a request wakes
// (wakeableByRequest), with its policy.
func (a *residencyAdapter) Wakeable(id uint) (yieldpolicy.Policy, bool) {
	dep, ok := a.wakeableByRequest(id)
	if !ok {
		return yieldpolicy.Policy{}, false
	}
	return dep.Policy(), true
}

// Blocking is what keeps the first of the deployment's waiting replicas from
// waking: the awake replicas on the cards it needs, one per deployment, or an
// occupant nothing can put to sleep. One replica is enough to serve the
// requests that wait; the rest wake when there is room.
func (a *residencyAdapter) Blocking(id uint) (automation.Blocking, error) {
	if a.waits != nil {
		if w, ok := a.waits.Get(id, a.now()); ok {
			return a.placeBlocking(w)
		}
	}
	dep, err := a.store.GetDeployment(id)
	if err != nil {
		return automation.Blocking{}, err
	}
	insts, err := a.store.InstancesForDeployment(id)
	if err != nil {
		return automation.Blocking{}, err
	}
	var waiting []store.Instance
	for _, in := range insts {
		if in.WakeWaitingSince != nil && store.HoldsSleepIntent(&in) && planning.InstanceSleepIntent(&in) > 0 {
			waiting = append(waiting, in)
		}
	}
	if len(waiting) == 0 {
		return automation.Blocking{}, nil
	}
	match, err := planning.DeploymentMatch(a.store, dep, insts)
	if err != nil {
		return automation.Blocking{}, err
	}
	_, refused, err := planning.AdmitWake(a.store, planning.WakeGroups(match, waiting), true)
	if err != nil || len(refused) == 0 {
		return automation.Blocking{}, err
	}
	targets, blockers, err := planning.YieldTargets(a.store, refused[:1])
	if err != nil {
		return automation.Blocking{}, err
	}
	out := automation.Blocking{Blocked: len(blockers) > 0}
	at := map[uint]int{}
	for _, target := range targets {
		index, ok := at[target.Deployment.ID]
		if !ok {
			at[target.Deployment.ID] = len(out.Holders)
			out.Holders = append(out.Holders, automation.Holder{
				DeploymentID: target.Deployment.ID, Name: target.Deployment.Name, Policy: target.Deployment.Policy(),
			})
			index = len(out.Holders) - 1
		}
		for _, member := range target.Members {
			out.Holders[index].Instances = append(out.Holders[index].Instances, member.UUID)
		}
		// Whether the holder is starting is its replica's own: another
		// replica of its deployment starting does not hold this one back.
		out.Holders[index].Starting = out.Holders[index].Starting || anyStarting(target.Members)
	}
	return out, nil
}

// Activity is the deployment's traffic through the gateway.
func (a *residencyAdapter) Activity(id uint) automation.Activity {
	held, _ := a.proxy.HeldRequests(id)
	last, _ := a.acct.LastRequestAt(id)
	return automation.Activity{InFlight: a.acct.InFlight(id), Held: held, LastRequest: last}
}

// Yield puts the holder's replica to sleep at the level its policy prefers
// (yieldpolicy.Policy.SleepLevel, resolved by planning.OrderSleep: auto is
// L1 unless the host cannot hold the weights), a sleep the platform chose
// (not held: a request for it waits and wakes it again). A holder that
// prefers L1 is refused when the host cannot hold the weights, as an
// operator's L1 sleep is.
func (a *residencyAdapter) Yield(id uint, instances []string, abort bool) (int, error) {
	defer a.orders.Lock()()
	mode := proto.SleepModeWait
	if abort {
		mode = proto.SleepModeAbort
	}
	dep, err := a.store.GetDeployment(id)
	if err != nil {
		return 0, err
	}
	level, refusal, err := planning.OrderSleep(a.store, dep, planning.SleepOrder{
		Pref: dep.Policy().SleepLevel, Mode: mode, Instances: append([]string{}, instances...),
	}, a.host)
	if refusal != nil {
		return 0, refusal.Err()
	}
	return level, err
}

// Drain stops (on) or resumes the gateway routing new requests to the
// instances; the deployment's other replicas take them.
func (a *residencyAdapter) Drain(id uint, instances []string, on bool) {
	a.proxy.SetDraining(id, instances, on)
}

// InstancesInFlight is the requests the gateway serves on the instances.
func (a *residencyAdapter) InstancesInFlight(instances []string) int {
	return a.proxy.InstancesInFlight(instances)
}

// wakeHeldBy is the gateway's held requests by deployment, for the scheduling
// overview's contention.
func wakeHeldBy(px *proxy.Proxy) func() map[uint]int {
	return func() map[uint]int {
		out := map[uint]int{}
		for _, id := range px.HeldDeployments() {
			if n, _ := px.HeldRequests(id); n > 0 {
				out[id] = n
			}
		}
		return out
	}
}
