package app

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The turn's start is read from the instance's own state and sleep events: it
// is serving since its creation, not while it sleeps, and since its wake once
// woken; a stretch longer than the window reads as the window.
func TestResidencyAdapterAwakeSinceReadsTheInstanceEvents(t *testing.T) {
	res, _, st, dep, other := residencyFixture(t)
	res.now = time.Now
	before := time.Now()
	at, ok := res.AwakeSince(other.ID, []string{"batch-1"}, time.Hour)
	if !ok || at.After(before) || before.Sub(at) > time.Minute {
		t.Fatalf("awake since = %v %v, want the instance's creation just before %v", at, ok, before)
	}
	time.Sleep(30 * time.Millisecond)
	if at, ok := res.AwakeSince(other.ID, []string{"batch-1"}, 10*time.Millisecond); !ok || time.Since(at) > 25*time.Millisecond {
		t.Fatalf("awake since within 10ms = %v %v, want the window's start", at, ok)
	}
	if _, ok := res.AwakeSince(other.ID, []string{"batch-1", "not-there"}, time.Hour); ok {
		t.Fatal("an instance the events do not show serving read awake")
	}
	if _, ok := res.AwakeSince(dep.ID, []string{"auto-1"}, time.Hour); ok {
		t.Fatal("a sleeping instance read awake")
	}

	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "batch-1", State: proto.InstRunning, SleepLevel: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := res.AwakeSince(other.ID, []string{"batch-1"}, time.Hour); ok {
		t.Fatal("the instance read awake after its sleep applied")
	}
	time.Sleep(20 * time.Millisecond)
	woke := time.Now()
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "batch-1", State: proto.InstRunning, SleepLevel: 0}); err != nil {
		t.Fatal(err)
	}
	if at, ok := res.AwakeSince(other.ID, []string{"batch-1"}, time.Hour); !ok || at.Before(woke.Add(-5*time.Millisecond)) {
		t.Fatalf("awake since = %v %v, want its wake at %v", at, ok, woke)
	}
}

// The resume list read back: the deployments whose newest sleep or wake event
// is the arbiter's yield to another and that still sleep for it, a request
// waking them, under a policy put to sleep for others.
func TestResidencyAdapterPreemptedReadsTheResumeListBack(t *testing.T) {
	res, auto, st, dep, other := residencyFixture(t)
	res.now = time.Now
	preempt := func(id, forID uint) {
		auto.Record(id, automation.EventAutoSleep, map[string]any{"trigger": automation.TriggerPreempt, "reason": "rank", "for_deployment": forID, "instances": []string{"x"}, "abort": true, "level": 1})
	}
	list := func() []automation.Preemption {
		t.Helper()
		got, err := res.Preempted()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// other (anytime) yields its card to dep and sleeps; dep's policy (never)
	// is not one the arbiter keeps on its resume list even with such an event.
	if _, err := res.Yield(other.ID, []string{"batch-1"}, true); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "batch-1", State: proto.InstRunning, SleepLevel: 1}); err != nil {
		t.Fatal(err)
	}
	preempt(dep.ID, other.ID)
	if got := list(); len(got) != 0 {
		t.Fatalf("resume list before other's yield was recorded = %+v", got)
	}
	before := time.Now()
	preempt(other.ID, dep.ID)
	got := list()
	if len(got) != 1 || got[0].DeploymentID != other.ID || got[0].For != dep.ID || got[0].Since.Before(before.Add(-time.Second)) || got[0].Since.After(time.Now()) {
		t.Fatalf("resume list = %+v, want other, yielded to %d just now", got, dep.ID)
	}

	// A newer order of any other kind takes it off: a schedule's sleep, an
	// operator's sleep or wake.
	auto.Record(other.ID, automation.EventAutoSleep, map[string]any{"trigger": "schedule"})
	if got := list(); len(got) != 0 {
		t.Fatalf("resume list after a schedule's sleep = %+v", got)
	}
	preempt(other.ID, dep.ID)
	st.AddDeploymentEventCode(other.ID, store.EventKindLifecycle, store.EventSleepOrdered, map[string]any{"level": 1, "actor": "admin"})
	if got := list(); len(got) != 0 {
		t.Fatalf("resume list after an operator's sleep = %+v", got)
	}
	preempt(other.ID, dep.ID)
	st.AddDeploymentEventCode(other.ID, store.EventKindLifecycle, store.EventWakeOrdered, map[string]any{"instances": []string{"batch-1"}})
	if got := list(); len(got) != 0 {
		t.Fatalf("resume list after an operator's wake = %+v", got)
	}

	// A held sleeper is not one a request wakes.
	preempt(other.ID, dep.ID)
	if got := list(); len(got) != 1 {
		t.Fatalf("resume list = %+v, want other back on it", got)
	}
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", "batch-1").Update("sleep_held", true).Error; err != nil {
		t.Fatal(err)
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("resume list with the sleeper held = %+v", got)
	}
}
