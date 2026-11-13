package app

import (
	"encoding/json"
	"time"

	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// residencyOrderCodes are the events that put a deployment to sleep or wake
// it: the newest of them says who put it to sleep last.
var residencyOrderCodes = []string{
	automation.EventAutoSleep, automation.EventAutoWake, automation.EventYieldedForWake,
	store.EventSleepOrdered, store.EventWakeOrdered,
}

// AwakeSince is when the deployment's instances last began serving, as the
// residency timeline (their state and sleep events) says, looking back
// within: the latest start of them, false unless the timeline shows each one
// serving now.
func (a *residencyAdapter) AwakeSince(deploymentID uint, instances []string, within time.Duration) (time.Time, bool) {
	if len(instances) == 0 || within <= 0 {
		return time.Time{}, false
	}
	now := a.now().UTC()
	from := now.Add(-within)
	insts, events, err := a.store.ResidencyInputsOf([]uint{deploymentID}, from)
	if err != nil {
		return time.Time{}, false
	}
	want := map[string]bool{}
	for _, uuid := range instances {
		want[uuid] = true
	}
	var latest time.Time
	found := 0
	for _, inst := range insts {
		if !want[inst.UUID] {
			continue
		}
		segs := store.DeriveResidency(inst, events[inst.EventsOf()], from, now)
		if len(segs) == 0 {
			return time.Time{}, false
		}
		last := segs[len(segs)-1]
		if last.State != store.ResidencyAwake || last.To.Before(now) {
			return time.Time{}, false
		}
		if last.From.After(latest) {
			latest = last.From
		}
		found++
	}
	if found != len(want) {
		return time.Time{}, false
	}
	return latest, true
}

// Preempted are the deployments whose newest sleep or wake event is a yield
// to another (AUTO_SLEEP, trigger preempt) and that still sleep for it: a
// sleeper the yield left not held, which a request wakes, under a policy that
// is put to sleep for others (the arbiter keeps no other kind on its resume
// list). Anything an operator ordered since, or a wake, takes a deployment
// off. Only deployments with a sleeper are read.
func (a *residencyAdapter) Preempted() ([]automation.Preemption, error) {
	sleepers, err := a.store.SleepingGPUHolders()
	if err != nil {
		return nil, err
	}
	looked := map[uint]bool{}
	var out []automation.Preemption
	for _, in := range sleepers {
		id := in.DeploymentID
		if in.SleepHeld || looked[id] {
			continue
		}
		looked[id] = true
		dep, ok := a.wakeableByRequest(id)
		if !ok || !dep.Policy().Preemptible() {
			continue
		}
		ev, found, err := a.store.LatestDeploymentEvent(id, residencyOrderCodes)
		if err != nil {
			return nil, err
		}
		if !found || !yieldedToAnother(ev) {
			continue
		}
		out = append(out, automation.Preemption{DeploymentID: id, Since: ev.CreatedAt, For: store.EventForDeployment(ev.Data)})
	}
	return out, nil
}

// yieldedToAnother says the event is the arbiter putting its deployment to
// sleep for another.
func yieldedToAnother(ev store.InstanceEvent) bool {
	if ev.Code != automation.EventAutoSleep {
		return false
	}
	var params struct {
		Trigger string `json:"trigger"`
	}
	return json.Unmarshal([]byte(ev.Data), &params) == nil && params.Trigger == automation.TriggerPreempt
}
