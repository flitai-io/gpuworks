package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeriveRuntimeStateAggregatesInstanceTruth(t *testing.T) {
	cases := []struct {
		desired        store.DeploymentState
		states         []string
		modelStates    []string
		scheduleStaled bool
		want           string
	}{
		{"active", nil, nil, false, "pending"},
		// Serving wins (GWKS-334): a running replica beside a pulling one serves.
		{"active", []string{proto.InstPulling, proto.InstRunning}, nil, false, "running"},
		{"active", []string{proto.InstPulling}, nil, false, "pulling"},
		{"active", []string{proto.InstStarting}, nil, false, "starting"},
		{"active", []string{proto.InstRunning, proto.InstRunning}, nil, false, "running"},
		{"active", []string{proto.InstFailed}, nil, false, "failed"},
		{"active", []string{proto.InstRunning, proto.InstFailed}, nil, false, "running"},
		{"active", []string{proto.InstStarting, proto.InstFailed}, nil, false, "starting"},
		{"active", []string{proto.InstPulling, proto.InstFailed}, nil, false, "pulling"},
		{"active", []string{proto.InstStopped, proto.InstFailed}, nil, false, "failed"},
		{"stopped", []string{proto.InstRunning}, nil, false, "stopped"},
		// A replica moved off an offline node stays removing until the node
		// returns: the running replacement is what the deployment is.
		{"active", []string{proto.InstRunning, proto.InstRunning, proto.InstRemoving}, nil, false, "running"},
		{"active", []string{proto.InstStarting, proto.InstRemoving}, nil, false, "starting"},
		{"active", []string{proto.InstRemoving}, nil, false, "starting"},
		// An earlier failure beside a replica being removed is history, as
		// beside any other live row.
		{"active", []string{proto.InstRemoving, proto.InstFailed}, nil, false, "starting"},
		{"active", []string{proto.InstDraining}, nil, false, "running"},
		// No instance can exist until every required model file is present.
		// A model file that failed, or one that is stuck awaiting an
		// operator-triggered download, must not read the same as "pending"
		// (about to schedule normally) — see DeriveRuntimeState.
		{"active", nil, []string{proto.MFFailed}, false, "failed"},
		{"active", nil, []string{proto.MFDownloadRequired}, false, "failed"},
		{"active", nil, []string{proto.MFPending}, false, "pending"},
		{"active", nil, []string{proto.MFDownloading}, false, "pending"},
		// The scheduler refused a placement and kept refusing past the
		// configured window. Reporting that as `pending` is what left
		// deployments sitting forever with no reachable reason — see
		// Controller.recordUnschedulable.
		{"active", nil, nil, true, "failed"},
		// Weights still downloading is not a stall, however long the
		// scheduler's own record has been around: there is nothing to place yet.
		{"active", nil, []string{proto.MFDownloading}, true, "pending"},
		// `registered` is the other ready state (a locally registered path), and
		// it is what a deployment of a local model actually carries. Missing it
		// here is what the L2 case caught.
		{"active", nil, []string{proto.MFRegistered}, true, "failed"},
		{"active", nil, []string{proto.MFRegistered}, false, "pending"},
		{"active", nil, []string{proto.MFAbsent}, true, "pending"},
		// An instance exists, so the stalled flag no longer describes anything.
		{"active", []string{proto.InstRunning}, nil, true, "running"},
	}
	for _, tc := range cases {
		instances := make([]store.Instance, 0, len(tc.states))
		for _, state := range tc.states {
			instances = append(instances, store.Instance{State: state})
		}
		if got := DeriveRuntimeState(tc.desired, instances, tc.modelStates, tc.scheduleStaled); got != tc.want {
			t.Fatalf("desired=%s states=%v modelStates=%v stalled=%v got=%s want=%s",
				tc.desired, tc.states, tc.modelStates, tc.scheduleStaled, got, tc.want)
		}
	}
}

// A headless rank exposes no endpoint: the gateway routes to the leader only,
// so a running follower is neither a serving replica nor a live one that
// makes its failed leader history.
func TestDeriveRuntimeStateDoesNotServeFromAHeadlessRank(t *testing.T) {
	follower := store.Instance{State: proto.InstRunning, Headless: true, NodeRank: 1}
	for _, tc := range []struct {
		name      string
		instances []store.Instance
		want      string
	}{
		{"failed leader beside a running follower", []store.Instance{follower, {State: proto.InstFailed}}, "failed"},
		{"running follower alone", []store.Instance{follower}, "starting"},
		{"running leader and follower", []store.Instance{follower, {State: proto.InstRunning}}, "running"},
		{"starting leader beside a running follower", []store.Instance{follower, {State: proto.InstStarting}}, "starting"},
	} {
		if got := DeriveRuntimeState(store.DeployActive, tc.instances, nil, false); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// GWKS-334: serving wins. A replica the gateway serves from makes the
// deployment running; a warming replacement beside it (a scale-out, a rolling
// restart) does not turn it starting, and an asleep replica beside one is
// still running here so that deployment.EffectiveState can read it asleep. A
// headless follower never makes a replica live, and a leader whose generation
// is missing a running follower is not serving (planning.GroupsIncomplete,
// the gateway's rule).
func TestDeriveRuntimeStateServingReplicaWins(t *testing.T) {
	running := func(uuid string) store.Instance {
		return store.Instance{UUID: uuid, State: proto.InstRunning}
	}
	asleep := store.Instance{UUID: "asleep", State: proto.InstRunning, SleepLevel: 1}
	leader := func(uuid, state string, attempt int) store.Instance {
		return store.Instance{UUID: uuid, State: state, NNodes: 2, DistGroup: "g-" + uuid, DistAttempt: attempt}
	}
	follower := func(of store.Instance, state string) store.Instance {
		return store.Instance{UUID: of.UUID + "-r1", State: state, NNodes: 2, NodeRank: 1, Headless: true, DistGroup: of.DistGroup, DistAttempt: of.DistAttempt}
	}
	role := func(uuid, r, state string) store.Instance {
		return store.Instance{UUID: uuid, Role: r, State: state}
	}
	for _, tc := range []struct {
		name      string
		instances []store.Instance
		want      string
	}{
		{"running replica beside a pulling one (scale-out)", []store.Instance{running("a"), {UUID: "b", State: proto.InstPulling}}, "running"},
		{"running replica beside a starting replacement (rolling restart)", []store.Instance{running("a"), {UUID: "b", State: proto.InstStarting}}, "running"},
		{"running replica beside a pending one", []store.Instance{running("a"), {UUID: "b", State: proto.InstPending}}, "running"},
		{"running replica beside an optimizing one", []store.Instance{running("a"), {UUID: "b", State: proto.InstOptimizing}}, "running"},
		{"asleep replica beside a starting one", []store.Instance{asleep, {UUID: "b", State: proto.InstStarting}}, "running"},
		{"pulling replica alone still pulls", []store.Instance{{UUID: "b", State: proto.InstPulling}, {UUID: "c", State: proto.InstFailed}}, "pulling"},

		{"leader failed beside a draining follower", []store.Instance{leader("l", proto.InstFailed, 1), follower(leader("l", proto.InstFailed, 1), proto.InstDraining)}, "failed"},
		{"leader failed beside a starting follower", []store.Instance{leader("l", proto.InstFailed, 1), follower(leader("l", proto.InstFailed, 1), proto.InstStarting)}, "failed"},
		{"leader running beside a failed follower", []store.Instance{leader("l", proto.InstRunning, 1), follower(leader("l", proto.InstRunning, 1), proto.InstFailed)}, "failed"},
		{"leader running beside a draining follower", []store.Instance{leader("l", proto.InstRunning, 1), follower(leader("l", proto.InstRunning, 1), proto.InstDraining)}, "starting"},
		{"leader running beside a starting follower", []store.Instance{leader("l", proto.InstRunning, 1), follower(leader("l", proto.InstRunning, 1), proto.InstStarting)}, "starting"},
		{"whole generation running beside a starting replacement", []store.Instance{leader("l", proto.InstRunning, 1), follower(leader("l", proto.InstRunning, 1), proto.InstRunning), leader("m", proto.InstStarting, 1)}, "running"},
		{"whole generation running beside an old generation's failed follower", []store.Instance{leader("l", proto.InstRunning, 2), follower(leader("l", proto.InstRunning, 2), proto.InstRunning), follower(leader("l", proto.InstRunning, 1), proto.InstFailed)}, "running"},
		{"broken generation beside a serving replica", []store.Instance{leader("l", proto.InstRunning, 1), follower(leader("l", proto.InstRunning, 1), proto.InstFailed), running("a")}, "running"},

		// PD: a role with nothing serving keeps the deployment from serving,
		// so a warming role still reads starting as before.
		{"PD prefill running, decode starting", []store.Instance{role("p", "prefill", proto.InstRunning), role("d", "decode", proto.InstStarting)}, "starting"},
		{"PD prefill running, decode pulling", []store.Instance{role("p", "prefill", proto.InstRunning), role("d", "decode", proto.InstPulling)}, "pulling"},
		{"PD both roles running, a second decode starting", []store.Instance{role("p", "prefill", proto.InstRunning), role("d", "decode", proto.InstRunning), role("d2", "decode", proto.InstStarting)}, "running"},
		{"PD both roles running", []store.Instance{role("p", "prefill", proto.InstRunning), role("d", "decode", proto.InstRunning)}, "running"},
	} {
		if got := DeriveRuntimeState(store.DeployActive, tc.instances, nil, false); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}
