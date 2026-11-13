package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

var residencyBase = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func at(h float64) time.Time { return residencyBase.Add(time.Duration(h * float64(time.Hour))) }

func ev(id uint, code string, when time.Time, state, phase string, level int, result string) ResidencyEvent {
	return ResidencyEvent{ID: id, Code: code, At: when, State: state, Phase: phase, Level: level, Result: result}
}

func summarize(segs []ResidencySegment) string {
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		label := string(s.State)
		if s.Level > 0 {
			label += fmt.Sprintf("-L%d", s.Level)
		}
		parts = append(parts, fmt.Sprintf("%s[%.2f,%.2f]", label, s.From.Sub(residencyBase).Hours(), s.To.Sub(residencyBase).Hours()))
	}
	return strings.Join(parts, " ")
}

func TestDeriveResidencySleepWakeSequence(t *testing.T) {
	inst := ResidencyInstance{UUID: "i1", DeploymentID: 1, Live: true, Start: at(-10)}
	events := []ResidencyEvent{
		// Before the window: it was running awake.
		ev(1, instanceEventStatusUpdated, at(-9), proto.InstRunning, "", 0, ""),
		ev(2, instanceEventSleepResult, at(-8), "", "", 0, "applied"),
		// In the window (from -6): sleep ordered at -4 (the agent reports the
		// sleep phase), result at -3.5, wake at -2 .. -1.5.
		ev(10, instanceEventStatusUpdated, at(-4), proto.InstRunning, proto.PhaseSleep, 0, ""),
		ev(11, instanceEventSleepResult, at(-3.5), "", "", 2, "applied"),
		ev(12, instanceEventStatusUpdated, at(-3.5), proto.InstRunning, "", 0, ""),
		ev(13, instanceEventStatusUpdated, at(-2), proto.InstRunning, proto.PhaseSleep, 0, ""),
		ev(14, instanceEventSleepResult, at(-1.5), "", "", 0, "applied"),
		ev(15, instanceEventStatusUpdated, at(-1.5), proto.InstRunning, "", 0, ""),
	}
	got := summarize(DeriveResidency(inst, events, at(-6), at(0)))
	want := "awake[-6.00,-4.00] switching[-4.00,-3.50] asleep-L2[-3.50,-2.00] switching[-2.00,-1.50] awake[-1.50,0.00]"
	if got != want {
		t.Fatalf("segments\n got  %s\n want %s", got, want)
	}
}

func TestDeriveResidencyIgnoresARolledBackSleepAndKnowsStartingAndRemoval(t *testing.T) {
	inst := ResidencyInstance{UUID: "i2", DeploymentID: 1}
	events := []ResidencyEvent{
		ev(1, instanceEventCreated, at(-5), proto.InstPending, "", 0, ""),
		ev(2, instanceEventStatusUpdated, at(-4.5), proto.InstPulling, "image_pull", 0, ""),
		ev(3, instanceEventStatusUpdated, at(-4), proto.InstStarting, "", 0, ""),
		ev(4, instanceEventStatusUpdated, at(-3), proto.InstRunning, "", 0, ""),
		ev(5, instanceEventSleepResult, at(-2), "", "", 1, "failed_rollback"),
		ev(6, EventInstanceRemoved, at(-1), "", "", 0, ""),
	}
	got := summarize(DeriveResidency(inst, events, at(-6), at(0)))
	want := "starting[-5.00,-3.00] awake[-3.00,-1.00]"
	if got != want {
		t.Fatalf("segments\n got  %s\n want %s", got, want)
	}
}

func TestDeriveResidencyClipsToTheWindowAndFallsBackToTheRow(t *testing.T) {
	// A stretch that began before the window and ends after it is cut to it.
	inst := ResidencyInstance{UUID: "i3", DeploymentID: 1, Live: true, Start: at(-30)}
	got := DeriveResidency(inst, []ResidencyEvent{ev(1, instanceEventStatusUpdated, at(-20), proto.InstRunning, "", 0, "")}, at(-6), at(0))
	if len(got) != 1 || !got[0].From.Equal(at(-6)) || !got[0].To.Equal(at(0)) {
		t.Fatalf("clipped = %s", summarize(got))
	}
	// No event at all (aged out): a live row says what it is now, from when
	// it exists; a removed instance is unknown.
	row := ResidencyInstance{UUID: "i4", Live: true, Start: at(-2), State: proto.InstRunning, SleepLevel: 1}
	if s := summarize(DeriveResidency(row, nil, at(-6), at(0))); s != "asleep-L1[-2.00,0.00]" {
		t.Fatalf("row fallback = %s", s)
	}
	if s := DeriveResidency(ResidencyInstance{UUID: "i5"}, nil, at(-6), at(0)); len(s) != 0 {
		t.Fatalf("removed without events = %v", s)
	}
	// A stopped instance holds nothing.
	if s := DeriveResidency(inst, []ResidencyEvent{ev(1, instanceEventStatusUpdated, at(-20), proto.InstStopped, "", 0, "")}, at(-6), at(0)); len(s) != 0 {
		t.Fatalf("stopped = %v", s)
	}
}

// seedResidency builds a node with two cards and the history of three
// instances: one live and napping, one live follower, one replaced mid-window.
func seedResidency(t *testing.T) (*Store, *Node, map[string]uint) {
	t.Helper()
	s := newTestStore(t)
	node, err := s.RegisterNode("res-node", HashToken("token"), "res-node")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]uint{}
	for _, name := range []string{"alpha", "beta"} {
		dep, err := s.CreateDeployment(withPolicy(&Deployment{ModelID: 1, NodeID: node.ID, Name: name, ServedName: name, Engine: "vllm", Sleepable: true, State: DeployActive}, yieldpolicy.TimeSlice))
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = dep.ID
	}
	return s, node, ids
}

func putEvent(t *testing.T, s *Store, uuid string, dep uint, kind, code string, when time.Time, data string) {
	t.Helper()
	if err := s.DB().Create(&InstanceEvent{InstanceUUID: uuid, DeploymentID: dep, Kind: kind, Code: code, Data: data, CreatedAt: when}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestResidencyInputsDeriveALiveAndAReplacedInstance(t *testing.T) {
	s, node, ids := seedResidency(t)
	now := time.Now().UTC().Truncate(time.Second)
	rel := func(h float64) time.Time { return now.Add(time.Duration(h * float64(time.Hour))) }
	// alpha-old held card 0 and was replaced by alpha-new at -3h; beta holds
	// card 1 and sleeps at -2h.
	for _, in := range []Instance{
		{UUID: "alpha-new", DeploymentID: ids["alpha"], NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", CreatedAt: rel(-3)},
		{UUID: "beta-0", DeploymentID: ids["beta"], NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[1]", SleepLevel: 1, CreatedAt: rel(-20)},
	} {
		in := in
		if err := s.DB().Create(&in).Error; err != nil {
			t.Fatal(err)
		}
	}
	// A noisy past the window must not matter.
	putEvent(t, s, "beta-0", ids["beta"], "state", instanceEventStatusUpdated, rel(-19), `{"state":"running","phase":""}`)
	putEvent(t, s, "alpha-old", ids["alpha"], "state", instanceEventStatusUpdated, rel(-9), `{"state":"running","phase":""}`)
	// In the window.
	putEvent(t, s, "alpha-old", ids["alpha"], EventKindResidency, EventInstanceRemoved, rel(-3), fmt.Sprintf(`{"node_id":%d,"gpus":[0]}`, node.ID))
	putEvent(t, s, "alpha-new", ids["alpha"], "state", instanceEventCreated, rel(-3), `{"state":"pending"}`)
	putEvent(t, s, "alpha-new", ids["alpha"], "state", instanceEventStatusUpdated, rel(-2.5), `{"state":"running","phase":""}`)
	putEvent(t, s, "beta-0", ids["beta"], "state", instanceEventStatusUpdated, rel(-2), `{"state":"running","phase":"sleep"}`)
	putEvent(t, s, "beta-0", ids["beta"], "sleep", instanceEventSleepResult, rel(-1.9), `{"result":"applied","level":1}`)
	putEvent(t, s, "beta-0", ids["beta"], "state", instanceEventStatusUpdated, rel(-1.9), `{"state":"running","phase":""}`)

	from := rel(-6)
	insts, events, err := s.ResidencyInputs([]uint{node.ID}, from)
	if err != nil {
		t.Fatal(err)
	}
	byCard := map[int][]string{}
	for _, inst := range insts {
		for _, seg := range DeriveResidency(inst, events[inst.EventsOf()], from, now) {
			label := fmt.Sprintf("%s:%s", seg.Instance, seg.State)
			if seg.Level > 0 {
				label += fmt.Sprintf("-L%d", seg.Level)
			}
			label += fmt.Sprintf("[%.1f,%.1f]", seg.From.Sub(now).Hours(), seg.To.Sub(now).Hours())
			for _, gpu := range inst.GPUs {
				byCard[gpu] = append(byCard[gpu], label)
			}
		}
	}
	if got, want := strings.Join(byCard[0], " "), "alpha-old:awake[-6.0,-3.0] alpha-new:starting[-3.0,-2.5] alpha-new:awake[-2.5,0.0]"; !sameWords(got, want) {
		t.Fatalf("card 0 = %s, want %s", got, want)
	}
	if got, want := strings.Join(byCard[1], " "), "beta-0:awake[-6.0,-2.0] beta-0:switching[-2.0,-1.9] beta-0:asleep-L1[-1.9,0.0]"; !sameWords(got, want) {
		t.Fatalf("card 1 = %s, want %s", got, want)
	}
}

func sameWords(a, b string) bool {
	x, y := map[string]int{}, map[string]int{}
	for _, w := range strings.Fields(a) {
		x[w]++
	}
	for _, w := range strings.Fields(b) {
		y[w]++
	}
	if len(x) != len(y) {
		return false
	}
	for w, n := range x {
		if y[w] != n {
			return false
		}
	}
	return true
}

func TestDeletingAnInstanceRecordsWhereItSat(t *testing.T) {
	s, node, ids := seedResidency(t)
	if err := s.DB().Create(&Instance{UUID: "gone", DeploymentID: ids["alpha"], NodeID: node.ID, State: proto.InstStopped, GPUIndexes: "[0,1]",
		// Old startup observations must not be copied into removal events.
		DesiredStateJSON: `{"launch":{"startup_estimate":{"weight_bytes":1073741824,"environment_key":"legacy-cohort"}}}`,
		Measured:         `{"StartupSeconds":100,"ModelLoadingSeconds":50}`,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Create(&Instance{UUID: "never-placed", DeploymentID: ids["alpha"], NodeID: node.ID, State: proto.InstPending}).Error; err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"gone", "never-placed"} {
		if err := s.DeleteInstance(uuid); err != nil {
			t.Fatal(err)
		}
	}
	var rows []InstanceEvent
	if err := s.DB().Where("code = ?", EventInstanceRemoved).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].InstanceUUID != "gone" || rows[0].Kind != EventKindResidency || rows[0].DeploymentID != ids["alpha"] {
		t.Fatalf("removed events = %+v", rows)
	}
	if want := fmt.Sprintf(`{"gpus":[0,1],"node_id":%d}`, node.ID); rows[0].Data != want {
		t.Fatalf("data = %s, want %s", rows[0].Data, want)
	}
	// The removal is found by the node it names and no other.
	insts, _, err := s.ResidencyInputs([]uint{node.ID}, time.Now().Add(-time.Hour))
	if err != nil || len(insts) != 1 || insts[0].UUID != "gone" || insts[0].Live || len(insts[0].GPUs) != 2 {
		t.Fatalf("inputs = %+v, %v", insts, err)
	}
	if insts, _, _ = s.ResidencyInputs([]uint{node.ID + 99}, time.Now().Add(-time.Hour)); len(insts) != 0 {
		t.Fatalf("another node's inputs = %+v", insts)
	}
}

func TestResidencyFollowerTakesItsLeadersSleep(t *testing.T) {
	s, node, ids := seedResidency(t)
	now := time.Now().UTC()
	for _, in := range []Instance{
		{UUID: "lead", DeploymentID: ids["alpha"], NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", DistGroup: "g", DistAttempt: 1, NNodes: 2, CreatedAt: now.Add(-3 * time.Hour)},
		{UUID: "follow", DeploymentID: ids["alpha"], NodeID: node.ID + 1, State: proto.InstRunning, GPUIndexes: "[0]", DistGroup: "g", DistAttempt: 1, Headless: true, NNodes: 2, CreatedAt: now.Add(-3 * time.Hour)},
	} {
		in := in
		if err := s.DB().Create(&in).Error; err != nil {
			t.Fatal(err)
		}
	}
	putEvent(t, s, "lead", ids["alpha"], "state", instanceEventStatusUpdated, now.Add(-2*time.Hour), `{"state":"running","phase":""}`)
	putEvent(t, s, "lead", ids["alpha"], "sleep", instanceEventSleepResult, now.Add(-time.Hour), `{"result":"applied","level":1}`)
	insts, events, err := s.ResidencyInputs([]uint{node.ID + 1}, now.Add(-6*time.Hour))
	if err != nil || len(insts) != 1 || insts[0].EventsFrom != "lead" {
		t.Fatalf("inputs = %+v, %v", insts, err)
	}
	segs := DeriveResidency(insts[0], events[insts[0].EventsOf()], now.Add(-6*time.Hour), now)
	if len(segs) != 2 || segs[0].State != ResidencyAwake || segs[1].State != ResidencyAsleep || segs[1].Instance != "follow" {
		t.Fatalf("follower segments = %+v", segs)
	}
}

// The replay of the scheduling activity hands over every yield and wake of
// the window, deleted deployments' included; counted the way the activity
// counts them they give each deployment's switches.
func TestSchedulingActivityReplayCountsSwitches(t *testing.T) {
	s, _, ids := seedResidency(t)
	now := time.Now().UTC()
	a, b := ids["alpha"], ids["beta"]
	// beta yields to alpha twice, alpha is woken once by a request and once by
	// a schedule; one older sleep is outside the window; an idle sleep is not
	// a yield.
	old := now.Add(-48 * time.Hour)
	putEvent(t, s, "", b, EventKindAutomation, "AUTO_SLEEP", old, fmt.Sprintf(`{"trigger":"preempt","for_deployment":%d}`, a))
	putEvent(t, s, "", a, EventKindAutomation, "AUTO_WAKE", now.Add(-5*time.Hour), `{"trigger":"request"}`)
	putEvent(t, s, "", b, EventKindAutomation, "AUTO_SLEEP", now.Add(-4*time.Hour), fmt.Sprintf(`{"trigger":"preempt","reason":"rank","for_deployment":%d}`, a))
	putEvent(t, s, "", b, EventKindAutomation, "AUTO_SLEEP", now.Add(-3*time.Hour), `{"trigger":"idle"}`)
	putEvent(t, s, "", a, EventKindAutomation, "AUTO_WAKE", now.Add(-2*time.Hour), `{"trigger":"schedule"}`)
	putEvent(t, s, "", b, EventKindAutomation, "AUTO_SLEEP", now.Add(-time.Hour), fmt.Sprintf(`{"trigger":"preempt","reason":"turn","for_deployment":%d}`, a))
	// An operator's yielding wake of alpha put beta to sleep: a yield too,
	// recorded on beta with alpha's name (no id), so beta's count has it and
	// no deployment is credited as yielded-to.
	putEvent(t, s, "", b, EventKindAutomation, "YIELDED_FOR_WAKE", now.Add(-30*time.Minute), `{"deployment":"alpha","instances":["b-1"],"level":1}`)
	// gamma, deleted since, yielded to alpha twice: alpha's yielded-to counts
	// them though the one that gave way is gone, and gamma keeps its count.
	gone, err := s.CreateDeployment(withPolicy(&Deployment{ModelID: 1, Name: "gamma", ServedName: "gamma", Engine: "vllm", Sleepable: true, State: DeployActive}, yieldpolicy.Anytime))
	if err != nil {
		t.Fatal(err)
	}
	g := gone.ID
	putEvent(t, s, "", g, EventKindAutomation, "AUTO_SLEEP", now.Add(-50*time.Minute), fmt.Sprintf(`{"trigger":"preempt","reason":"rank","for_deployment":%d}`, a))
	putEvent(t, s, "", g, EventKindAutomation, "AUTO_SLEEP", now.Add(-40*time.Minute), fmt.Sprintf(`{"trigger":"preempt","reason":"rank","for_deployment":%d}`, a))
	if err := s.DeleteDeployment(g); err != nil {
		t.Fatal(err)
	}
	// Another kind of event with a switch's code is not one.
	putEvent(t, s, "", b, "state", "AUTO_WAKE", now.Add(-20*time.Minute), `{}`)
	through, err := s.LastEventID()
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint]ResidencySwitches{}
	count := func(evs []ObservedEvent) {
		for _, ev := range evs {
			c := got[ev.DeploymentID]
			switch {
			case ev.Wake:
				c.Wakes++
			case ev.Yield:
				c.Yielded++
				if ev.ForDeployment != 0 && ev.ForDeployment != ev.DeploymentID {
					to := got[ev.ForDeployment]
					to.YieldedTo++
					got[ev.ForDeployment] = to
				}
			default:
				continue
			}
			got[ev.DeploymentID] = c
		}
	}
	if err := s.ReplaySchedulingActivity(ActivityReplay{Since: now.Add(-6 * time.Hour), ThroughID: through, Scheduling: count}, now); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[a] != (ResidencySwitches{YieldedTo: 4, Wakes: 2}) || got[b] != (ResidencySwitches{Yielded: 3}) || got[g] != (ResidencySwitches{Yielded: 2}) {
		t.Fatalf("counts = %+v", got)
	}
	if !got[a].TookPartInYields() || !got[g].TookPartInYields() || (ResidencySwitches{Wakes: 3}).TookPartInYields() {
		t.Fatal("taking part in yields is giving cards up or being given them, not waking")
	}
	// The overview's count of the same window goes by the same definition.
	events, err := s.YieldEvents(now.Add(-6 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	overview := 0
	for _, ev := range events {
		if IsYield(ev) {
			overview++
		}
	}
	if timeline := got[a].Yielded + got[b].Yielded + got[g].Yielded; overview != timeline {
		t.Fatalf("the overview counts %d yields, the timeline %d", overview, timeline)
	}
	names, err := s.ResidencyDeployments([]uint{a, g, 9999})
	if err != nil || names[a].Name != "alpha" || names[a].Yield != yieldpolicy.TimeSlice || names[a].Deleted || names[a].Since.IsZero() || !names[a].Until.IsZero() {
		t.Fatalf("names = %+v, %v", names, err)
	}
	// A deleted one keeps its name and when it went; its policy is not kept.
	if d := names[g]; d.Name != "gamma" || !d.Deleted || d.Yield != "" || !d.Since.IsZero() || d.Until.IsZero() {
		t.Fatalf("deleted deployment = %+v", d)
	}
	if _, ok := names[9999]; ok {
		t.Fatal("an unknown deployment has a name")
	}
}

// Every read of the timeline must be a range of an index: a plan that scans
// instance_events or sorts a temp b-tree would cost a pass over the whole
// timeline on every refresh of the scheduling center.
func TestResidencyQueryPlans(t *testing.T) {
	s := newTestStore(t)
	db := s.DB()
	queries := map[string]*gorm.DB{
		"window start":    residencyBoundaryQuery(db, residencyBase),
		"removed":         residencyRemovedQuery(db, 100),
		"window events":   residencyWindowQuery(db, []string{"a", "b"}, 100, 0),
		"bounded events":  residencyWindowQuery(db, []string{"a", "b"}, 100, 200),
		"prior state":     residencyPriorQuery(db, []string{"a", "b"}, 100, residencyStateCodes, 1),
		"prior sleep":     residencyPriorQuery(db, []string{"a", "b"}, 100, []string{instanceEventSleepResult}, residencyPriorSleepResults),
		"activity replay": activityReplayQuery(db, EventKindAutomation, []string{codeAutoSleep, codeAutoWake}, 100, 200),
	}
	// The activity's replay is one range of the (kind, id) index over the
	// window, whatever deployments there are.
	stmt := activityReplayQuery(db, EventKindAutomation, []string{codeAutoSleep, codeAutoWake}, 100, 200).Session(&gorm.Session{DryRun: true}).Find(&[]InstanceEvent{}).Statement
	var plan []struct{ Detail string }
	if err := db.Raw("EXPLAIN QUERY PLAN "+stmt.SQL.String(), stmt.Vars...).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !strings.Contains(plan[0].Detail, "idx_instance_events_kind_id (kind=? AND id>? AND id<?)") {
		t.Errorf("activity replay plan = %+v, want one range of idx_instance_events_kind_id", plan)
	}
	for name, q := range queries {
		stmt := q.Session(&gorm.Session{DryRun: true}).Find(&[]InstanceEvent{}).Statement
		var plan []struct{ Detail string }
		if err := db.Raw("EXPLAIN QUERY PLAN "+stmt.SQL.String(), stmt.Vars...).Scan(&plan).Error; err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var text []string
		for _, row := range plan {
			text = append(text, row.Detail)
		}
		joined := strings.Join(text, " | ")
		t.Logf("%s: %s", name, joined)
		if !strings.Contains(joined, "USING INDEX") && !strings.Contains(joined, "USING COVERING INDEX") {
			t.Errorf("%s reads without an index: %s", name, joined)
		}
		if strings.Contains(joined, "TEMP B-TREE") || strings.Contains(joined, "SCAN instance_events") && !strings.Contains(joined, "USING") {
			t.Errorf("%s sorts or scans the table: %s", name, joined)
		}
	}
}

// A rolled-back sleep order before the window says nothing about the level:
// the newest one that applied does.
func TestResidencyPriorLevelSkipsARolledBackOrder(t *testing.T) {
	s, node, ids := seedResidency(t)
	now := time.Now().UTC()
	if err := s.DB().Create(&Instance{UUID: "napper", DeploymentID: ids["alpha"], NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", CreatedAt: now.Add(-30 * time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	putEvent(t, s, "napper", ids["alpha"], "state", instanceEventStatusUpdated, now.Add(-29*time.Hour), `{"state":"running","phase":""}`)
	putEvent(t, s, "napper", ids["alpha"], "sleep", instanceEventSleepResult, now.Add(-12*time.Hour), `{"result":"applied","level":1}`)
	putEvent(t, s, "napper", ids["alpha"], "sleep", instanceEventSleepResult, now.Add(-10*time.Hour), `{"result":"failed_rollback","target":0,"actual":1}`)
	putEvent(t, s, "other", ids["beta"], "state", instanceEventStatusUpdated, now.Add(-time.Hour), `{"state":"running","phase":""}`) // something happened in the window
	from := now.Add(-6 * time.Hour)
	insts, events, err := s.ResidencyInputs([]uint{node.ID}, from)
	if err != nil || len(insts) != 1 {
		t.Fatalf("inputs = %+v, %v", insts, err)
	}
	segs := DeriveResidency(insts[0], events["napper"], from, now)
	if len(segs) != 1 || segs[0].State != ResidencyAsleep || segs[0].Level != 1 || !segs[0].From.Equal(from) {
		t.Fatalf("segments = %+v, want asleep L1 over the whole window", segs)
	}
}

// The events before the window are read for many instances at once: each
// instance, on either side of a batch's edge, still gets its own newest state
// and its own newest sleep result that applied, never a neighbour's.
func TestResidencyPriorEventsAcrossBatches(t *testing.T) {
	s, node, ids := seedResidency(t)
	now := time.Now().UTC()
	n := residencyEventChunk + 7
	for i := 0; i < n; i++ {
		uuid := fmt.Sprintf("many-%03d", i)
		if err := s.DB().Create(&Instance{UUID: uuid, DeploymentID: ids["alpha"], NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", CreatedAt: now.Add(-30 * time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
		putEvent(t, s, uuid, ids["alpha"], "state", instanceEventStatusUpdated, now.Add(-29*time.Hour), `{"state":"running","phase":""}`)
		level := 1 + i%2
		putEvent(t, s, uuid, ids["alpha"], "sleep", instanceEventSleepResult, now.Add(-12*time.Hour), fmt.Sprintf(`{"result":"applied","level":%d}`, level))
		if i%3 == 0 {
			putEvent(t, s, uuid, ids["alpha"], "sleep", instanceEventSleepResult, now.Add(-10*time.Hour), `{"result":"failed_rollback","target":0,"actual":1}`)
		}
	}
	putEvent(t, s, "other", ids["beta"], "state", instanceEventStatusUpdated, now.Add(-time.Hour), `{"state":"running","phase":""}`) // something happened in the window
	from := now.Add(-6 * time.Hour)
	insts, events, err := s.ResidencyInputs([]uint{node.ID}, from)
	if err != nil || len(insts) != n {
		t.Fatalf("inputs = %d instances, %v; want %d", len(insts), err, n)
	}
	for _, inst := range insts {
		var i int
		if _, err := fmt.Sscanf(inst.UUID, "many-%03d", &i); err != nil {
			t.Fatalf("unexpected instance %q", inst.UUID)
		}
		if got := len(events[inst.UUID]); got != 2 {
			t.Fatalf("%s has %d events before the window, want its state and its applied sleep: %+v", inst.UUID, got, events[inst.UUID])
		}
		segs := DeriveResidency(inst, events[inst.UUID], from, now)
		if len(segs) != 1 || segs[0].State != ResidencyAsleep || segs[0].Level != 1+i%2 {
			t.Fatalf("%s segments = %+v, want asleep L%d over the whole window", inst.UUID, segs, 1+i%2)
		}
	}
}
