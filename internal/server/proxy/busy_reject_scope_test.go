package proxy

import (
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/priority"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// On a replica that does not schedule by priority the engine runs requests
// in arrival order, so every lease holds a slot whatever its class: two
// batch requests fill a two-slot replica for a normal one.
func TestArrivalOrderReplicaCountsEveryLease(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prioritized bool
		busy        bool
	}{
		{"arrival order", false, true},
		{"priority scheduling", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBalancer(nil)
			replica := []candidate{{instanceUUID: "a", nodeUUID: "n", slots: 2, prioritized: tc.prioritized}}
			b.take(replica[0], admission{priority: -40})
			b.take(replica[0], admission{priority: -40})
			if l, busy := b.pick(replica, nil, allOnline, declared(0)); busy != tc.busy || (l == nil) != tc.busy {
				t.Fatalf("lease %v busy %v, want busy %v", l, busy, tc.busy)
			}
		})
	}
}

// Through the gateway: two batch requests held on "two", which does not
// schedule by priority, leave no slot for a declared normal one.
func TestBusyOnAnArrivalOrderReplicaCountsLowerClasses(t *testing.T) {
	rig := newBusyRig(t)
	key := rig.key(t, store.APIKeyPriority{Class: priority.Normal, Ceiling: priority.Critical})
	held := rig.hold(t, key, "two", priority.Batch, "", 2)
	if rec := rig.chat(key, "two", "", "reject"); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"code":"MODEL_BUSY"`) {
		t.Fatalf("a normal behind two batch on an arrival-order replica = %d %s, want 429 MODEL_BUSY", rec.Code, rec.Body)
	}
	rig.letAll()
	for range 2 {
		if rec := <-held; rec.Code != http.StatusOK {
			t.Fatalf("held request = %d", rec.Code)
		}
	}
}

// A deployment /v1/key reports busy_reject:false is never refused, even
// when the only replica left to pick from states slots and is full: here
// mixed's replica of unknown slots drains, on the route snapshot's path and
// on the rows read for the request alike.
func TestBusyRejectFalseNeverRefusesWhicheverReplicasAreLeft(t *testing.T) {
	rig := newBusyRig(t)
	key, err := rig.st.CreateAPIKey("busy", 0)
	if err != nil {
		t.Fatal(err)
	}
	depID := rig.deployments["mixed"]
	rig.p.SetDraining(depID, []string{"mixed-b"}, true)
	full := candidate{instanceUUID: "mixed-a", nodeUUID: "n", slots: 1}
	rig.p.balancer.take(full, admission{})

	rig.forwarded(t, key, "mixed", "", "reject")

	dep, err := rig.st.GetDeployment(depID)
	if err != nil {
		t.Fatal(err)
	}
	tgt, rejection, _ := rig.p.selectDeploymentInstances(dep, "mixed", "", declared(0))
	if rejection != nil || tgt.instanceUUID != "mixed-a" {
		t.Fatalf("rows path = %v %v, want the full mixed-a leased", tgt.instanceUUID, rejection)
	}
	rig.p.releaseTarget(tgt)
	rig.letAll()
}

// A full replica ordered to sleep after the route snapshot was built is
// asleep, not busy: the snapshot's busy answer stands only while no instance
// change has passed it.
func TestFullReplicaOrderedToSleepIsAsleepNotBusy(t *testing.T) {
	rig := newBusyRig(t)
	key, err := rig.st.CreateAPIKey("busy", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the snapshot as it is: no rebuild catches the sleep order up.
	rig.p.routes.close()
	two := candidate{instanceUUID: "two-a", nodeUUID: "n", slots: 2}
	rig.p.balancer.take(two, admission{})
	rig.p.balancer.take(two, admission{})
	if rec := rig.chat(key, "two", "", "reject"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("full and awake = %d %s, want 429", rec.Code, rec.Body)
	}
	if err := rig.st.OrderInstancesSleep(rig.deployments["two"], []string{"two-a"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	if deps, ok := rig.p.routes.snapshot("two"); !ok || len(deps) != 1 || len(deps[0].candidates) != 1 {
		t.Fatal("the snapshot no longer routes two-a; the test needs it stale")
	}
	rec := rig.chat(key, "two", "", "reject")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"MODEL_ASLEEP"`) {
		t.Fatalf("full and ordered to sleep = %d %s, want 503 MODEL_ASLEEP", rec.Code, rec.Body)
	}
}

// A resend after a connection failure goes only to a replica that schedules
// as the first did, so the priority written and the
// X-GPUWorks-Priority-Scheduling header stay true of the replica answering.
func TestResendKeepsTheFirstReplicasScheduling(t *testing.T) {
	for _, first := range []bool{true, false} {
		b := newBalancer(nil)
		candidates := []candidate{
			{instanceUUID: "first", nodeUUID: "n", modelName: "m", prioritized: first},
			{instanceUUID: "other", nodeUUID: "n", modelName: "m", prioritized: !first},
			{instanceUUID: "same", nodeUUID: "n", modelName: "m", prioritized: first},
		}
		lease, _ := b.pickFailover(candidates, nil, allOnline, admission{})
		transport := &retryTransport{balancer: b, hub: dialTestHub{}}
		attempt := &routeAttempt{candidates: candidates, current: lease, tried: map[string]bool{"first": true}, attempts: 1, modelName: "m", policy: deploy.Failover, prioritized: first}
		lease.release(outcomeConnFailure)
		next := transport.pickNext(attempt, false)
		if next == nil || next.c.instanceUUID != "same" {
			t.Fatalf("first prioritized %v: resend = %v, want the replica scheduling alike", first, next)
		}
		if got := b.counts("other").total; got != 0 {
			t.Fatalf("first prioritized %v: the other replica holds %d leases", first, got)
		}
		next.release(outcomeConnFailure)
		attempt.tried["same"] = true
		if next := transport.pickNext(attempt, false); next != nil {
			t.Fatalf("first prioritized %v: with only an unlike replica left the resend = %v, want none", first, next)
		}
	}
}

// The header follows the replica the request was leased on, as its candidate
// was read, not a later snapshot.
func TestPrioritySchedulingHeaderFollowsTheLeasedCandidate(t *testing.T) {
	p := &Proxy{}
	for _, on := range []bool{true, false} {
		tgt := target{instanceUUID: "x", lease: &lease{c: candidate{instanceUUID: "x", prioritized: on}}}
		if got := p.targetRunsPriority(tgt); got != on {
			t.Fatalf("targetRunsPriority = %v, want %v", got, on)
		}
	}
	if p.targetRunsPriority(target{instanceUUID: "x"}) {
		t.Fatal("a target leased no other way runs priority without a snapshot")
	}
}
