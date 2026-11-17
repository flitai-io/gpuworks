package statsdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/rollup"
)

func schedulingHour(hour int64, idle, cardSwitches int64) SchedulingHour {
	return SchedulingHour{
		HourMS:  hour,
		Reasons: map[string]int64{"idle_yield": idle, "failure": 1, "queued": 0},
		Cards:   []SchedulingCard{{NodeUUID: "n1", GPU: 0, Switches: cardSwitches}, {NodeUUID: "n2", GPU: 3, Switches: 1}},
		Deployments: []SchedulingDeployment{
			{DeploymentID: 7, AwakeSeconds: 1800, PresentSeconds: 3600, Yielded: idle, YieldedTo: 1, Wakes: 2},
		},
	}
}

// Hours are written whole: writing an hour again replaces its rows, so a
// restart that counted it again from the event log leaves the same figures.
// Days roll up from hours and are retained with them.
func TestSchedulingHoursReplaceRollUpAndRetain(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	h0, h1 := hourMS(20, 0), hourMS(20, 1)
	if err := s.ReplaceSchedulingHours(ctx, []SchedulingHour{schedulingHour(h0, 4, 9), schedulingHour(h1, 2, 1)}); err != nil {
		t.Fatal(err)
	}
	// h0 again, with other figures and without the second card.
	again := schedulingHour(h0, 5, 3)
	again.Cards = again.Cards[:1]
	if err := s.ReplaceSchedulingHours(ctx, []SchedulingHour{again}); err != nil {
		t.Fatal(err)
	}
	hours := Range{From: h0, To: h1 + hourSpanMS}
	reasons, err := s.SchedulingReasons(ctx, hours)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(reasons, func(i, j int) bool {
		if reasons[i].Bucket != reasons[j].Bucket {
			return reasons[i].Bucket < reasons[j].Bucket
		}
		return reasons[i].Reason < reasons[j].Reason
	})
	want := []SchedulingReasonRow{{h0, "failure", 1}, {h0, "idle_yield", 5}, {h1, "failure", 1}, {h1, "idle_yield", 2}}
	if len(reasons) != len(want) {
		t.Fatalf("reasons = %+v (a zero count is not written)", reasons)
	}
	for i := range want {
		if reasons[i] != want[i] {
			t.Fatalf("reason %d = %+v, want %+v", i, reasons[i], want[i])
		}
	}
	cards, err := s.SchedulingCards(ctx, hours)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].NodeUUID < cards[j].NodeUUID })
	if len(cards) != 2 || cards[0] != (SchedulingCard{"n1", 0, 4}) || cards[1] != (SchedulingCard{"n2", 3, 1}) {
		t.Fatalf("cards = %+v", cards)
	}
	deps, err := s.SchedulingDeployments(ctx, hours)
	if err != nil || len(deps) != 1 || deps[0] != (SchedulingDeployment{7, 3600, 7200, 7, 2, 4}) {
		t.Fatalf("deployments = %+v %v", deps, err)
	}

	day := rollup.DayOf(time.UnixMilli(h0), time.UTC)
	if err := s.RollupDay(ctx, day, time.UTC); err != nil {
		t.Fatal(err)
	}
	days := Range{Daily: true, From: int64(day), To: int64(day.Next())}
	if cards, err := s.SchedulingCards(ctx, days); err != nil || len(cards) != 2 {
		t.Fatalf("day cards = %+v %v", cards, err)
	}
	if reasons, err := s.SchedulingReasons(ctx, days); err != nil || len(reasons) != 2 {
		t.Fatalf("day reasons = %+v %v", reasons, err)
	}
	if deps, err := s.SchedulingDeployments(ctx, days); err != nil || len(deps) != 1 || deps[0].AwakeSeconds != 3600 {
		t.Fatalf("day deployments = %+v %v", deps, err)
	}
	hourly, daily, err := s.RowCount(ctx, KindScheduling)
	if err != nil || hourly != 4+3+2 || daily != 2+2+1 {
		t.Fatalf("rows = %d %d %v", hourly, daily, err)
	}
	if _, err := s.Retain(ctx, h1+hourSpanMS, day.Next()); err != nil {
		t.Fatal(err)
	}
	if hourly, daily, err := s.RowCount(ctx, KindScheduling); err != nil || hourly != 0 || daily != 0 {
		t.Fatalf("after retain = %d %d %v", hourly, daily, err)
	}
}

// A week's top cards and reasons read a range of the primary key, never the
// whole table.
func TestSchedulingQueriesUseThePrimaryKey(t *testing.T) {
	s, _ := openTest(t)
	for _, q := range []string{
		`SELECT hour_ms, reason, events FROM sched_reason_hourly WHERE hour_ms >= 0 AND hour_ms < 1`,
		`SELECT node_uuid, gpu_index, SUM(switches) FROM sched_card_hourly WHERE hour_ms >= 0 AND hour_ms < 1 GROUP BY node_uuid, gpu_index`,
		`SELECT node_uuid, gpu_index, SUM(switches) FROM sched_card_daily WHERE day >= 0 AND day < 1 GROUP BY node_uuid, gpu_index`,
		`SELECT deployment_id, SUM(awake_seconds) FROM sched_deployment_hourly WHERE hour_ms >= 0 AND hour_ms < 1 GROUP BY deployment_id`,
	} {
		plan := queryPlan(t, s.reader, q)
		if !strings.Contains(plan, "sqlite_autoindex") || !strings.Contains(plan, "hour_ms>? AND hour_ms<?") && !strings.Contains(plan, "day>? AND day<?") {
			t.Fatalf("%s\nplan: %s", q, plan)
		}
	}
}

func queryPlan(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN ` + q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	return strings.Join(details, "\n")
}

// writeV2 makes a stats.db as the second schema left it, holding a GPU hour;
// extra statements run after.
func writeV2(t *testing.T, extra ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := append(append(append([]string{}, schemaV1...), schemaV2...),
		`INSERT INTO gpu_hourly VALUES (1000, 'n1', 0, 'GPU-a', 'H100', 1.5, 1, 1.5, 2, 1, 2, 80, 300, 1, 60, 1, 60)`,
		`PRAGMA user_version = 2`)
	for _, stmt := range append(stmts, extra...) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return path
}

// Opening a second-schema file adds the scheduling tables and the version
// marker, keeps its rows, and a second open changes nothing.
func TestSchemaV3AddsTheSchedulingTables(t *testing.T) {
	path := writeV2(t)
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var version, tables, gpu int
		if err := s.reader.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
			t.Fatalf("version = %d %v", version, err)
		}
		if err := s.reader.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name LIKE 'sched_%'`).Scan(&tables); err != nil || tables != 6 {
			t.Fatalf("scheduling tables = %d %v", tables, err)
		}
		if err := s.reader.QueryRow(`SELECT COUNT(*) FROM gpu_hourly`).Scan(&gpu); err != nil || gpu != 1 {
			t.Fatalf("gpu rows = %d %v", gpu, err)
		}
		if err := s.ReplaceSchedulingHours(context.Background(), []SchedulingHour{schedulingHour(hourMS(20, 0), 1, 1)}); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}

// A step that fails halfway leaves the second schema whole: the tables it
// made and the version roll back together, and it runs on the next start.
func TestSchemaV3FailingHalfwayLeavesTheSecondSchema(t *testing.T) {
	path := writeV2(t, `CREATE TABLE sched_card_hourly (x INTEGER)`)
	if _, err := Open(path); err == nil {
		t.Fatal("the step succeeded over a table in its way")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, made int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatalf("version = %d %v", version, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('sched_reason_hourly', 'sched_reason_daily')`).Scan(&made); err != nil || made != 0 {
		t.Fatalf("tables left by the failed step = %d %v", made, err)
	}
	if _, err := db.Exec(`DROP TABLE sched_card_hourly`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
}
