package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/schedactivity"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// The event history the scheduling activity is sized for (plan B8): 1000
// deployments that share cards, each switching 10 times an hour.
const (
	activityDeployments     = 1000
	activitySwitchesPerHour = 10
)

// seedActivityHistory writes hours of raw history ending at end: for every
// switching deployment, activitySwitchesPerHour yields an hour (an
// automation sleep naming its instance and the deployment it made room for)
// and its instance's sleep result (asleep, then awake again at the next).
// It returns the rows written.
func seedActivityHistory(t *testing.T, st *store.Store, deps []uint, insts []string, end time.Time, hours int) int {
	t.Helper()
	reasons := []string{"idle", "rank", "turn"}
	gap := time.Hour / activitySwitchesPerHour
	written := 0
	for h := hours; h > 0; h-- {
		hourStart := end.Add(-time.Duration(h) * time.Hour)
		rows := make([]store.InstanceEvent, 0, len(deps)*activitySwitchesPerHour*2)
		for s := 0; s < activitySwitchesPerHour; s++ {
			for i, dep := range deps {
				at := hourStart.Add(time.Duration(s)*gap + time.Duration(i)*time.Second%gap)
				other := deps[(i+1)%len(deps)]
				data := fmt.Sprintf(`{"trigger":"preempt","reason":%q,"for_deployment":%d,"instances":[%q],"level":1}`, reasons[(i+s)%3], other, insts[i])
				level := (s + 1) % 2
				rows = append(rows,
					store.InstanceEvent{DeploymentID: dep, Kind: store.EventKindAutomation, Code: "AUTO_SLEEP", Data: data, CreatedAt: at},
					store.InstanceEvent{InstanceUUID: insts[i], DeploymentID: dep, Kind: "sleep", Code: "INSTANCE_SLEEP_RESULT", Data: fmt.Sprintf(`{"result":"applied","level":%d}`, level), CreatedAt: at})
			}
		}
		sort.SliceStable(rows, func(a, b int) bool { return rows[a].CreatedAt.Before(rows[b].CreatedAt) })
		if err := st.DB().Transaction(func(tx *gorm.DB) error { return insertEvents(tx, rows) }); err != nil {
			t.Fatal(err)
		}
		written += len(rows)
	}
	return written
}

// seedActivityRollups writes a week of hour rows into stats.db, as a
// server's hourly flushes leave them: every reason, every switching card and
// every sharing deployment, every hour.
func seedActivityRollups(t *testing.T, stats *statsdb.Store, deps []uint, cards []store.EventCard, end time.Time) int {
	t.Helper()
	var hours []statsdb.SchedulingHour
	rows := 0
	for h := 7 * 24; h > 24; h-- {
		hour := statsdb.SchedulingHour{HourMS: end.Add(-time.Duration(h) * time.Hour).Truncate(time.Hour).UnixMilli(), Reasons: map[string]int64{}}
		for i, r := range store.ActivityReasons() {
			hour.Reasons[string(r)] = int64(100 * (i + 1))
		}
		for i, c := range cards {
			hour.Cards = append(hour.Cards, statsdb.SchedulingCard{NodeUUID: c.NodeUUID, GPU: c.GPU, Switches: int64(10 + i%7)})
		}
		for _, dep := range deps {
			hour.Deployments = append(hour.Deployments, statsdb.SchedulingDeployment{DeploymentID: dep, AwakeSeconds: 1800, PresentSeconds: 3600, Yielded: 10, YieldedTo: 10, Wakes: 10})
		}
		rows += len(hour.Reasons) + len(hour.Cards) + len(hour.Deployments)
		hours = append(hours, hour)
	}
	if err := stats.ReplaceSchedulingHours(context.Background(), hours); err != nil {
		t.Fatal(err)
	}
	return rows
}

// The scheduling activity at the scale it is sized for: 1000 nodes, 3000+
// instances, 1000 deployments switching 10 times an hour, a day of raw
// events and a week of hour rows. The replay on start is bounded and timed;
// the 24 h and 7 d activity and the 24 h sharing stats answer within a
// budget, cached and not, and do not grow when the event log does: they
// never read it.
//
//	go test -count=1 -run TestSchedulingActivityAtFleetScale -v ./internal/server/api
func TestSchedulingActivityAtFleetScale(t *testing.T) {
	fleet := seedScaleFleet(t)
	st := fleet.rig.st
	db := st.DB()
	end := time.Now().UTC()

	// 300 more deployments that share cards, each with an instance, beside
	// the fleet's 700: 1000 switching deployments.
	extra := make([]store.Deployment, activityDeployments-scaleDeploys)
	for i := range extra {
		extra[i] = store.Deployment{Name: fmt.Sprintf("sw-%03d", i), ServedName: fmt.Sprintf("sw-%03d", i), ModelID: 1, Engine: "vllm", Sleepable: true, State: store.DeployActive, Replicas: 1}
		presetDeployment(&extra[i], yieldpolicy.Anytime)
	}
	if err := db.CreateInBatches(&extra, 200).Error; err != nil {
		t.Fatal(err)
	}
	var nodes []store.Node
	if err := db.Select("id", "uuid").Order("id").Find(&nodes).Error; err != nil {
		t.Fatal(err)
	}
	extraInsts := make([]store.Instance, len(extra))
	for i := range extra {
		extraInsts[i] = store.Instance{UUID: fmt.Sprintf("sw-inst-%03d", i), DeploymentID: extra[i].ID, NodeID: nodes[(i*13)%len(nodes)].ID,
			State: proto.InstRunning, GPUIndexes: fmt.Sprintf("[%d]", i%scaleCards), NNodes: 1, CreatedAt: end.Add(-48 * time.Hour)}
	}
	if err := db.CreateInBatches(&extraInsts, 500).Error; err != nil {
		t.Fatal(err)
	}
	var all []store.Instance
	if err := db.Select("uuid", "deployment_id", "node_id", "gp_uindexes").Order("id").Find(&all).Error; err != nil {
		t.Fatal(err)
	}
	firstInst := map[uint]string{}
	for _, in := range all {
		if _, ok := firstInst[in.DeploymentID]; !ok {
			firstInst[in.DeploymentID] = in.UUID
		}
	}
	var deps []uint
	var insts []string
	for id, uuid := range firstInst {
		deps = append(deps, id)
		insts = append(insts, uuid)
	}
	if len(deps) < activityDeployments {
		t.Fatalf("%d switching deployments, want %d", len(deps), activityDeployments)
	}
	nodeUUID := map[uint]string{}
	for _, n := range nodes {
		nodeUUID[n.ID] = n.UUID
	}
	var cards []store.EventCard
	seenCard := map[store.EventCard]bool{}
	for _, in := range all {
		if in.UUID == firstInst[in.DeploymentID] {
			var gpus []int
			_ = json.Unmarshal([]byte(in.GPUIndexes), &gpus)
			for _, g := range gpus {
				if c := (store.EventCard{NodeUUID: nodeUUID[in.NodeID], GPU: g}); !seenCard[c] {
					seenCard[c] = true
					cards = append(cards, c)
				}
			}
		}
	}

	seedStarted := time.Now()
	raw := seedActivityHistory(t, st, deps, insts, end, 25)
	stats, err := statsdb.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stats.Close() })
	rollups := seedActivityRollups(t, stats, deps, cards, end)
	t.Logf("seeded %d raw events (25 h) and %d hour rows (6 days) in %v", raw, rollups, time.Since(seedStarted).Round(time.Millisecond))

	retention := store.DefaultDataRetention(72*time.Hour, "UTC")
	svc := &schedactivity.Service{Tracker: schedactivity.NewTracker(), Store: st, Stats: stats,
		Retention: func() store.DataRetention { return retention }, Now: time.Now}
	replayBudget, uncachedBudget, cachedBudget := 60*time.Second, 2*time.Second, 200*time.Millisecond
	if raceEnabled {
		replayBudget, uncachedBudget, cachedBudget = 10*time.Minute, 20*time.Second, 2*time.Second
	}
	started := time.Now()
	read, _, err := svc.Replay(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	replay := time.Since(started)
	t.Logf("replay on start: %d events in %v (budget %v)", read, replay, replayBudget)
	if replay > replayBudget {
		t.Errorf("the replay took %v, budget %v", replay, replayBudget)
	}
	if read < raw*9/10 {
		t.Errorf("the replay read %d of %d events", read, raw)
	}

	fleet.deps.SchedulingActivity = svc
	fleet.rig.h = Router(fleet.deps)
	paths := []string{
		"/api/v1/scheduling/activity?window=24h",
		"/api/v1/scheduling/activity?window=7d",
		"/api/v1/scheduling/timeline?sharing=1&window=24h",
	}
	measure := func(label string) map[string]time.Duration {
		out := map[string]time.Duration{}
		var h *apiHandlers
		for _, path := range paths {
			// Uncached: a fresh handler set, the best of three.
			best := time.Hour
			for i := 0; i < 3; i++ {
				h = &apiHandlers{d: fleet.deps}
				started := time.Now()
				rec := callAuthed(fleet, h, path)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
				}
				best = min(best, time.Since(started))
			}
			out[path] = best
			// Cached: the router's handlers, p95 of 50.
			var samples []time.Duration
			for i := 0; i < 50; i++ {
				started := time.Now()
				if rec := fleet.get(path); rec.Code != http.StatusOK {
					t.Fatalf("%s = %d", path, rec.Code)
				}
				samples = append(samples, time.Since(started))
			}
			p95 := percentile(samples, 0.95)
			t.Logf("%s %s: uncached %v (budget %v), cached p95 %v (budget %v)", label, path, best, uncachedBudget, p95, cachedBudget)
			if best > uncachedBudget {
				t.Errorf("%s uncached took %v, budget %v", path, best, uncachedBudget)
			}
			if p95 > cachedBudget {
				t.Errorf("%s cached p95 = %v, budget %v", path, p95, cachedBudget)
			}
		}
		return out
	}
	before := measure("with a day of events:")

	var week dto.SchedulingActivity
	if body := plainBody(t, fleet, paths[1]); json.Unmarshal(body, &week) != nil || week.BucketSeconds != 3600 || len(week.TopCards) != 20 {
		t.Fatalf("7d = %d buckets of %ds, %d cards", len(week.Buckets), week.BucketSeconds, len(week.TopCards))
	}
	var day dto.SchedulingActivity
	if body := plainBody(t, fleet, paths[0]); json.Unmarshal(body, &day) != nil {
		t.Fatal("24h")
	}
	var yields int64
	for _, r := range day.Reasons[:3] {
		yields += r.Count
	}
	if want := int64(activityDeployments * activitySwitchesPerHour * 24); yields < want*95/100 || yields > want*105/100 {
		t.Fatalf("24h yields = %d, want about %d", yields, want)
	}
	var sharing dto.SchedulingTimeline
	if body := plainBody(t, fleet, paths[2]); json.Unmarshal(body, &sharing) != nil || len(sharing.Deployments) < activityDeployments/2 {
		t.Fatalf("24h sharing lists %d deployments", len(sharing.Deployments))
	}

	// The event log emptied: the answers are the same and no slower, for
	// they never read it, and so do not grow with it.
	if err := db.Exec(`DELETE FROM instance_events`).Error; err != nil {
		t.Fatal(err)
	}
	fleet.rig.h = Router(fleet.deps) // a new router: nothing cached
	after := measure("with the event log emptied:")
	for _, path := range paths {
		if after[path] > 2*before[path]+100*time.Millisecond {
			t.Errorf("%s went from %v to %v with the event log emptied", path, before[path], after[path])
		}
	}
	var again dto.SchedulingActivity
	if body := plainBody(t, fleet, paths[0]); json.Unmarshal(body, &again) != nil || fmt.Sprint(again.Reasons) != fmt.Sprint(day.Reasons) {
		t.Fatalf("24h reasons with the log emptied = %v, before %v", again.Reasons, day.Reasons)
	}
}

// insertEvents writes rows in multi-row inserts, faster than the ORM's
// batches for the hundreds of thousands a scale test seeds.
func insertEvents(tx *gorm.DB, rows []store.InstanceEvent) error {
	const batch = 500
	for start := 0; start < len(rows); start += batch {
		part := rows[start:min(start+batch, len(rows))]
		var q strings.Builder
		q.WriteString("INSERT INTO instance_events (instance_id, instance_uuid, deployment_id, kind, code, message, data, created_at) VALUES ")
		args := make([]any, 0, len(part)*6)
		for i, r := range part {
			if i > 0 {
				q.WriteString(",")
			}
			q.WriteString("(0, ?, ?, ?, ?, '', ?, ?)")
			args = append(args, r.InstanceUUID, r.DeploymentID, r.Kind, r.Code, r.Data, r.CreatedAt)
		}
		if err := tx.Exec(q.String(), args...).Error; err != nil {
			return err
		}
	}
	return nil
}

// plainBody is the router's answer to path, not compressed.
func plainBody(t *testing.T, f *scaleFleet, path string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+f.session)
	rec := httptest.NewRecorder()
	f.rig.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
	}
	return rec.Body.Bytes()
}

// callAuthed serves path on handlers of their own, which share no cache
// with the router's.
func callAuthed(_ *scaleFleet, h *apiHandlers, path string) *httptest.ResponseRecorder {
	if strings.HasPrefix(path, "/api/v1/scheduling/activity") {
		return callHandler(h.schedulingActivity, path)
	}
	return callHandler(h.schedulingTimeline, path)
}
