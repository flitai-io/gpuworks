package tsdb

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func gpuSample(node, gpu string, ts int64, v float64) Sample {
	return Sample{Metric: metricUtil, Labels: map[string]string{"node": node, "gpu": gpu}, Value: v, TimeMS: ts}
}

func TestQueryNodeFilterMatchesUnfilteredScan(t *testing.T) {
	s := openTemp(t)
	batch := []Sample{
		gpuSample("node-1", "0", 1000, 10),
		gpuSample("node-1", "1", 1000, 11),
		gpuSample("node-2", "0", 1000, 20),
		{Metric: metricUtil, Labels: map[string]string{"gpu": "0"}, Value: 30, TimeMS: 1000}, // no node label
	}
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("write: %v", err)
	}
	allRange, err := s.QueryRange(metricUtil, nil, 0, 10_000, 1000)
	if err != nil {
		t.Fatalf("unfiltered range: %v", err)
	}
	allLatest, err := s.QueryLatest(metricUtil, nil, 0)
	if err != nil {
		t.Fatalf("unfiltered latest: %v", err)
	}
	cases := []struct {
		name     string
		matchers map[string]string
		want     int
	}{
		{"node only", map[string]string{"node": "node-1"}, 2},
		{"node and gpu", map[string]string{"node": "node-1", "gpu": "1"}, 1},
		{"node with no match", map[string]string{"node": "node-9"}, 0},
		{"node and mismatched gpu", map[string]string{"node": "node-2", "gpu": "1"}, 0},
		// As with instance and deployment, an empty value means "label absent
		// or empty" and stays a Go-side check.
		{"empty node value matches the node-less series", map[string]string{"node": ""}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.QueryRange(metricUtil, tc.matchers, 0, 10_000, 1000)
			if err != nil {
				t.Fatalf("range: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("range returned %d series, want %d: %+v", len(got), tc.want, got)
			}
			assertSameSeries(t, got, manualFilter(allRange, tc.matchers))
			latest, err := s.QueryLatest(metricUtil, tc.matchers, 0)
			if err != nil {
				t.Fatalf("latest: %v", err)
			}
			want := 0
			for _, p := range allLatest {
				if matchesAll(p.Labels, tc.matchers) {
					want++
				}
			}
			if len(latest) != want || want != tc.want {
				t.Fatalf("latest returned %d points, want %d", len(latest), tc.want)
			}
		})
	}
}

func TestNodeFilterPlanUsesNodeIndex(t *testing.T) {
	s := openTemp(t).(*sqliteStore)
	clause, extraArgs, rest := indexedMatcherClause("sr.", map[string]string{"node": "node-1", "gpu": "0"})
	if !strings.Contains(clause, "sr.node = ?") || rest["gpu"] != "0" || len(rest) != 1 {
		t.Fatalf("clause %q rest %v: node should be pushed down and gpu kept for Go", clause, rest)
	}
	for name, query := range map[string]string{
		"range":  fmt.Sprintf(queryRangeJoinSQL, clause),
		"latest": queryLatestBaseSQL + clause + queryLatestOrderSQL,
	} {
		args := append([]any{int64(0), int64(10_000), metricUtil}, extraArgs...)
		plan := explainPlan(t, s.db, query, args...)
		if !strings.Contains(plan, "idx_series_metric_node") {
			t.Fatalf("%s with a node matcher must seek series by (metric, node), plan:\n%s", name, plan)
		}
	}
}

func TestMigrationThreeBackfillsNodeAndRollsBackOnConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	db := openRawSQLite(t, path)
	if err := migrateSchema(db, schemaMigrations[:2]); err != nil {
		t.Fatalf("migrations 1-2: %v", err)
	}
	if columnNames(t, db, "series")["node"] {
		t.Fatal("node column present before migration 3")
	}
	for key, labels := range map[string]string{
		"with-node": `{"node":"n1","gpu":"0"}`,
		"no-node":   `{"instance":"inst-1"}`,
	} {
		if _, err := db.Exec(`INSERT INTO series(metric, labels_key, labels_json) VALUES(?, ?, ?)`, metricUtil, key, labels); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	// An object already holding the index name fails the migration after its
	// ALTER TABLE and backfill; neither may survive.
	if _, err := db.Exec(`CREATE TABLE idx_series_metric_node(x INTEGER)`); err != nil {
		t.Fatalf("plant conflict: %v", err)
	}
	if err := migrateSchema(db, schemaMigrations); err == nil {
		t.Fatal("migration 3 succeeded despite the conflicting object")
	}
	if columnNames(t, db, "series")["node"] || schemaVersionMarkerExists(t, db, 3) {
		t.Fatal("failed migration 3 left its column or marker behind")
	}

	if _, err := db.Exec(`DROP TABLE idx_series_metric_node`); err != nil {
		t.Fatalf("drop conflict: %v", err)
	}
	if err := migrateSchema(db, schemaMigrations); err != nil {
		t.Fatalf("migration 3 after fixing the conflict: %v", err)
	}
	if !schemaVersionMarkerExists(t, db, 3) {
		t.Fatal("missing schema_version marker 3")
	}
	var node sql.NullString
	if err := db.QueryRow(`SELECT node FROM series WHERE labels_key = 'with-node'`).Scan(&node); err != nil || node.String != "n1" {
		t.Fatalf("with-node backfilled as %+v (%v), want n1", node, err)
	}
	if err := db.QueryRow(`SELECT node FROM series WHERE labels_key = 'no-node'`).Scan(&node); err != nil || node.Valid {
		t.Fatalf("no-node backfilled as %+v (%v), want NULL", node, err)
	}
	if err := migrateSchema(db, schemaMigrations); err != nil {
		t.Fatalf("re-run: %v", err)
	}
}

// A series row an older binary wrote has no node column value; the first
// write of that label set in a process fills it in place.
func TestWriteBatchSelfHealsNullNodeColumn(t *testing.T) {
	s := openTemp(t).(*sqliteStore)
	sm := gpuSample("node-1", "0", 1000, 1)
	key := labelsKey(sm.Metric, sm.Labels)
	lj, _ := json.Marshal(sm.Labels)
	if _, err := s.db.Exec(`INSERT INTO series(metric, labels_key, labels_json) VALUES(?, ?, ?)`, sm.Metric, key, string(lj)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.WriteBatch([]Sample{sm}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.QueryRange(metricUtil, map[string]string{"node": "node-1"}, 0, 10_000, 1000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || len(got[0].Points) != 1 {
		t.Fatalf("node-1 after heal = %+v, want one series with one point", got)
	}
}

// Retain's sweep deletes series rows and SQLite may give a deleted id to the
// next new series, so a remembered id must not outlive the sweep.
func TestWriteAfterSweepDoesNotReuseSweptSeriesID(t *testing.T) {
	s := openTemp(t)
	a := gpuSample("node-a", "0", 1000, 1)
	if err := s.WriteBatch([]Sample{a}); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := s.Retain(t.Context(), 2000); err != nil {
		t.Fatalf("retain: %v", err)
	}
	if err := s.WriteBatch([]Sample{gpuSample("node-b", "0", 3000, 2)}); err != nil {
		t.Fatalf("write b: %v", err)
	}
	a.TimeMS, a.Value = 4000, 3
	if err := s.WriteBatch([]Sample{a}); err != nil {
		t.Fatalf("rewrite a: %v", err)
	}
	assertNodePoints(t, s, "node-a", []Point{{TimeMS: 4000, Value: 3}})
	assertNodePoints(t, s, "node-b", []Point{{TimeMS: 3000, Value: 2}})
}

// A batch that fails after creating a series rolls the row back; its id must
// not be remembered, or the next new series (which may get that id) would
// receive the first series' samples.
func TestFailedBatchDoesNotRememberRolledBackSeries(t *testing.T) {
	s := openTemp(t)
	// The trigger aborts the batch on its second sample, after node-a's
	// series row was created in the same transaction.
	if _, err := s.(*sqliteStore).db.Exec(`CREATE TRIGGER fail_batch BEFORE INSERT ON samples
		WHEN NEW.value = -1 BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if err := s.WriteBatch([]Sample{gpuSample("node-a", "0", 1000, 1), gpuSample("node-x", "0", 1000, -1)}); err == nil {
		t.Fatal("the injected failure did not fail the batch")
	}
	if err := s.WriteBatch([]Sample{gpuSample("node-b", "0", 2000, 2)}); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if err := s.WriteBatch([]Sample{gpuSample("node-a", "0", 3000, 3)}); err != nil {
		t.Fatalf("write a: %v", err)
	}
	assertNodePoints(t, s, "node-a", []Point{{TimeMS: 3000, Value: 3}})
	assertNodePoints(t, s, "node-b", []Point{{TimeMS: 2000, Value: 2}})
}

func TestSeriesIDCacheBoundKeepsWritesCorrect(t *testing.T) {
	old := seriesIDCacheMax
	seriesIDCacheMax = 2
	t.Cleanup(func() { seriesIDCacheMax = old })
	s := openTemp(t)
	for round := int64(0); round < 3; round++ {
		var batch []Sample
		for n := 0; n < 5; n++ {
			batch = append(batch, gpuSample(fmt.Sprintf("node-%d", n), "0", 1000*(round+1), float64(round)))
		}
		if err := s.WriteBatch(batch); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if n := len(s.(*sqliteStore).ids); n > 2 {
		t.Fatalf("cache holds %d ids, bound is 2", n)
	}
	for n := 0; n < 5; n++ {
		assertNodePoints(t, s, fmt.Sprintf("node-%d", n), []Point{{1000, 0}, {2000, 1}, {3000, 2}})
	}
}

func TestConcurrentWriteBatchesShareSeries(t *testing.T) {
	s := openTemp(t)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				ts := int64(w*1000 + i)
				if err := s.WriteBatch([]Sample{gpuSample("node-1", "0", ts, 1), gpuSample("node-2", "0", ts, 2)}); err != nil {
					t.Errorf("writer %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	var count int
	if err := s.(*sqliteStore).db.QueryRow(`SELECT COUNT(*) FROM series`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("series rows = %d (%v), want 2", count, err)
	}
	for _, node := range []string{"node-1", "node-2"} {
		got, err := s.QueryRange(metricUtil, map[string]string{"node": node}, 0, 10_000, 1)
		if err != nil || len(got) != 1 || len(got[0].Points) != 80 {
			t.Fatalf("%s = %d series (%v), want one series with 80 points", node, len(got), err)
		}
	}
}

func TestWriteBatchSkipsNonFiniteValues(t *testing.T) {
	s := openTemp(t)
	batch := []Sample{
		gpuSample("node-1", "0", 1000, 1),
		gpuSample("node-1", "0", 2000, math.NaN()),
		gpuSample("node-1", "0", 3000, math.Inf(1)),
		gpuSample("node-1", "0", 4000, math.Inf(-1)),
		gpuSample("node-2", "0", 1000, math.NaN()), // only non-finite: no series row
		gpuSample("node-1", "0", 5000, 5),
	}
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("a batch with non-finite values failed: %v", err)
	}
	assertNodePoints(t, s, "node-1", []Point{{1000, 1}, {5000, 5}})
	got, err := s.QueryRange(metricUtil, nil, 0, 10_000, 1000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("series = %+v, want only node-1", got)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("answer does not encode as JSON: %v", err)
	}
}

func assertNodePoints(t *testing.T, s Store, node string, want []Point) {
	t.Helper()
	got, err := s.QueryRange(metricUtil, map[string]string{"node": node}, 0, 10_000, 1000)
	if err != nil {
		t.Fatalf("query %s: %v", node, err)
	}
	if len(got) != 1 {
		t.Fatalf("%s has %d series, want 1: %+v", node, len(got), got)
	}
	if fmt.Sprint(got[0].Points) != fmt.Sprint(want) {
		t.Fatalf("%s points = %v, want %v", node, got[0].Points, want)
	}
}
