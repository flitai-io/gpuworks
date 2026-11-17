package uxstore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func shaped(id string, dep uint, ts int64, prompt, completion, cached, firstAnswer int32) uxrecord.Request {
	r := okReq(id, dep, ts)
	r.PromptTokens, r.CompletionTokens, r.CachedTokens, r.FirstAnswerMS = prompt, completion, cached, firstAnswer
	return r
}

func TestGridAccumulatesAcrossFlushesAndBuckets(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c))
	b1 := ms(day0.Add(-time.Hour)) // 11:00 bucket
	b2 := b1 + 5*60*1000           // 11:05 bucket
	ctx := context.Background()

	// Deployment 1, bucket 1: two requests in the same io cell, one per flush.
	s.Observe(shaped("a", 1, b1+10, 1000, 100, 600, 300)) // io (9,6) ut (8, TTFT 300ms)
	mustSync(t, s, false)
	s.Observe(shaped("b", 1, b1+20, 800, 120, uxrecord.Unknown, 300)) // io (9,6), no ut
	mustSync(t, s, false)
	// Deployment 1, bucket 2; deployment 2, bucket 1; one without usage.
	s.Observe(shaped("c", 1, b2+1, 4, 2, 0, 50))        // io (2,1) ut (2,0)
	s.Observe(shaped("d", 2, b1+30, 1000, 100, 0, 300)) // io (9,6) ut (9,TTFT)
	noUsage := okReq("e", 1, b1+40)
	noUsage.Usage = false
	s.Observe(noUsage)
	mustSync(t, s, false)

	ttft300 := uxrecord.TTFTBucket(300)
	cases := []struct {
		q    GridQuery
		want []uxrecord.Cell
	}{
		{GridQuery{DeploymentID: 1, Kind: uxrecord.GridIO}, []uxrecord.Cell{{X: 2, Y: 1, N: 1}, {X: 9, Y: 6, N: 2}}},
		{GridQuery{DeploymentID: 1, Kind: uxrecord.GridUncachedTTFT}, []uxrecord.Cell{{X: 2, Y: 0, N: 1}, {X: 8, Y: ttft300, N: 1}}},
		// Deployment 0 sums every deployment.
		{GridQuery{Kind: uxrecord.GridIO}, []uxrecord.Cell{{X: 2, Y: 1, N: 1}, {X: 9, Y: 6, N: 3}}},
		{GridQuery{Kind: uxrecord.GridUncachedTTFT}, []uxrecord.Cell{{X: 2, Y: 0, N: 1}, {X: 8, Y: ttft300, N: 1}, {X: 9, Y: ttft300, N: 1}}},
		// FromMS inside bucket 2 is floored to it: bucket 1 is out.
		{GridQuery{DeploymentID: 1, Kind: uxrecord.GridIO, FromMS: b2 + 60_000}, []uxrecord.Cell{{X: 2, Y: 1, N: 1}}},
		// ToMS before bucket 2 starts: only bucket 1.
		{GridQuery{Kind: uxrecord.GridIO, ToMS: b2 - 1}, []uxrecord.Cell{{X: 9, Y: 6, N: 3}}},
		{GridQuery{DeploymentID: 3, Kind: uxrecord.GridIO}, []uxrecord.Cell{}},
	}
	for _, tc := range cases {
		got, err := s.Grid(ctx, tc.q)
		if err != nil {
			t.Fatalf("Grid(%+v): %v", tc.q, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Grid(%+v) = %v, want %v", tc.q, got, tc.want)
		}
	}

	for _, tc := range []struct {
		dep      uint
		from, to int64
		want     CoverageStats
	}{
		{1, 0, 0, CoverageStats{Total: 4, WithUsage: 3, WithCache: 2}},
		{0, 0, 0, CoverageStats{Total: 5, WithUsage: 4, WithCache: 3}},
		{1, b2, 0, CoverageStats{Total: 1, WithUsage: 1, WithCache: 1}},
		{2, b2, 0, CoverageStats{}},
	} {
		got, err := s.Coverage(ctx, tc.dep, tc.from, tc.to)
		if err != nil || got != tc.want {
			t.Errorf("Coverage(%d, %d, %d) = %+v, %v; want %+v", tc.dep, tc.from, tc.to, got, err, tc.want)
		}
	}

	byDep, err := s.RequestsByDeployment(ctx, 0, 0)
	if err != nil || !reflect.DeepEqual(byDep, map[uint]int64{1: 4, 2: 1}) {
		t.Fatalf("RequestsByDeployment = %v, %v; want every request per deployment", byDep, err)
	}
	if byDep, err := s.RequestsByDeployment(ctx, b2, 0); err != nil || !reflect.DeepEqual(byDep, map[uint]int64{1: 1}) {
		t.Fatalf("RequestsByDeployment from bucket 2 = %v, %v", byDep, err)
	}

	// The stored row is one per cell: the second flush added to the first.
	var n int64
	if err := s.reader.QueryRow(`SELECT n FROM grid_20261012 WHERE bucket_ms=? AND deployment_id=1 AND kind='io' AND x=9 AND y=6`, b1).Scan(&n); err != nil || n != 2 {
		t.Fatalf("stored n = %d (%v), want 2", n, err)
	}
}

func TestGridSpansDays(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c))
	for d := range 3 {
		s.Observe(shaped(fmt.Sprint(d), 1, ms(day0.AddDate(0, 0, -d)), 1000, 100, 0, 300))
	}
	mustSync(t, s, false)
	got, err := s.Grid(context.Background(), GridQuery{DeploymentID: 1, Kind: uxrecord.GridIO, FromMS: ms(day0.AddDate(0, 0, -1).Add(-time.Minute))})
	if err != nil || len(got) != 1 || got[0].N != 2 {
		t.Fatalf("two-day grid = %v, %v", got, err)
	}
}

func TestBadQueries(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c))
	ctx := context.Background()
	if _, _, err := s.Requests(ctx, RequestQuery{}); !errors.Is(err, ErrBadQuery) {
		t.Errorf("missing deployment: %v", err)
	}
	for _, cur := range []string{"!!", "bm90LWEtY3Vyc29y", cursor{ts: ms(day0), day: "20200101", seq: 1}.encode()} {
		if _, _, err := s.Requests(ctx, RequestQuery{DeploymentID: 1, Cursor: cur}); !errors.Is(err, ErrBadQuery) {
			t.Errorf("cursor %q: %v", cur, err)
		}
	}
	if _, err := s.Grid(ctx, GridQuery{Kind: "tpot"}); !errors.Is(err, ErrBadQuery) {
		t.Errorf("bad kind: %v", err)
	}
	if got, next, err := s.Requests(ctx, RequestQuery{DeploymentID: 1}); err != nil || len(got) != 0 || next != "" {
		t.Errorf("empty store: %v %q %v", got, next, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.Requests(cancelled, RequestQuery{DeploymentID: 1}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// The page and grid queries are served by their indexes without a sort.
func TestQueryPlans(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c))
	s.Observe(okReq("a", 1, ms(day0)))
	mustSync(t, s, false)
	plan := func(query string, args []any) string {
		rows, err := s.reader.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatalf("explain %s: %v", query, err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			b.WriteString(detail + "\n")
		}
		return b.String()
	}
	cur := &cursor{ts: ms(day0), day: "20261012", seq: 5}
	for _, q := range []RequestQuery{
		{DeploymentID: 1},
		{DeploymentID: 1, FromMS: 1, ToMS: ms(day0)},
		{DeploymentID: 1, APIKeyID: 3},
		{DeploymentID: 1, Results: []uxrecord.Result{uxrecord.ResultLength, uxrecord.ResultError}, MinEndMS: 1000, InstanceUUID: "i"},
		{DeploymentID: 1, MinTTFTMS: 500, MaxTTFTMS: 2000, MaxEndMS: 9000},
	} {
		p := plan(requestSQL("20261012", q, cur, 51))
		if strings.Contains(p, "TEMP B-TREE") || !strings.Contains(p, "USING INDEX req_20261012_") {
			t.Errorf("request plan for %+v:\n%s", q, p)
		}
	}
	where, args := bucketRange(1, 1, 0)
	p := plan(`SELECT x, y, SUM(n) FROM grid_20261012 WHERE `+strings.Join(where, " AND ")+` AND kind = ? GROUP BY x, y`, append(args, "io"))
	if !strings.Contains(p, "grid_20261012_dep") {
		t.Errorf("per-deployment grid plan does not use the deployment index:\n%s", p)
	}
}
