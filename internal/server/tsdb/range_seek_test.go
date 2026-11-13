package tsdb

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// withSeekThreshold runs fn with the seek path forced on (threshold 1) or off
// (threshold above any step used here).
func withSeekThreshold(t *testing.T, threshold int64, fn func()) {
	t.Helper()
	old := seekBucketsMinStepMS
	seekBucketsMinStepMS = threshold
	defer func() { seekBucketsMinStepMS = old }()
	fn()
}

// The seek path must answer exactly what the scan path answers: same series
// in the same order, same bucket starts, same last values, empty series kept.
func TestQueryRangeSeekMatchesScan(t *testing.T) {
	s := openTemp(t)
	rng := rand.New(rand.NewSource(7))
	var batch []Sample
	for n := 0; n < 3; n++ {
		for g := 0; g < 2; g++ {
			labels := map[string]string{"node": fmt.Sprintf("node-%d", n), "gpu": fmt.Sprint(g)}
			if n == 2 {
				labels["instance"] = "inst-1"
				labels["deployment"] = "7"
			}
			// Irregular timestamps with gaps longer than a bucket.
			for ts := int64(1_000 + rng.Intn(5_000)); ts < 600_000; ts += int64(1 + rng.Intn(40_000)) {
				batch = append(batch, Sample{Metric: metricUtil, Labels: labels, Value: float64(rng.Intn(1000)), TimeMS: ts})
			}
		}
	}
	// A series whose only sample is outside every window queried below.
	batch = append(batch, Sample{Metric: metricUtil, Labels: map[string]string{"node": "node-9", "gpu": "0"}, Value: 1, TimeMS: 900_000})
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("write: %v", err)
	}

	windows := [][2]int64{{0, 600_000}, {12_345, 456_789}, {300_000, 300_000}, {599_999, 700_000}, {500, 400}}
	// 3_000 is finer than most sample gaps (many empty buckets); 1_000_000 is
	// wider than every window (one bucket).
	steps := []int64{3_000, 7_000, 30_000, 60_000, 123_457, 1_000_000}
	matcherSets := []map[string]string{
		nil,
		{"node": "node-1"},
		{"node": "node-1", "gpu": "1"},
		{"instance": "inst-1"},
		{"deployment": "7", "gpu": "0"},
		{"node": ""},
		{"node": "missing"},
	}
	for _, w := range windows {
		for _, step := range steps {
			for _, m := range matcherSets {
				var scan, seek []Series
				var scanErr, seekErr error
				withSeekThreshold(t, 1<<62, func() { scan, scanErr = s.QueryRange(metricUtil, m, w[0], w[1], step) })
				withSeekThreshold(t, 1, func() { seek, seekErr = s.QueryRange(metricUtil, m, w[0], w[1], step) })
				if scanErr != nil || seekErr != nil {
					t.Fatalf("window %v step %d matchers %v: scan err %v, seek err %v", w, step, m, scanErr, seekErr)
				}
				if !reflect.DeepEqual(scan, seek) {
					t.Fatalf("window %v step %d matchers %v:\nscan %+v\nseek %+v", w, step, m, scan, seek)
				}
			}
		}
	}
}

func TestQueryRangeSeekEmptyResultMarshalsAsEmptyArrays(t *testing.T) {
	s := openTemp(t)
	if err := s.WriteBatch([]Sample{sample("0", 1000, 1)}); err != nil {
		t.Fatalf("write: %v", err)
	}
	for name, matchers := range map[string]map[string]string{"no series": {"gpu": "9"}, "no samples in window": nil} {
		got, err := s.QueryRange(metricUtil, matchers, 100_000, 200_000, seekBucketsMinStepMS)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if strings.Contains(string(b), "null") {
			t.Fatalf("%s: %s, want [] rather than null", name, b)
		}
	}
}

func TestQueryRangeSeekPlanSeeksSamplesByKey(t *testing.T) {
	s := openTemp(t).(*sqliteStore)
	plan := explainPlan(t, s.db, querySeekBucketsSQL, int64(0), int64(60_000), int64(60_000), int64(600_000),
		int64(0), int64(60_000), int64(600_000), "[1,2,3]")
	if !strings.Contains(plan, "SEARCH sm") || strings.Contains(plan, "SCAN sm") {
		t.Fatalf("each bucket must seek samples by (series_id, ts_ms), plan:\n%s", plan)
	}
}
