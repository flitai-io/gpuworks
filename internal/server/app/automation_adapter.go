package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/lifecycle"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/placewait"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// automationAdapter backs the scheduling automation with the store, the
// ledger, the gateway's request clock and the host memory the agents report.
// Every order takes the lifecycle order lock, as an operator's does.
type automationAdapter struct {
	store *store.Store
	acct  *proxy.Accountant
	// host reads a node's host memory as its agent recently reported it
	// (planning.RecentHostMemory, the reader an operator's sleep uses); nil knows
	// nothing.
	host  planning.HostMemory
	scale func(deploymentID uint, role string, replicas int) error
	// waits are the replicas waiting to be placed (the controller's): a wake
	// does not take the cards one that keeps them against it waits for.
	waits *placewait.Registry
	// orders is the server's sleep and wake order lock (the controller's).
	orders *lifecycle.Orders
	log    *slog.Logger
	now    func() time.Time
}

// runAutomation carries out schedules, auto sleep, waiting wakes and the
// residency arbiter until ctx ends. A change to an instance or a deployment
// nudges it, since a waiting wake may fit once another instance sleeps or
// goes; a request the gateway holds for a sleeping deployment asks it to wake
// that one.
func runAutomation(ctx context.Context, st *store.Store, px *proxy.Proxy, acct *proxy.Accountant, host planning.HostMemory, scale func(deploymentID uint, role string, replicas int) error, waits *placewait.Registry, orders *lifecycle.Orders, log *slog.Logger) {
	adapter := &automationAdapter{store: st, acct: acct, host: host, scale: scale, waits: waits, orders: orders, log: log, now: time.Now}
	evaluator := automation.New(adapter, adapter, log)
	if px != nil {
		residency := &residencyAdapter{store: st, proxy: px, acct: acct, host: host, waits: waits, orders: orders, now: time.Now}
		evaluator.WithResidency(residency, residency)
		if waits != nil {
			waits.OnWait(evaluator.Demand)
		}
		px.SetWakeDemand(evaluator.Demand)
	}
	if st.Bus != nil {
		sub := st.Bus.Subscribe(64, "instance", "deployment")
		go func() {
			defer sub.Close()
			for {
				select {
				case <-ctx.Done():
					return
				case <-sub.C():
					evaluator.Nudge()
				}
			}
		}()
	}
	evaluator.Run(ctx)
}

// Schedules are the enabled schedules of active deployments. One whose time
// zone the server does not know is skipped and logged rather than run in the
// wrong zone.
func (a *automationAdapter) Schedules() ([]automation.Schedule, error) {
	rows, err := a.store.EnabledLifecycleSchedules()
	if err != nil {
		return nil, err
	}
	out := make([]automation.Schedule, 0, len(rows))
	for _, row := range rows {
		s, err := deployment.AutomationSchedule(row)
		if err != nil {
			a.log.Warn("automation: schedule time zone unknown; skipped", "schedule", row.ID, "timezone", row.Timezone)
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// WaitingWakes groups the instances whose wake waits by deployment, each
// group's wait starting at its oldest.
func (a *automationAdapter) WaitingWakes() ([]automation.WaitingWake, error) {
	insts, err := a.store.InstancesWakeWaiting()
	if err != nil {
		return nil, err
	}
	var out []automation.WaitingWake
	at := map[uint]int{}
	for _, in := range insts {
		index, ok := at[in.DeploymentID]
		if !ok {
			at[in.DeploymentID] = len(out)
			out = append(out, automation.WaitingWake{DeploymentID: in.DeploymentID, Since: *in.WakeWaitingSince})
			index = len(out) - 1
		}
		out[index].Instances = append(out[index].Instances, in.UUID)
		if in.WakeWaitingSince.Before(out[index].Since) {
			out[index].Since = *in.WakeWaitingSince
		}
	}
	return out, nil
}

// Sleep orders a deployment to sleep as an operator's deployment-wide order
// does (planning.OrderSleep): refused when the deployment cannot sleep, or at
// an explicit L1 when a host cannot hold the weights; at the auto level
// (automation.LevelAuto) it sleeps at L1, or at L2 where the host cannot hold
// the weights. It returns the level ordered.
func (a *automationAdapter) Sleep(deploymentID uint, level int, held bool) (int, error) {
	defer a.orders.Lock()()
	dep, err := a.store.GetDeployment(deploymentID)
	if err != nil {
		return 0, err
	}
	ordered, refusal, err := planning.OrderSleep(a.store, dep, planning.SleepOrder{
		Pref: planning.SleepPreference(level), Mode: proto.SleepModeWait, Held: held,
	}, a.host)
	if refusal != nil {
		return 0, refusal.Err()
	}
	return ordered, err
}

// Wake wakes the deployment's sleepers that fit and leaves the rest waiting
// for room. A held wake (a schedule's) names every sleeper; one a request or
// a resume drives names only those a request wakes
// (store.Deployment.WakesOnRequest), so the sleepers an operator or a
// schedule holds stay asleep and held.
func (a *automationAdapter) Wake(deploymentID uint, held bool) (automation.WakeResult, error) {
	defer a.orders.Lock()()
	dep, err := a.store.GetDeployment(deploymentID)
	if err != nil {
		return automation.WakeResult{}, err
	}
	if code := planning.SleepEligibility(dep); code != "" {
		return automation.WakeResult{}, messages.NewProductError(code, "the deployment cannot sleep or wake", map[string]any{"engine": dep.Engine})
	}
	insts, err := a.store.InstancesForDeployment(dep.ID)
	if err != nil {
		return automation.WakeResult{}, err
	}
	match, err := planning.DeploymentMatch(a.store, dep, insts)
	if err != nil {
		return automation.WakeResult{}, err
	}
	named := planning.Sleepers(insts)
	whole := true
	if !held {
		named, whole = wakesOnRequest(dep, named)
	}
	sleepers, kept := a.notForPlacement(dep, named)
	var woken []store.Instance
	var refused []planning.WakeRefusal
	if len(sleepers) > 0 {
		woken, refused, err = planning.OrderWake(a.store, dep, match, sleepers, true, whole && len(kept) == 0)
		if err != nil {
			return automation.WakeResult{}, err
		}
	}
	result := automation.WakeResult{Woken: uuidList(woken), Waiting: uuidList(kept)}
	for _, r := range refused {
		result.Waiting = append(result.Waiting, uuidList(r.Instances)...)
	}
	if err := a.store.MarkWakeWaiting(dep.ID, result.Waiting, a.now()); err != nil {
		return result, err
	}
	return result, nil
}

// wakesOnRequest are the sleepers a request wakes, and whether they are all
// of them: a held sleeper stays asleep, so a wake that leaves one is not the
// deployment-wide wake.
func wakesOnRequest(dep *store.Deployment, sleepers []store.Instance) ([]store.Instance, bool) {
	out := make([]store.Instance, 0, len(sleepers))
	for i := range sleepers {
		if dep.WakesOnRequest(&sleepers[i]) {
			out = append(out, sleepers[i])
		}
	}
	return out, len(out) == len(sleepers)
}

// WakeWaiting wakes the named waiting instances whose GPUs can take them back
// now. An instance no longer waiting asleep (removed, rebuilt, woken or slept
// again by an operator) drops out.
func (a *automationAdapter) WakeWaiting(deploymentID uint, uuids []string) ([]string, error) {
	defer a.orders.Lock()()
	dep, err := a.store.GetDeployment(deploymentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	insts, err := a.store.InstancesForDeployment(dep.ID)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, uuid := range uuids {
		want[uuid] = true
	}
	var gone []string
	var waiting []store.Instance
	for _, in := range insts {
		if !want[in.UUID] {
			continue
		}
		if !store.HoldsSleepIntent(&in) || planning.InstanceSleepIntent(&in) == 0 {
			gone = append(gone, in.UUID)
			continue
		}
		waiting = append(waiting, in)
	}
	if len(gone) > 0 {
		if err := a.store.EndWakeWaiting(dep.ID, gone); err != nil {
			return nil, err
		}
	}
	if len(waiting) == 0 {
		return nil, nil
	}
	match, err := planning.DeploymentMatch(a.store, dep, insts)
	if err != nil {
		return nil, err
	}
	waiting, _ = a.notForPlacement(dep, waiting)
	if len(waiting) == 0 {
		return nil, nil
	}
	woken, _, err := planning.OrderWake(a.store, dep, match, waiting, true, false)
	if err != nil {
		return nil, err
	}
	return uuidList(woken), nil
}

// notForPlacement splits the sleepers of dep into those a wake may take
// cards for and those it leaves asleep: the ones on a card a replica waits
// to be placed on, of a deployment that keeps them against dep
// (yieldpolicy.KeepsCardsAgainst: the cards were given up for it). The
// placement waits name the cards they wait for, so the cards stay the
// waiter's after the holders' sleeps are ordered, until it is placed.
func (a *automationAdapter) notForPlacement(dep *store.Deployment, sleepers []store.Instance) (free, held []store.Instance) {
	if a.waits == nil {
		return sleepers, nil
	}
	waits := a.waits.Waiting(a.now())
	if len(waits) == 0 {
		return sleepers, nil
	}
	taken := map[planning.Card]bool{}
	policy := dep.Policy()
	for _, w := range waits {
		if w.DeploymentID == dep.ID {
			continue
		}
		waiter, err := a.store.GetDeployment(w.DeploymentID)
		if err != nil || !yieldpolicy.KeepsCardsAgainst(waiter.Policy(), policy) {
			continue
		}
		for _, index := range w.Cards.GPUIndexes {
			taken[planning.Card{NodeID: w.Cards.NodeID, Index: index}] = true
		}
	}
	if len(taken) == 0 {
		return sleepers, nil
	}
	return planning.SplitByCards(sleepers, taken)
}

func (a *automationAdapter) EndWaiting(deploymentID uint, uuids []string) error {
	return a.store.EndWakeWaiting(deploymentID, uuids)
}

// ScaleReplicas sets the deployment's replica count through the API's own
// path (planning.Scaling.ScaleScheduled): a multi-node deployment the platform places gets
// its nodes picked as an operator's request would, a PD deployment's role is
// scaled through the PD update, and a refusal (a deployment placed by hand,
// no room for a replica, a role below its minimum) comes back as the product
// error the failure event records.
func (a *automationAdapter) ScaleReplicas(deploymentID uint, role string, replicas int) error {
	return a.scale(deploymentID, role, replicas)
}

func (a *automationAdapter) MarkFired(scheduleID uint, at time.Time) error {
	return a.store.MarkLifecycleScheduleFired(scheduleID, at)
}

func (a *automationAdapter) Record(deploymentID uint, code string, params map[string]any) {
	a.store.AddDeploymentEventCode(deploymentID, store.EventKindAutomation, code, params)
}

func uuidList(insts []store.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, in := range insts {
		out = append(out, in.UUID)
	}
	return out
}
